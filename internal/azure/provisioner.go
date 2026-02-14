/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

const (
	// InstanceIDPrefix is the prefix for Azure instance IDs (resourceGroup/vmName)
	InstanceIDPrefix = "azure/"
	// NICNameSuffix is appended to VM name for the NIC resource name
	NICNameSuffix = "-nic"
	// MaxRetries is the maximum number of retry attempts
	MaxRetries = 3
	// RetryDelay is the initial delay between retries
	RetryDelay = 2 * time.Second
	// DefaultVNetAddressPrefix is the default address space for auto-created VNets
	DefaultVNetAddressPrefix = "10.0.0.0/16"
	// DefaultSubnetAddressPrefix is the default address prefix for auto-created subnets
	DefaultSubnetAddressPrefix = "10.0.0.0/24"
)

// Provisioner handles Azure VM provisioning (NIC + VM).
// It auto-creates the resource group, VNet, and subnet if they don't exist.
type Provisioner struct {
	vmClient      *armcompute.VirtualMachinesClient
	nicClient     *armnetwork.InterfacesClient
	rgClient      *armresources.ResourceGroupsClient
	vnetClient    *armnetwork.VirtualNetworksClient
	subnetClient  *armnetwork.SubnetsClient
	subID         string
	resourceGroup string
	location      string
	subnetID      string
	infraReady    bool // true after ensureInfrastructure succeeds
}

// NewProvisioner creates a new Azure provisioner using Service Principal or default credential chain.
// tenantID, clientID, clientSecret can be empty to use default Azure credential (env, managed identity).
func NewProvisioner(ctx context.Context, subscriptionID, resourceGroup, location, subnetID string, tenantID, clientID, clientSecret string) (*Provisioner, error) {
	if subscriptionID == "" || resourceGroup == "" || location == "" {
		return nil, fmt.Errorf("subscriptionID, resourceGroup, and location are required")
	}
	if subnetID == "" {
		return nil, fmt.Errorf("subnetID is required for Azure VM (create NIC in subnet)")
	}

	var cred azcore.TokenCredential
	var err error
	if tenantID != "" && clientID != "" && clientSecret != "" {
		cred, err = azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, nil)
	} else {
		cred, err = azidentity.NewDefaultAzureCredential(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure credential: %w", err)
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM client: %w", err)
	}
	nicClient, err := armnetwork.NewInterfacesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create NIC client: %w", err)
	}
	rgClient, err := armresources.NewResourceGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource groups client: %w", err)
	}
	vnetClient, err := armnetwork.NewVirtualNetworksClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create VNet client: %w", err)
	}
	subnetClient, err := armnetwork.NewSubnetsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create subnet client: %w", err)
	}

	return &Provisioner{
		vmClient:      vmClient,
		nicClient:     nicClient,
		rgClient:      rgClient,
		vnetClient:    vnetClient,
		subnetClient:  subnetClient,
		subID:         subscriptionID,
		resourceGroup: resourceGroup,
		location:      location,
		subnetID:      subnetID,
	}, nil
}

// ensureInfrastructure creates the resource group, VNet, and subnet if they don't already exist.
// It parses the subnetID to extract VNet and subnet names.
// Idempotent: safe to call multiple times; skipped after first success.
func (p *Provisioner) ensureInfrastructure(ctx context.Context) error {
	if p.infraReady {
		return nil
	}

	vnetName, subnetName, err := parseSubnetID(p.subnetID)
	if err != nil {
		return fmt.Errorf("cannot parse subnetID %q: %w", p.subnetID, err)
	}

	// 1. Ensure resource group
	if _, err := p.rgClient.Get(ctx, p.resourceGroup, nil); err != nil {
		// Resource group doesn't exist -- create it
		if _, err := p.rgClient.CreateOrUpdate(ctx, p.resourceGroup, armresources.ResourceGroup{
			Location: ptr(p.location),
		}, nil); err != nil {
			return fmt.Errorf("failed to create resource group %s: %w", p.resourceGroup, err)
		}
	}

	// 2. Ensure VNet (only creates if it doesn't exist; no-ops otherwise)
	if _, err := p.vnetClient.Get(ctx, p.resourceGroup, vnetName, nil); err != nil {
		vnetPoller, err := p.vnetClient.BeginCreateOrUpdate(ctx, p.resourceGroup, vnetName, armnetwork.VirtualNetwork{
			Location: ptr(p.location),
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{
					AddressPrefixes: []*string{ptr(DefaultVNetAddressPrefix)},
				},
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to create VNet %s: %w", vnetName, err)
		}
		if _, err := vnetPoller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("failed waiting for VNet %s: %w", vnetName, err)
		}
	}

	// 3. Ensure subnet (only creates if it doesn't exist; no-ops otherwise)
	if _, err := p.subnetClient.Get(ctx, p.resourceGroup, vnetName, subnetName, nil); err != nil {
		subnetPoller, err := p.subnetClient.BeginCreateOrUpdate(ctx, p.resourceGroup, vnetName, subnetName, armnetwork.Subnet{
			Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix: ptr(DefaultSubnetAddressPrefix),
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to create subnet %s: %w", subnetName, err)
		}
		if _, err := subnetPoller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("failed waiting for subnet %s: %w", subnetName, err)
		}
	}

	p.infraReady = true
	return nil
}

