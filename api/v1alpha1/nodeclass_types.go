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
	// GCP configuration
	GCP GCPConfig `json:"gcp"`

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
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
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
