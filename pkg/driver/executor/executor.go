// SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/gardener/machine-controller-manager-provider-openstack/pkg/apis/cloudprovider"
	api "github.com/gardener/machine-controller-manager-provider-openstack/pkg/apis/openstack"
	"github.com/gardener/machine-controller-manager-provider-openstack/pkg/client"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/schedulerhints"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"k8s.io/utils/pointer"
)

// Executor concretely handles the execution of requests to the machine controller. Executor is responsible
// for communicating with OpenStack services and orchestrates the operations.
type Executor struct {
	Compute client.Compute
	Network client.Network
	Config  *api.MachineProviderConfig
}

// NewExecutor returns a new instance of Executor.
func NewExecutor(factory *client.Factory, config *api.MachineProviderConfig) (*Executor, error) {
	computeClient, err := factory.Compute(client.WithRegion(config.Spec.Region))
	if err != nil {
		klog.Errorf("failed to create compute client for executor: %v", err)
		return nil, err
	}
	networkClient, err := factory.Network(client.WithRegion(config.Spec.Region))
	if err != nil {
		klog.Errorf("failed to create network client for executor: %v", err)
		return nil, err
	}

	ex := &Executor{
		Compute: computeClient,
		Network: networkClient,
		Config:  config,
	}
	return ex, nil
}

// CreateMachine creates a new OpenStack server instance and waits until it reports "ACTIVE".
// If there is an error during the build process, or if the building phase timeouts, it will delete any artifacts created.
func (ex *Executor) CreateMachine(ctx context.Context, machineName string, userData []byte) (string, error) {
	serverNetworks, err := ex.computeServerNetworks(machineName)
	if err != nil {
		return "", fmt.Errorf("failed to resolve server networks: %w", err)
	}

	server, err := ex.deployServer(machineName, userData, serverNetworks)
	if err != nil {
		return "", fmt.Errorf("failed to deploy server for machine %q: %w", machineName, err)
	}

	providerID := EncodeProviderID(ex.Config.Spec.Region, server.ID)

	// if we fail in the creation post-processing step we have to delete the server we created
	deleteOnFail := func(err error) error {
		klog.Infof("attempting to delete server [ID=%q] after unsuccessful create operation with error: %v", server.ID, err)
		if errIn := ex.DeleteMachine(ctx, machineName, providerID); errIn != nil {
			return fmt.Errorf("error deleting server [ID=%q] after unsuccessful creation attempt: %w. Original error: %v", server.ID, errIn, err)
		}
		return err
	}

	err = ex.waitForStatus(server.ID, []string{client.ServerStatusBuild}, []string{client.ServerStatusActive}, 600)
	if err != nil {
		return "", deleteOnFail(fmt.Errorf("error waiting for server [ID=%q] to reach target status: %w", server.ID, err))
	}

	if err := ex.patchServerPortsForPodNetwork(server.ID); err != nil {
		return "", deleteOnFail(fmt.Errorf("failed to patch server [ID=%q] ports: %s", server.ID, err))
	}
	return providerID, nil
}

