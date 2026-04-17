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

// PublicIPSpec defines the desired state of PublicIP
type PublicIPSpec struct {
	// Pool is the name of the PublicIPPool this IP is allocated from.
	// This field is immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pool is immutable"
	Pool string `json:"pool"`

	// ComputeInstance is the optional name of the ComputeInstance this IP is attached to.
	// Setting this field triggers attachment of the IP to the referenced instance.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	ComputeInstance string `json:"computeInstance,omitempty"`
}

// PublicIPPhaseType is a valid value for .status.phase
type PublicIPPhaseType string

const (
	// PublicIPPhaseProgressing means an update is in progress
	PublicIPPhaseProgressing PublicIPPhaseType = "Progressing"

	// PublicIPPhaseFailed means the IP provisioning has failed
	PublicIPPhaseFailed PublicIPPhaseType = "Failed"

	// PublicIPPhaseReady means the IP and all associated resources are ready
	PublicIPPhaseReady PublicIPPhaseType = "Ready"

	// PublicIPPhaseDeleting means there has been a request to delete the PublicIP
	PublicIPPhaseDeleting PublicIPPhaseType = "Deleting"
)

// PublicIPStateType is a valid value for .status.state
type PublicIPStateType string

const (
	// PublicIPStatePending means the IP allocation is pending
	PublicIPStatePending PublicIPStateType = "Pending"

	// PublicIPStateAllocated means the IP has been allocated from the pool
	PublicIPStateAllocated PublicIPStateType = "Allocated"

	// PublicIPStateAttached means the IP is attached to a ComputeInstance
	PublicIPStateAttached PublicIPStateType = "Attached"

	// PublicIPStateReleasing means the IP is being released back to the pool
	PublicIPStateReleasing PublicIPStateType = "Releasing"

	// PublicIPStateFailed means provisioning or release failed
	PublicIPStateFailed PublicIPStateType = "Failed"
)

// PublicIPStatus defines the observed state of PublicIP
type PublicIPStatus struct {
	// Phase provides a single-value overview of the state of the PublicIP
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Progressing;Failed;Ready;Deleting
	Phase PublicIPPhaseType `json:"phase,omitempty"`

	// DesiredConfigVersion is a hash of the spec, used to detect spec changes and control retry behavior.
	// +kubebuilder:validation:Optional
	DesiredConfigVersion string `json:"desiredConfigVersion,omitempty"`

	// Jobs holds an array of JobStatus tracking provisioning and deprovisioning operations
	// +kubebuilder:validation:Optional
	Jobs []JobStatus `json:"jobs,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the PublicIP
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// Address is the allocated public IP address
	// +kubebuilder:validation:Optional
	Address string `json:"address,omitempty"`

	// State tracks the attachment lifecycle of the PublicIP
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Pending;Allocated;Attached;Releasing;Failed
	State PublicIPStateType `json:"state,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=publicip
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.pool`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PublicIP is the Schema for the publicips API
type PublicIP struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of PublicIP
	// +required
	Spec PublicIPSpec `json:"spec"`

	// status defines the observed state of PublicIP
	// +optional
	Status PublicIPStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// PublicIPList contains a list of PublicIP
type PublicIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PublicIP `json:"items"`
}

// GetName returns the name of the PublicIP resource
func (p *PublicIP) GetName() string {
	return p.ObjectMeta.Name
}

func init() {
	SchemeBuilder.Register(&PublicIP{}, &PublicIPList{})
}
