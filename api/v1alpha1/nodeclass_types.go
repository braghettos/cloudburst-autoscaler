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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeClassSpec defines the desired state of NodeClass
type NodeClassSpec struct {
	// GCP configuration (used when NodeClaim Chosen.Provider is gcp)
	// +optional
	GCP GCPConfig `json:"gcp,omitempty"`

	// Scaleway configuration (used when NodeClaim Chosen.Provider is scaleway)
	// +optional
	Scaleway *ScalewayConfig `json:"scaleway,omitempty"`

	// Hetzner Cloud configuration (used when NodeClaim Chosen.Provider is hetzner)
	// +optional
	Hetzner *HetznerConfig `json:"hetzner,omitempty"`

	// Join configuration for joining the Kubernetes cluster
	Join JoinConfig `json:"join"`

	// Tailscale configuration for VPN connectivity
	Tailscale TailscaleConfig `json:"tailscale"`

	// Bootstrap configuration
	// +optional
	Bootstrap *BootstrapConfig `json:"bootstrap,omitempty"`
}

// GCPConfig defines GCP-specific configuration
type GCPConfig struct {
	// Project is the GCP project ID
	Project string `json:"project"`

	// Zone is the GCP zone
	Zone string `json:"zone"`

	// ImageFamily is the image family to use (default: ubuntu-2204-lts)
	// +kubebuilder:default=ubuntu-2204-lts
	ImageFamily string `json:"imageFamily,omitempty"`

	// ImageProject is the image project (default: ubuntu-os-cloud)
	// +kubebuilder:default=ubuntu-os-cloud
	ImageProject string `json:"imageProject,omitempty"`
}

// ScalewayConfig defines Scaleway-specific configuration
type ScalewayConfig struct {
	// Zone is the Scaleway zone (e.g. fr-par-1, nl-ams-1)
	Zone string `json:"zone"`

	// ProjectID is the Scaleway project ID (optional; can use default from credentials)
	// +optional
	ProjectID string `json:"projectID,omitempty"`

	// OrganizationID is the Scaleway organization ID (optional; can use default from credentials)
	// +optional
	OrganizationID string `json:"organizationID,omitempty"`

	// Image is the Scaleway image identifier (e.g. ubuntu_jammy, or image UUID).
	// Defaults to Ubuntu 22.04 if empty.
	// +optional
	Image string `json:"image,omitempty"`

	// APIKeySecretRef references a secret containing the Scaleway API credentials.
	// Secret must have key "SCW_SECRET_KEY" or "secret-key" (required).
	// Secret may also have "SCW_ACCESS_KEY" or "access-key" (optional but recommended).
	// The controller will set both SCW_SECRET_KEY and SCW_ACCESS_KEY environment variables
	// from the secret if present. Alternatively set SCW_SECRET_KEY and SCW_ACCESS_KEY env vars.
	// +optional
	APIKeySecretRef *corev1.SecretKeySelector `json:"apiKeySecretRef,omitempty"`
}

// HetznerConfig defines Hetzner Cloud-specific configuration
type HetznerConfig struct {
	// Location is the Hetzner Cloud datacenter location (e.g. "fsn1", "nbg1", "hel1")
	Location string `json:"location"`

	// APITokenSecretRef references a secret containing the Hetzner Cloud API token.
	// Secret must have the key specified (default "HETZNER_API_TOKEN").
	// +optional
	APITokenSecretRef *corev1.SecretKeySelector `json:"apiTokenSecretRef,omitempty"`

	// Image is the Hetzner Cloud image name or ID (default: "ubuntu-22.04")
	// +optional
	Image string `json:"image,omitempty"`
}

// JoinConfig defines how nodes join the Kubernetes cluster
type JoinConfig struct {
	// HostApiServer is the API server endpoint (e.g. https://<HOST_TAILSCALE_IP>:6443)
	HostApiServer string `json:"hostApiServer"`

	// KindClusterName is the kind cluster name (default: cloudburst)
	// The control plane container name will be constructed as <KindClusterName>-control-plane
	// +kubebuilder:default=cloudburst
	KindClusterName string `json:"kindClusterName,omitempty"`

	// KindControlPlaneContainer is the kind control plane container name (optional, overrides KindClusterName)
	// If not set, will be constructed as <KindClusterName>-control-plane
	// +optional
	KindControlPlaneContainer string `json:"kindControlPlaneContainer,omitempty"`

	// TokenTtlMinutes is the token TTL in minutes (default: 30)
	// +kubebuilder:default=30
	TokenTtlMinutes int32 `json:"tokenTtlMinutes,omitempty"`
}

// TailscaleConfig defines Tailscale VPN configuration
type TailscaleConfig struct {
	// AuthKeySecretRef references a secret containing the Tailscale auth key (static key for all nodes).
	// Ignored if EphemeralKeyFromAPI is set.
	// +optional
	AuthKeySecretRef *corev1.SecretKeySelector `json:"authKeySecretRef,omitempty"`

	// EphemeralKeyFromAPI when true, the controller creates a new ephemeral auth key per NodeClaim
	// via the Tailscale API instead of using a key from specs. Requires APITokenSecretRef.
	// Nodes are removed from the tailnet when they run `tailscale logout` (e.g. on VM shutdown).
	// +optional
	EphemeralKeyFromAPI bool `json:"ephemeralKeyFromAPI,omitempty"`

	// APITokenSecretRef references a secret containing the Tailscale API token (tskey-api-...).
	// Required when EphemeralKeyFromAPI is true. The token must have devices:write (create keys).
	// +optional
	APITokenSecretRef *corev1.SecretKeySelector `json:"apiTokenSecretRef,omitempty"`
}

// BootstrapConfig defines bootstrap configuration
type BootstrapConfig struct {
	// KubernetesVersion is the Kubernetes version to use (optional)
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
}

// NodeClassStatus defines the observed state of NodeClass
type NodeClassStatus struct {
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NodeClass is the Schema for the nodeclasses API
type NodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeClassSpec   `json:"spec,omitempty"`
	Status NodeClassStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NodeClassList contains a list of NodeClass
type NodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeClass{}, &NodeClassList{})
}
