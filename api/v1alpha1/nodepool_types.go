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
	// Deprecated: Architecture is now extracted from pod nodeSelector/nodeAffinity (kubernetes.io/arch)
	// and passed to CloudBroker. This field is ignored by the controller.
	// +kubebuilder:validation:MinItems=1
	Arch []string `json:"arch"`

	// MaxPriceEurPerHour is the maximum price per hour in EUR
	// +kubebuilder:validation:Type=number
	MaxPriceEurPerHour float64 `json:"maxPriceEurPerHour"`

	// AllowedProviders is an optional list of allowed cloud providers
	// +optional
	AllowedProviders []string `json:"allowedProviders,omitempty"`

	// ExcludedInstanceTypes excludes specific instance types from CloudBroker recommendations.
	// Useful when certain VM families have no quota (e.g. Azure B-series with 0 cores).
	// +optional
	ExcludedInstanceTypes []string `json:"excludedInstanceTypes,omitempty"`

	// ExcludedInstanceTypePrefixes excludes instance types whose name starts with any prefix.
	// E.g. "Standard_B" excludes all Azure B-series (which often have 0 quota in free-tier subs).
	// +optional
	ExcludedInstanceTypePrefixes []string `json:"excludedInstanceTypePrefixes,omitempty"`

	// TCOPlacement defines placement affinity for TCO-aware recommendations.
	// When set, CloudBroker factors in egress costs when ranking instances.
	// +optional
	TCOPlacement *TCOPlacement `json:"tcoPlacement,omitempty"`
}

// TCOPlacement defines placement affinity for TCO-aware recommendations.
// When data source and estimated egress are provided, cheaper instances in other
// clouds may be deprioritized if cross-cloud transfer costs outweigh savings.
type TCOPlacement struct {
	// DataSourceProvider is the provider where data/dependencies live (e.g. "gcp").
	// +optional
	DataSourceProvider string `json:"dataSourceProvider,omitempty"`

	// DataSourceRegion is the region where data/dependencies live (e.g. "us-central1").
	// +optional
	DataSourceRegion string `json:"dataSourceRegion,omitempty"`

	// EstimatedEgressGbPerHour is the expected egress from the new instance in GB/hour.
	// +optional
	EstimatedEgressGbPerHour float64 `json:"estimatedEgressGbPerHour,omitempty"`

	// MaxTCOEurPerHour is the maximum total cost of ownership (instance + egress) in EUR/hour.
	// When set with TCO placement, CloudBroker filters by tco <= max instead of only instance price.
	// +optional
	MaxTCOEurPerHour float64 `json:"maxTCOEurPerHour,omitempty"`
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
	// Conditions represent the latest available observations of the NodePool's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
