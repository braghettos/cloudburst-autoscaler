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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodePoolSpec defines the desired state of NodePool
type NodePoolSpec struct {
	// Requirements define constraints for node selection
	Requirements NodePoolRequirements `json:"requirements"`

	// Limits define maximum constraints
	Limits NodePoolLimits `json:"limits"`

	// Template defines labels and taints for nodes
	Template NodePoolTemplate `json:"template"`

	// Disruption defines when nodes can be disrupted
	Disruption NodePoolDisruption `json:"disruption"`

	// Weight determines priority when multiple NodePools match
	// +kubebuilder:default=1
	Weight int32 `json:"weight"`
}

// NodePoolRequirements defines constraints for node selection and the minimum
// instance size for nodes in this pool. Used to compute how many nodes are needed
// from unschedulable pod demand and to request recommendations from CloudBroker.
type NodePoolRequirements struct {
	// RegionConstraint specifies the region requirement: "EU" or "ANY"
	// +kubebuilder:validation:Enum=EU;ANY
	RegionConstraint string `json:"regionConstraint"`

	// Arch specifies allowed architectures
	// +kubebuilder:validation:MinItems=1
	Arch []string `json:"arch"`

	// MaxPriceEurPerHour is the maximum price per hour in EUR
	// +kubebuilder:validation:Type=number
	MaxPriceEurPerHour float64 `json:"maxPriceEurPerHour"`

	// AllowedProviders is an optional list of allowed cloud providers
	// +optional
	AllowedProviders []string `json:"allowedProviders,omitempty"`

	// MinVCPU is the minimum vCPU per node for this pool. Used for scaling math and CloudBroker requests.
	// When unset, defaults to 2.
	// +optional
	MinVCPU *int32 `json:"minVCPU,omitempty"`

	// MinRAMGb is the minimum RAM in GB per node for this pool. Used for scaling math and CloudBroker requests.
	// When unset, defaults to 8.
	// +optional
	// +kubebuilder:validation:Type=number
	MinRAMGb *float64 `json:"minRAMGb,omitempty"`
}

// NodePoolLimits defines maximum constraints
type NodePoolLimits struct {
	// MaxNodes is the maximum number of nodes allowed
	MaxNodes int32 `json:"maxNodes"`

	// MinNodes is the minimum number of ready nodes to keep when scaling down.
	// When unset, defaults to 1. Set to 0 to allow scaling to zero (e.g. for testing).
	// +optional
	MinNodes *int32 `json:"minNodes,omitempty"`
}

// NodePoolTemplate defines labels and taints for nodes
type NodePoolTemplate struct {
	// Labels to apply to nodes
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Taints to apply to nodes
	// +optional
	Taints []Taint `json:"taints,omitempty"`
}

// Taint represents a Kubernetes taint
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// NodePoolDisruption defines when nodes can be disrupted
type NodePoolDisruption struct {
	// TTLSecondsAfterEmpty is the time in seconds after a node becomes empty before it can be disrupted
	TTLSecondsAfterEmpty int32 `json:"ttlSecondsAfterEmpty"`

	// TTLSecondsUntilExpired is the time in seconds until a node expires
	TTLSecondsUntilExpired int32 `json:"ttlSecondsUntilExpired"`
}

// NodePoolStatus defines the observed state of NodePool
type NodePoolStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NodePool is the Schema for the nodepools API
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodePoolSpec   `json:"spec,omitempty"`
	Status NodePoolStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NodePoolList contains a list of NodePool
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodePool{}, &NodePoolList{})
}
