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

// NodeClaimSpec defines the desired state of NodeClaim
type NodeClaimSpec struct {
	// NodePoolRef is a reference to the NodePool
	NodePoolRef string `json:"nodePoolRef"`

	// NodeClassRef is a reference to the NodeClass
	NodeClassRef string `json:"nodeClassRef"`

	// Chosen contains the selected instance details
	Chosen InstanceDetails `json:"chosen"`
}

// InstanceDetails contains the selected instance information
type InstanceDetails struct {
	// Provider is the cloud provider
	Provider string `json:"provider"`

	// Region is the region where the instance is located
	Region string `json:"region"`

	// InstanceType is the instance type
	InstanceType string `json:"instanceType"`

	// VCPU is the number of vCPUs
	VCPU int32 `json:"vcpu"`

	// RamGb is the amount of RAM in GB
	// +kubebuilder:validation:Type=number
	RamGb float64 `json:"ramGb"`

	// PriceEurPerHour is the price per hour in EUR
	// +kubebuilder:validation:Type=number
	PriceEurPerHour float64 `json:"priceEurPerHour"`
}

// NodeClaimPhase represents the phase of a NodeClaim
// +kubebuilder:validation:Enum=Pending;Provisioning;Joining;Ready;Failed;Deleting
type NodeClaimPhase string

const (
	// NodeClaimPhasePending indicates the NodeClaim is pending
	NodeClaimPhasePending NodeClaimPhase = "Pending"

	// NodeClaimPhaseProvisioning indicates the instance is being provisioned
	NodeClaimPhaseProvisioning NodeClaimPhase = "Provisioning"

	// NodeClaimPhaseJoining indicates the node is joining the cluster
	NodeClaimPhaseJoining NodeClaimPhase = "Joining"

	// NodeClaimPhaseReady indicates the node is ready
	NodeClaimPhaseReady NodeClaimPhase = "Ready"

	// NodeClaimPhaseFailed indicates the NodeClaim has failed
	NodeClaimPhaseFailed NodeClaimPhase = "Failed"

	// NodeClaimPhaseDeleting indicates the NodeClaim is being deleted
	NodeClaimPhaseDeleting NodeClaimPhase = "Deleting"
)

// NodeClaimStatus defines the observed state of NodeClaim
type NodeClaimStatus struct {
	// Phase is the current phase of the NodeClaim
	Phase NodeClaimPhase `json:"phase"`

	// InstanceID is the cloud provider instance ID
	// +optional
	InstanceID string `json:"instanceID,omitempty"`

	// NodeName is the name of the Kubernetes node
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// LastError contains the last error message if any
	// +optional
	LastError string `json:"lastError,omitempty"`

	// Timestamps contains important timestamps
	Timestamps NodeClaimTimestamps `json:"timestamps,omitempty"`
}

// NodeClaimTimestamps contains important timestamps
type NodeClaimTimestamps struct {
	// CreatedAt is when the NodeClaim was created
	CreatedAt metav1.Time `json:"createdAt,omitempty"`

	// ReadyAt is when the NodeClaim became ready
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`

	// LastTransition is when the phase last changed
	LastTransition metav1.Time `json:"lastTransition,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="InstanceID",type="string",JSONPath=".status.instanceID"
//+kubebuilder:printcolumn:name="NodeName",type="string",JSONPath=".status.nodeName"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NodeClaim is the Schema for the nodeclaims API
type NodeClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeClaimSpec   `json:"spec,omitempty"`
	Status NodeClaimStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NodeClaimList contains a list of NodeClaim
type NodeClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeClaim{}, &NodeClaimList{})
}
