package gcp

import (
	"fmt"
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

func TestGetProject(t *testing.T) {
	if got := GetProject("my-project"); got != "my-project" {
		t.Errorf("GetProject = %q, want %q", got, "my-project")
	}
	if got := GetProject(""); got != "" {
		t.Errorf("GetProject(\"\") = %q, want empty", got)
	}
}

func TestGetZone(t *testing.T) {
	if got := GetZone("us-central1-a"); got != "us-central1-a" {
		t.Errorf("GetZone = %q, want %q", got, "us-central1-a")
	}
	if got := GetZone(""); got != "" {
		t.Errorf("GetZone(\"\") = %q, want empty", got)
	}
}

func TestParseInstanceID_GCPFormat(t *testing.T) {
	// GCP uses inline parsing in deleteInstanceOnce/IsInstanceRunning.
	// We replicate the logic here to test the expected format.
	tests := []struct {
		name        string
		instanceID  string
		wantProject string
		wantZone    string
		wantName    string
		wantErr     bool
	}{
		{
			name:        "valid id",
			instanceID:  "projects/my-project/zones/us-central1-a/instances/cb-pool-abc",
			wantProject: "my-project",
			wantZone:    "us-central1-a",
			wantName:    "cb-pool-abc",
		},
		{
			name:        "valid with hyphens",
			instanceID:  "projects/my-proj-123/zones/europe-west1-b/instances/my-vm-name",
			wantProject: "my-proj-123",
			wantZone:    "europe-west1-b",
			wantName:    "my-vm-name",
		},
		{
			name:       "too few parts",
			instanceID: "projects/my-project/zones",
			wantErr:    true,
		},
		{
			name:       "wrong prefix",
			instanceID: "aws/us-east-1/i-12345",
			wantErr:    true,
		},
		{
			name:       "wrong structure - missing instances segment",
			instanceID: "projects/p/zones/z/vms/n",
			wantErr:    true,
		},
		{
			name:       "empty",
			instanceID: "",
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, zone, name, err := parseGCPInstanceID(tt.instanceID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if project != tt.wantProject {
				t.Errorf("project = %q, want %q", project, tt.wantProject)
			}
			if zone != tt.wantZone {
				t.Errorf("zone = %q, want %q", zone, tt.wantZone)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
		})
	}
}

func TestFormatInstanceID(t *testing.T) {
	project := "my-project"
	zone := "us-central1-a"
	name := "cb-pool-abc"
	want := "projects/my-project/zones/us-central1-a/instances/cb-pool-abc"
	got := fmt.Sprintf("projects/%s/zones/%s/instances/%s", project, zone, name)
	if got != want {
		t.Errorf("formatted ID = %q, want %q", got, want)
	}
}

func TestDefaultConstants(t *testing.T) {
	if DefaultImageFamily != "ubuntu-2204-lts" {
		t.Errorf("DefaultImageFamily = %q, want %q", DefaultImageFamily, "ubuntu-2204-lts")
	}
	if DefaultImageProject != "ubuntu-os-cloud" {
		t.Errorf("DefaultImageProject = %q, want %q", DefaultImageProject, "ubuntu-os-cloud")
	}
}

// parseGCPInstanceID mirrors the inline parsing in provisioner.go
// (deleteInstanceOnce / IsInstanceRunning).
func parseGCPInstanceID(instanceID string) (project, zone, name string, err error) {
	parts := strings.Split(instanceID, "/")
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "zones" || parts[4] != "instances" {
		return "", "", "", fmt.Errorf("invalid instance ID format: %s", instanceID)
	}
	return parts[1], parts[3], parts[5], nil
}