// parseSubnetID extracts vnetName and subnetName from a full ARM subnet resource ID:
// /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/virtualNetworks/{vnet}/subnets/{subnet}
func parseSubnetID(subnetID string) (vnetName, subnetName string, err error) {
	lower := strings.ToLower(subnetID)
	vnetIdx := strings.Index(lower, "/virtualnetworks/")
	subnetIdx := strings.Index(lower, "/subnets/")
	if vnetIdx < 0 || subnetIdx < 0 || subnetIdx <= vnetIdx {
		return "", "", fmt.Errorf("subnetID must contain /virtualNetworks/{name}/subnets/{name}")
	}
	// Extract from original string (preserve case)
	vnetStart := vnetIdx + len("/virtualnetworks/")
	vnetName = subnetID[vnetStart : vnetStart+(subnetIdx-vnetStart)]
	subnetName = subnetID[subnetIdx+len("/subnets/"):]
	if vnetName == "" || subnetName == "" {
		return "", "", fmt.Errorf("empty VNet or subnet name in subnetID: %s", subnetID)
	}
	return vnetName, subnetName, nil
}

// CreateInstance creates an Azure VM with a NIC in the configured subnet.
// It ensures the resource group, VNet, and subnet exist before creating the VM.
// name is the VM name, vmSize is the instance type (e.g. Standard_B2s), locationParam is ignored (use provisioner location), startupScript is cloud-init, imageFromConfig is optional (e.g. "Canonical:0001-com-ubuntu-server-jammy:22_04-lts:latest" or empty for default).
// Returns instanceID in the form "azure/<resourceGroup>/<vmName>".
func (p *Provisioner) CreateInstance(ctx context.Context, name, vmSize, locationParam, startupScript, imageFromConfig string) (string, error) {
	// Ensure infrastructure (resource group, VNet, subnet) before the retry loop.
	if err := p.ensureInfrastructure(ctx); err != nil {
		return "", fmt.Errorf("failed to ensure Azure infrastructure: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			waitTime := RetryDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(waitTime):
			}
		}
		instanceID, err := p.createInstanceOnce(ctx, name, vmSize, startupScript, imageFromConfig)
		if err == nil {
			return instanceID, nil
		}
		if !isRetryableError(err) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("failed after %d attempts: %w", MaxRetries+1, lastErr)
}

func (p *Provisioner) createInstanceOnce(ctx context.Context, name, vmSize, startupScript, imageFromConfig string) (string, error) {
	nicName := name + NICNameSuffix

	// Create NIC in the subnet
	nicParams := armnetwork.Interface{
		Location: ptr(p.location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Name: ptr("ipconfig1"),
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						Subnet:                    &armnetwork.Subnet{ID: ptr(p.subnetID)},
						PrivateIPAllocationMethod: ptr(armnetwork.IPAllocationMethodDynamic),
					},
				},
			},
		},
	}
	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.resourceGroup, nicName, nicParams, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create NIC: %w", err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed waiting for NIC: %w", err)
	}

	// Image reference: default Ubuntu 22.04 if not specified
	imageRef := parseImageReference(imageFromConfig)

	vmParams := armcompute.VirtualMachine{
		Location: ptr(p.location),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: ptr(armcompute.VirtualMachineSizeTypes(vmSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: imageRef,
				OSDisk: &armcompute.OSDisk{
					CreateOption: ptr(armcompute.DiskCreateOptionTypesFromImage),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						StorageAccountType: ptr(armcompute.StorageAccountTypesPremiumLRS),
					},
					DiskSizeGB: ptr(int32(30)),
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  ptr(name),
				AdminUsername: ptr("azureuser"),
				// Use a random password for managed VMs (SSH access is via Tailscale, not direct).
				// Azure requires either a password or SSH key; password is simplest for disposable autoscaler VMs.
				AdminPassword: ptr(generatePassword()),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: ptr(false),
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{ID: ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkInterfaces/%s", p.subID, p.resourceGroup, nicName))},
				},
			},
		},
	}
	if startupScript != "" {
		vmParams.Properties.OSProfile.CustomData = ptr(base64.StdEncoding.EncodeToString([]byte(startupScript)))
	}

	vmPoller, err := p.vmClient.BeginCreateOrUpdate(ctx, p.resourceGroup, name, vmParams, nil)
	if err != nil {
		_, _ = p.nicClient.BeginDelete(ctx, p.resourceGroup, nicName, nil)
		return "", fmt.Errorf("failed to create VM: %w", err)
	}
	_, err = vmPoller.PollUntilDone(ctx, nil)
	if err != nil {
		_, _ = p.vmClient.BeginDelete(ctx, p.resourceGroup, name, nil)
		_, _ = p.nicClient.BeginDelete(ctx, p.resourceGroup, nicName, nil)
		return "", fmt.Errorf("failed waiting for VM: %w", err)
	}

	return fmt.Sprintf("%s%s/%s", InstanceIDPrefix, p.resourceGroup, name), nil
}