func (ex *Executor) computeServerNetworks(machineName string) ([]servers.Network, error) {
	var (
		networkID      = ex.Config.Spec.NetworkID
		subnetID       = ex.Config.Spec.SubnetID
		networks       = ex.Config.Spec.Networks
		serverNetworks = make([]servers.Network, 0)
	)

	klog.V(3).Infof("resolving network setup for machine %q", machineName)
	// If NetworkID is specified in the spec, we deploy the VMs in an existing Network.
	// If SubnetID is specified in addition to NetworkID, we have to preallocate a Neutron Port to force the VMs to get IP from the subnet's range.
	if !isEmptyString(pointer.StringPtr(networkID)) && !isEmptyString(subnetID) {
		klog.V(3).Infof("deploying in existing network [ID=%q]", networkID)
		klog.V(3).Infof("deploying in existing subnet [ID=%q]. Pre-allocating Neutron Port... ", *subnetID)
		if _, err := ex.Network.GetSubnet(*subnetID); err != nil {
			return nil, err
		}

		var securityGroupIDs []string
		for _, securityGroup := range ex.Config.Spec.SecurityGroups {
			securityGroupID, err := ex.Network.GroupIDFromName(securityGroup)
			if err != nil {
				return nil, err
			}
			securityGroupIDs = append(securityGroupIDs, securityGroupID)
		}

		port, err := ex.Network.CreatePort(&ports.CreateOpts{
			Name:                machineName,
			NetworkID:           ex.Config.Spec.NetworkID,
			FixedIPs:            []ports.IP{{SubnetID: *ex.Config.Spec.SubnetID}},
			AllowedAddressPairs: []ports.AddressPair{{IPAddress: ex.Config.Spec.PodNetworkCidr}},
			SecurityGroups:      &securityGroupIDs,
		})
		if err != nil {
			return nil, err
		}
		klog.V(3).Infof("port [ID=%q] successfully created", port.ID)
		serverNetworks = append(serverNetworks, servers.Network{UUID: ex.Config.Spec.NetworkID, Port: port.ID})

		return serverNetworks, nil
	} else if !isEmptyString(pointer.StringPtr(networkID)) {
		// if no SubnetID is specified, use only the NetworkID for the network attachments.
		serverNetworks = append(serverNetworks, servers.Network{UUID: ex.Config.Spec.NetworkID})

		return serverNetworks, nil
	}

	for _, network := range networks {
		var (
			resolvedNetworkID string
			err               error
		)
		if isEmptyString(pointer.StringPtr(network.Id)) {
			resolvedNetworkID, err = ex.Network.NetworkIDFromName(network.Name)
			if err != nil {
				return nil, err
			}
		} else {
			resolvedNetworkID = network.Id
		}
		serverNetworks = append(serverNetworks, servers.Network{UUID: resolvedNetworkID})
	}

	return serverNetworks, nil

}

