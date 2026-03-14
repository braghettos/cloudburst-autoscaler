package azure

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerateInstanceName(t *testing.T) {
	tests := []struct {
		nodeClaimName string
		want          string
	}{
		{"cb-pool1-abcde", "cb-pool1-abcde"},
		{"cb-gpu-pool-xyz12", "cb-gpu-pool-xyz12"},
		{"my-node", "my-node"},
	}
	for _, tt := range tests {
		t.Run(tt.nodeClaimName, func(t *testing.T) {
			got := GenerateInstanceName(tt.nodeClaimName)
			if got != tt.want {
				t.Errorf("GenerateInstanceName(%q) = %q, want %q", tt.nodeClaimName, got, tt.want)
			}
		})
	}
}

func TestParseInstanceID_Valid(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		wantRG     string
		wantVM     string
	}{
		{
			name:       "standard format",
			instanceID: "azure/my-resource-group/my-vm",
			wantRG:     "my-resource-group",
			wantVM:     "my-vm",
		},
		{
			name:       "complex rg name",
			instanceID: "azure/rg-cloudburst-prod-eastus/cb-pool-abc123",
			wantRG:     "rg-cloudburst-prod-eastus",
			wantVM:     "cb-pool-abc123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rg, vm, err := parseInstanceID(tt.instanceID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rg != tt.wantRG {
				t.Errorf("resourceGroup = %q, want %q", rg, tt.wantRG)
			}
			if vm != tt.wantVM {
				t.Errorf("vmName = %q, want %q", vm, tt.wantVM)
			}
		})
	}
}

func TestParseInstanceID_Invalid(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
	}{
		{"wrong prefix", "aws/us-east-1/i-123"},
		{"no prefix", "my-rg/my-vm"},
		{"empty", ""},
		{"prefix only", "azure/"},
		{"no vm name", "azure/my-rg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseInstanceID(tt.instanceID)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestNICNameSuffix(t *testing.T) {
	vmName := "cb-pool-abc"
	nicName := vmName + NICNameSuffix
	if nicName != "cb-pool-abc-nic" {
		t.Errorf("NIC name = %q, want %q", nicName, "cb-pool-abc-nic")
	}
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{"nil", nil, false},
		{"azure throttling", errors.New("Throttling: Too many requests"), true},
		{"timeout (base)", errors.New("request timeout"), true},
		{"connection refused (base)", errors.New("connection refused"), true},
		{"permission denied", errors.New("AuthorizationFailed"), false},
		{"not found", errors.New("ResourceNotFound"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryableError(tt.err)
			if got != tt.wantRetry {
				t.Errorf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.wantRetry)
			}
		})
	}
}

func TestParseSubnetID_Valid(t *testing.T) {
	tests := []struct {
		name       string
		subnetID   string
		wantVNet   string
		wantSubnet string
	}{
		{
			name:       "full ARM id",
			subnetID:   "/subscriptions/11111111-2222-3333-4444-555555555555/resourceGroups/my-rg/providers/Microsoft.Network/virtualNetworks/my-vnet/subnets/my-subnet",
			wantVNet:   "my-vnet",
			wantSubnet: "my-subnet",
		},
		{
			name:       "case insensitive",
			subnetID:   "/subscriptions/sub123/resourceGroups/rg1/providers/Microsoft.Network/VirtualNetworks/MyVNet/Subnets/MySubnet",
			wantVNet:   "MyVNet",
			wantSubnet: "MySubnet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vnet, subnet, err := parseSubnetID(tt.subnetID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if vnet != tt.wantVNet {
				t.Errorf("vnet = %q, want %q", vnet, tt.wantVNet)
			}
			if subnet != tt.wantSubnet {
				t.Errorf("subnet = %q, want %q", subnet, tt.wantSubnet)
			}
		})
	}
}

func TestParseSubnetID_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		subnetID string
	}{
		{"missing virtualNetworks", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/subnets/s"},
		{"missing subnets", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v"},
		{"empty", ""},
		{"random string", "just-a-string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseSubnetID(tt.subnetID)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestParseImageReference_Default(t *testing.T) {
	ref := parseImageReference("", "Standard_B2s")
	if ref.Publisher == nil || *ref.Publisher != "Canonical" {
		t.Errorf("Publisher = %v, want Canonical", ref.Publisher)
	}
	if ref.SKU == nil || *ref.SKU != "22_04-lts-gen2" {
		t.Errorf("SKU = %v, want 22_04-lts-gen2", ref.SKU)
	}
}

func TestParseImageReference_Gen1ForASeries(t *testing.T) {
	ref := parseImageReference("", "Standard_A2_v2")
	if ref.SKU == nil || *ref.SKU != "22_04-lts" {
		t.Errorf("SKU = %v, want 22_04-lts (Gen1 for A-series)", ref.SKU)
	}
}

func TestParseImageReference_Gen2OverrideForASeries(t *testing.T) {
	ref := parseImageReference("Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest", "Standard_A2_v2")
	if ref.SKU == nil || *ref.SKU != "22_04-lts" {
		t.Errorf("SKU = %v, want 22_04-lts (gen2 downgraded for A-series)", ref.SKU)
	}
}

func TestParseImageReference_CustomImage(t *testing.T) {
	ref := parseImageReference("MyPublisher:MyOffer:MySku:1.0", "Standard_D2s_v3")
	if ref.Publisher == nil || *ref.Publisher != "MyPublisher" {
		t.Errorf("Publisher = %v, want MyPublisher", ref.Publisher)
	}
	if ref.Offer == nil || *ref.Offer != "MyOffer" {
		t.Errorf("Offer = %v, want MyOffer", ref.Offer)
	}
	if ref.SKU == nil || *ref.SKU != "MySku" {
		t.Errorf("SKU = %v, want MySku", ref.SKU)
	}
	if ref.Version == nil || *ref.Version != "1.0" {
		t.Errorf("Version = %v, want 1.0", ref.Version)
	}
}

func TestParseImageReference_ThreeParts(t *testing.T) {
	ref := parseImageReference("Pub:Offer:Sku", "Standard_D2s_v3")
	if ref.Version == nil || *ref.Version != "latest" {
		t.Errorf("Version = %v, want latest (default when only 3 parts)", ref.Version)
	}
}

func TestGeneratePassword(t *testing.T) {
	pw := generatePassword()
	if len(pw) < 12 {
		t.Errorf("password too short: %d chars", len(pw))
	}
	if !strings.HasPrefix(pw, "Cb!9") {
		t.Error("password missing complexity prefix")
	}
	// Verify uniqueness (two calls should produce different passwords)
	pw2 := generatePassword()
	if pw == pw2 {
		t.Error("two generated passwords should differ")
	}
}

func TestConstants(t *testing.T) {
	if InstanceIDPrefix != "azure/" {
		t.Errorf("InstanceIDPrefix = %q, want %q", InstanceIDPrefix, "azure/")
	}
	if NICNameSuffix != "-nic" {
		t.Errorf("NICNameSuffix = %q, want %q", NICNameSuffix, "-nic")
	}
	if DefaultVNetAddressPrefix != "10.0.0.0/16" {
		t.Errorf("DefaultVNetAddressPrefix = %q, want %q", DefaultVNetAddressPrefix, "10.0.0.0/16")
	}
	if DefaultSubnetAddressPrefix != "10.0.0.0/24" {
		t.Errorf("DefaultSubnetAddressPrefix = %q, want %q", DefaultSubnetAddressPrefix, "10.0.0.0/24")
	}
}