func parseImageReference(s string) *armcompute.ImageReference {
	if s == "" {
		return &armcompute.ImageReference{
			Publisher: ptr("Canonical"),
			Offer:     ptr("0001-com-ubuntu-server-jammy"),
			SKU:       ptr("22_04-lts-gen2"),
			Version:   ptr("latest"),
		}
	}
	// Format "Publisher:Offer:Sku:Version" or "Publisher:Offer:Sku"
	parts := strings.SplitN(s, ":", 4)
	ref := &armcompute.ImageReference{}
	if len(parts) >= 1 && parts[0] != "" {
		ref.Publisher = &parts[0]
	}
	if len(parts) >= 2 && parts[1] != "" {
		ref.Offer = &parts[1]
	}
	if len(parts) >= 3 && parts[2] != "" {
		ref.SKU = &parts[2]
	}
	if len(parts) >= 4 && parts[3] != "" {
		ref.Version = &parts[3]
	} else {
		ref.Version = ptr("latest")
	}
	return ref
}

func ptr[T any](v T) *T { return &v }

// generatePassword creates a random password that meets Azure complexity requirements
// (12+ chars, uppercase, lowercase, digit, special char). These passwords are for
// disposable autoscaler VMs where SSH access goes through Tailscale, not password login.
func generatePassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	// Prefix guarantees uppercase + lowercase + special + digit; base64 body adds length
	return "Cb!9" + base64.URLEncoding.EncodeToString(b)
}

// DeleteInstance deletes the Azure VM and its NIC. instanceID must be "azure/<resourceGroup>/<vmName>".
func (p *Provisioner) DeleteInstance(ctx context.Context, instanceID string) error {
	_, vmName, err := parseInstanceID(instanceID)
	if err != nil {
		return err
	}
	nicName := vmName + NICNameSuffix

	vmPoller, err := p.vmClient.BeginDelete(ctx, p.resourceGroup, vmName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete VM: %w", err)
	}
	_, _ = vmPoller.PollUntilDone(ctx, nil)

	nicPoller, err := p.nicClient.BeginDelete(ctx, p.resourceGroup, nicName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete NIC: %w", err)
	}
	_, _ = nicPoller.PollUntilDone(ctx, nil)
	return nil
}

// IsInstanceRunning returns true if the VM is running.
func (p *Provisioner) IsInstanceRunning(ctx context.Context, instanceID string) (bool, error) {
	_, vmName, err := parseInstanceID(instanceID)
	if err != nil {
		return false, err
	}

	resp, err := p.vmClient.Get(ctx, p.resourceGroup, vmName, &armcompute.VirtualMachinesClientGetOptions{
		Expand: ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		return false, fmt.Errorf("failed to get VM: %w", err)
	}
	if resp.Properties == nil || resp.Properties.InstanceView == nil || len(resp.Properties.InstanceView.Statuses) == 0 {
		return false, nil
	}
	for _, s := range resp.Properties.InstanceView.Statuses {
		if s.Code != nil && *s.Code == "PowerState/running" {
			return true, nil
		}
	}
	return false, nil
}

// Close releases resources. Azure SDK clients have no explicit close; no-op.
func (p *Provisioner) Close() error {
	return nil
}

// GenerateInstanceName returns the Azure VM name for a NodeClaim.
func GenerateInstanceName(nodeClaimName string) string {
	return nodeClaimName
}

func parseInstanceID(instanceID string) (string, string, error) {
	if !strings.HasPrefix(instanceID, InstanceIDPrefix) {
		return "", "", fmt.Errorf("invalid Azure instance ID format: %s", instanceID)
	}
	rest := strings.TrimPrefix(instanceID, InstanceIDPrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid Azure instance ID format: %s", instanceID)
	}
	return parts[0], parts[1], nil
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	retryable := []string{"rate limit", "throttl", "temporary", "timeout", "deadline exceeded", "unavailable", "503", "502", "500"}
	for _, r := range retryable {
		if strings.Contains(strings.ToLower(errStr), r) {
			return true
		}
	}
	return false
}