func (ex *Executor) computePodNetworkIDs(serverID string) ([]ports.Port, error) {
	var (
		networkID  = ex.Config.Spec.NetworkID
		networks   = ex.Config.Spec.Networks
		networkIDs = sets.NewString()
		result     []ports.Port
	)

	if !isEmptyString(pointer.StringPtr(networkID)) {
		networkIDs.Insert(networkID)
	} else {
		for _, network := range networks {
			if network.PodNetwork {
				var (
					resolvedNetworkID string
					err               error
				)

				if isEmptyString(pointer.StringPtr(network.Id)) {
					resolvedNetworkID, err = ex.Network.NetworkIDFromName(network.Name)
					if err != nil {
						return nil, err
					}
				} else {
					resolvedNetworkID = network.Id
				}
				networkIDs.Insert(resolvedNetworkID)
			}
		}
	}

	serverPorts, err := ex.Network.ListPorts(&ports.ListOpts{
		DeviceID: serverID,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to get ports: %v", err)
	}

	if len(serverPorts) == 0 {
		return nil, fmt.Errorf("got an empty port list for server %q", serverID)
	}

	for _, port := range serverPorts {
		if networkIDs.Has(port.NetworkID) {
			result = append(result, port)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no port candidates found for pod network")
	}

	return result, nil
}

// waitForStatus blocks until the server with the specified ID reaches one of the target status.
// waitForStatus will fail if an error occurs, the operation it timeouts after the specified time, or the server status is not in the pending list.
func (ex *Executor) waitForStatus(serverID string, pending []string, target []string, secs int) error {
	return wait.Poll(time.Second, time.Duration(secs)*time.Second, func() (done bool, err error) {
		current, err := ex.Compute.GetServer(serverID)
		if err != nil {
			if client.IsNotFoundError(err) && strSliceContains(target, client.ServerStatusDeleted) {
				return true, nil
			}
			return false, err
		}

		klog.V(5).Infof("waiting for server [ID=%q] and current status %v, to reach status %v.", serverID, current.Status, target)
		if strSliceContains(target, current.Status) {
			return true, nil
		}

		// if there is no pending statuses defined or current status is in the pending list, then continue polling
		if len(pending) == 0 || strSliceContains(pending, current.Status) {
			return false, nil
		}

		retErr := fmt.Errorf("server [ID=%q] reached unexpected status %q", serverID, current.Status)
		if current.Status == client.ServerStatusError {
			retErr = fmt.Errorf("%s, fault: %+v", retErr, current.Fault)
		}

		return false, retErr
	})
}

// deployServer handles creating the server instance.
func (ex *Executor) deployServer(machineName string, userData []byte, nws []servers.Network) (*servers.Server, error) {
	keyName := ex.Config.Spec.KeyName
	imageName := ex.Config.Spec.ImageName
	imageID := ex.Config.Spec.ImageID
	securityGroups := ex.Config.Spec.SecurityGroups
	availabilityZone := ex.Config.Spec.AvailabilityZone
	metadata := ex.Config.Spec.Tags
	rootDiskSize := ex.Config.Spec.RootDiskSize
	useConfigDrive := ex.Config.Spec.UseConfigDrive
	flavorName := ex.Config.Spec.FlavorName

	var (
		imageRef   string
		createOpts servers.CreateOptsBuilder
		err        error
	)

	// use imageID if provided, otherwise try to resolve the imageName to an imageID
	if imageID != "" {
		imageRef = imageID
	} else {
		imageRef, err = ex.Compute.ImageIDFromName(imageName)
		if err != nil {
			return nil, fmt.Errorf("error resolving image ID from image name %q: %v", imageName, err)
		}
	}
	flavorRef, err := ex.Compute.FlavorIDFromName(flavorName)
	if err != nil {
		return nil, fmt.Errorf("error resolving flavor ID from flavor name %q: %v", imageName, err)
	}

	createOpts = &servers.CreateOpts{
		Name:             machineName,
		FlavorRef:        flavorRef,
		ImageRef:         imageRef,
		Networks:         nws,
		SecurityGroups:   securityGroups,
		Metadata:         metadata,
		UserData:         userData,
		AvailabilityZone: availabilityZone,
		ConfigDrive:      useConfigDrive,
	}

	createOpts = &keypairs.CreateOptsExt{
		CreateOptsBuilder: createOpts,
		KeyName:           keyName,
	}

	if ex.Config.Spec.ServerGroupID != nil {
		hints := schedulerhints.SchedulerHints{
			Group: *ex.Config.Spec.ServerGroupID,
		}
		createOpts = schedulerhints.CreateOptsExt{
			CreateOptsBuilder: createOpts,
			SchedulerHints:    hints,
		}
	}

	// If a custom block_device (root disk size is provided) we need to boot from volume
	if rootDiskSize > 0 {
		blockDevices, err := resourceInstanceBlockDevicesV2(rootDiskSize, imageRef)
		if err != nil {
			return nil, err
		}

		createOpts = &bootfromvolume.CreateOptsExt{
			CreateOptsBuilder: createOpts,
			BlockDevice:       blockDevices,
		}
		return ex.Compute.BootFromVolume(createOpts)
	}

	return ex.Compute.CreateServer(createOpts)
}

func resourceInstanceBlockDevicesV2(rootDiskSize int, imageID string) ([]bootfromvolume.BlockDevice, error) {
	blockDeviceOpts := make([]bootfromvolume.BlockDevice, 1)
	blockDeviceOpts[0] = bootfromvolume.BlockDevice{
		UUID:                imageID,
		VolumeSize:          rootDiskSize,
		BootIndex:           0,
		DeleteOnTermination: true,
		SourceType:          "image",
		DestinationType:     "volume",
	}
	klog.V(3).Infof("[DEBUG] Block Device Options: %+v", blockDeviceOpts)
	return blockDeviceOpts, nil
}

// patchServerPortsForPodNetwork updates a server's ports with rules for whitelisting the pod network CIDR.
func (ex *Executor) patchServerPortsForPodNetwork(serverID string) error {
	podNetworkPorts, err := ex.computePodNetworkIDs(serverID)
	if err != nil {
		return err
	}

	for _, port := range podNetworkPorts {
		if err := ex.Network.UpdatePort(port.ID, ports.UpdateOpts{
			AllowedAddressPairs: &[]ports.AddressPair{{IPAddress: ex.Config.Spec.PodNetworkCidr}},
		}); err != nil {
			return fmt.Errorf("failed to update allowed address pair for port [ID=%q]: %v", port.ID, err)
		}
	}
	return nil
}

// DeleteMachine deletes a server based on the supplied ID or name. The machine must have the cluster/role tags for any operation to take place.
// If providerID is specified it takes priority over the machineName. If no providerID is specified, DeleteMachine will
// try to resolve the machineName to an appropriate server ID.
func (ex *Executor) DeleteMachine(ctx context.Context, machineName, providerID string) error {
	var (
		err    error
		server *servers.Server
	)

	if isEmptyString(pointer.StringPtr(providerID)) {
		server, err = ex.getMachineByName(ctx, machineName)
	} else {
		server, err = ex.getMachineByProviderID(ctx, providerID)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	klog.V(1).Infof("deleting server [ID=%s]", server.ID)
	if err := ex.Compute.DeleteServer(server.ID); err != nil {
		return err
	}

	if err = ex.waitForStatus(server.ID, nil, []string{client.ServerStatusDeleted}, 300); err != nil {
		return fmt.Errorf("error while waiting for server [ID=%q] to be deleted: %v", server.ID, err)
	}

	if !isEmptyString(ex.Config.Spec.SubnetID) {
		return ex.deletePort(ctx, machineName)
	}

	return nil
}

func (ex *Executor) deletePort(_ context.Context, machineName string) error {
	portID, err := ex.Network.PortIDFromName(machineName)
	if err != nil {
		if client.IsNotFoundError(err) {
			klog.V(3).Infof("port [Name=%q] was not found", machineName)
			return nil
		}
		return fmt.Errorf("error deleting port with name %q: %s", machineName, err)
	}

	klog.V(3).Infof("deleting port [ID=%q]", portID)
	err = ex.Network.DeletePort(portID)
	if err != nil {
		klog.Errorf("failed to delete port [ID=%q]", portID)
		return err
	}
	klog.V(3).Infof("deleted port [ID=%q]", portID)

	return nil
}

// getMachineByProviderID fetches the data for a server based on a provider-encoded ID.
func (ex *Executor) getMachineByProviderID(_ context.Context, providerID string) (*servers.Server, error) {
	klog.V(2).Infof("finding server with providerID %s", providerID)
	serverID := DecodeProviderID(providerID)
	if isEmptyString(pointer.StringPtr(serverID)) {
		return nil, fmt.Errorf("could not parse serverID from providerID %q", providerID)
	}

	server, err := ex.Compute.GetServer(serverID)
	if err != nil {
		klog.V(2).Infof("error finding server [ID=%q]: %v", serverID, err)
		if client.IsNotFoundError(err) {
			// normalize errors by wrapping not found error
			return nil, fmt.Errorf("could not find server [ID=%q]: %w", serverID, ErrNotFound)
		}
		return nil, err
	}

	var (
		searchClusterName string
		searchNodeRole    string
	)
	for key := range ex.Config.Spec.Tags {
		if strings.Contains(key, cloudprovider.ServerTagClusterPrefix) {
			searchClusterName = key
		} else if strings.Contains(key, cloudprovider.ServerTagRolePrefix) {
			searchNodeRole = key
		}
	}

	if _, nameOk := server.Metadata[searchClusterName]; nameOk {
		if _, roleOk := server.Metadata[searchNodeRole]; roleOk {
			return server, nil
		}
	}

	klog.Warningf("server [ID=%q] found, but cluster/role tags are missing/not matching", serverID)
	return nil, fmt.Errorf("could not find server [ID=%q]: %w", serverID, ErrNotFound)
}

// getMachineByName returns a server that matches the following criteria:
// a) has the same name as machineName
// b) has the cluster and role tags as set in the machineClass
// The current approach is weak because the tags are currently stored as server metadata. Later Nova versions allow
// to store tags in a respective field and do a server-side filtering. To avoid incompatibility with older versions
// we will continue making the filtering clientside.
func (ex *Executor) getMachineByName(_ context.Context, machineName string) (*servers.Server, error) {
	var (
		searchClusterName string
		searchNodeRole    string
	)

	for key := range ex.Config.Spec.Tags {
		if strings.Contains(key, cloudprovider.ServerTagClusterPrefix) {
			searchClusterName = key
		} else if strings.Contains(key, cloudprovider.ServerTagRolePrefix) {
			searchNodeRole = key
		}
	}

	if searchClusterName == "" || searchNodeRole == "" {
		klog.Warningf("getMachineByName operation can not proceed: cluster/role tags are missing for machine [Name=%q]", machineName)
		return nil, fmt.Errorf("getMachineByName operation can not proceed: cluster/role tags are missing for machine [Name=%q]", machineName)
	}

	listedServers, err := ex.Compute.ListServers(&servers.ListOpts{
		Name: machineName,
	})
	if err != nil {
		return nil, err
	}

	matchingServers := []servers.Server{}
	for _, server := range listedServers {
		if server.Name == machineName {
			if _, nameOk := server.Metadata[searchClusterName]; nameOk {
				if _, roleOk := server.Metadata[searchNodeRole]; roleOk {
					matchingServers = append(matchingServers, server)
				}
			}
		}
	}

	if len(matchingServers) > 1 {
		return nil, fmt.Errorf("failed to find server [Name=%q]: %w", machineName, ErrMultipleFound)
	} else if len(matchingServers) == 0 {
		return nil, fmt.Errorf("failed to find server [Name=%q]: %w", machineName, ErrNotFound)
	}

	return &matchingServers[0], nil
}

// GetMachineStatus returns the provider-encoded ID of a server.
func (ex *Executor) GetMachineStatus(ctx context.Context, machineName string) (string, error) {
	server, err := ex.getMachineByName(ctx, machineName)
	if err != nil {
		return "", err
	}

	err = ex.verifyServerPortsForPodNetwork(server.ID)
	if err != nil {
		return "", err
	}

	return EncodeProviderID(ex.Config.Spec.Region, server.ID), nil
}

func (ex *Executor) verifyServerPortsForPodNetwork(serverID string) error {
	podNetworkPorts, err := ex.computePodNetworkIDs(serverID)
	if err != nil {
		return err
	}

outerLoop:
	for _, port := range podNetworkPorts {
		for _, addressPair := range port.AllowedAddressPairs {
			if addressPair.IPAddress == ex.Config.Spec.PodNetworkCidr {
				continue outerLoop
			}
		}
		return fmt.Errorf("port [ID=%q] of server [ID=%q] is not configured for pod network, but it should", port.ID, serverID)
	}
	return nil
}

// ListMachines lists all servers.
func (ex *Executor) ListMachines(_ context.Context) (map[string]string, error) {
	searchClusterName := ""
	searchNodeRole := ""

	for key := range ex.Config.Spec.Tags {
		if strings.Contains(key, cloudprovider.ServerTagClusterPrefix) {
			searchClusterName = key
		} else if strings.Contains(key, cloudprovider.ServerTagRolePrefix) {
			searchNodeRole = key
		}
	}

	//
	if searchClusterName == "" || searchNodeRole == "" {
		klog.Warningf("operation can not proceed: cluster/role tags are missing")
		return nil, fmt.Errorf("operation can not proceed: cluster/role tags are missing")
	}

	servers, err := ex.Compute.ListServers(&servers.ListOpts{})
	if err != nil {
		return nil, err
	}

	result := map[string]string{}
	for _, server := range servers {
		if _, nameOk := server.Metadata[searchClusterName]; nameOk {
			if _, roleOk := server.Metadata[searchNodeRole]; roleOk {
				providerID := EncodeProviderID(ex.Config.Spec.Region, server.ID)
				result[providerID] = server.Name
			}
		}
	}

	return result, nil
}
