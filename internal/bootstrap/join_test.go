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

package bootstrap

import (
	"strings"
	"testing"
)

func TestPrepareDiscoveryKubeconfigForProxy(t *testing.T) {
	// List-format cluster-info (as from kind cluster)
	input := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTi...
    server: https://127.0.0.1:6443
  name: ""
`
	got := PrepareDiscoveryKubeconfigForProxy(input, "https://tailscale-operator:443")
	// Must have proxy server URL (quoted)
	if !strings.Contains(got, `server: "https://tailscale-operator:443"`) {
		t.Errorf("expected server URL to be replaced and quoted, got:\n%s", got)
	}
	// Must not have certificate-authority-data (kubeadm forbids it with insecure-skip-tls-verify)
	if strings.Contains(got, "certificate-authority-data:") {
		t.Errorf("expected certificate-authority-data to be removed, got:\n%s", got)
	}
	// Must have insecure-skip-tls-verify
	if !strings.Contains(got, "insecure-skip-tls-verify: true") {
		t.Errorf("expected insecure-skip-tls-verify: true in cluster block, got:\n%s", got)
	}
}

func TestNewJoinCommandProvider(t *testing.T) {
	tests := []struct {
		name            string
		containerName   string
		kindClusterName string
		tokenTTL        int32
		expectedName    string
		expectedTTL     int32
	}{
		{
			name:            "container name provided",
			containerName:   "custom-container",
			kindClusterName: "cloudburst",
			tokenTTL:        30,
			expectedName:    "custom-container",
			expectedTTL:     30,
		},
		{
			name:            "kind cluster name provided, no container name",
			containerName:   "",
			kindClusterName: "cloudburst",
			tokenTTL:        30,
			expectedName:    "cloudburst-control-plane",
			expectedTTL:     30,
		},
		{
			name:            "neither provided, uses default",
			containerName:   "",
			kindClusterName: "",
			tokenTTL:        30,
			expectedName:    DefaultKindControlPlaneContainer,
			expectedTTL:     30,
		},
		{
			name:            "container name overrides kind cluster name",
			containerName:   "override-container",
			kindClusterName: "cloudburst",
			tokenTTL:        30,
			expectedName:    "override-container",
			expectedTTL:     30,
		},
		{
			name:            "default token TTL when zero",
			containerName:   "test-container",
			kindClusterName: "",
			tokenTTL:        0,
			expectedName:    "test-container",
			expectedTTL:     DefaultTokenTTLMinutes,
		},
		{
			name:            "custom kind cluster name",
			containerName:   "",
			kindClusterName: "my-cluster",
			tokenTTL:        60,
			expectedName:    "my-cluster-control-plane",
			expectedTTL:     60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := NewJoinCommandProvider(tt.containerName, tt.kindClusterName, tt.tokenTTL)

			if provider.containerName != tt.expectedName {
				t.Errorf("expected container name %q, got %q", tt.expectedName, provider.containerName)
			}

			if provider.tokenTTL != tt.expectedTTL {
				t.Errorf("expected token TTL %d, got %d", tt.expectedTTL, provider.tokenTTL)
			}
		})
	}
}
