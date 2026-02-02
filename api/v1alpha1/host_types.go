/*
Copyright 2025.

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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// HostSpec defines the desired state of Host
type HostSpec struct {
	// PowerState defines the desired power state of the host
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=HOST_POWER_STATE_UNSPECIFIED;HOST_POWER_STATE_ON;HOST_POWER_STATE_OFF
	PowerState HostPowerState `json:"powerState,omitempty"`
}

// HostPowerState is a valid value for .spec.powerState and .status.powerState
type HostPowerState string

const (
	// HostPowerStateUnspecified means we don't know the current power state
	HostPowerStateUnspecified HostPowerState = "HOST_POWER_STATE_UNSPECIFIED"

	// HostPowerStateOn means the host should be powered on
	HostPowerStateOn HostPowerState = "HOST_POWER_STATE_ON"

	// HostPowerStateOff means the host should be powered off
	HostPowerStateOff HostPowerState = "HOST_POWER_STATE_OFF"
)

// HostStateType represents the overall state of a host
type HostStateType string

const (
	// HostStateUnspecified indicates that the state is unknown
	HostStateUnspecified HostStateType = "Unspecified"

	// HostStateProgressing indicates that the host isn't ready yet
	HostStateProgressing HostStateType = "Progressing"

	// HostStateReady indicates that the host is ready
	HostStateReady HostStateType = "Ready"

	// HostStateFailed indicates that the host is unusable
	HostStateFailed HostStateType = "Failed"
)

// HostPhaseType is a valid value for .status.phase
type HostPhaseType string

const (
	// HostPhaseProgressing means an update is in progress
	HostPhaseProgressing HostPhaseType = "Progressing"

	// HostPhaseFailed means the host operation has failed
	HostPhaseFailed HostPhaseType = "Failed"

	// HostPhaseReady means the host is ready and operational
	HostPhaseReady HostPhaseType = "Ready"

	// HostPhaseDeleting means there has been a request to delete the Host
	HostPhaseDeleting HostPhaseType = "Deleting"
)

// HostConditionType is a valid value for .status.conditions.type
type HostConditionType string

const (
	// HostConditionAccepted means the host has been accepted but work has not yet started
	HostConditionAccepted HostConditionType = "Accepted"

	// HostConditionProgressing means that an update is in progress
	HostConditionProgressing HostConditionType = "Progressing"

	// HostConditionReady means the host is ready to use
	HostConditionReady HostConditionType = "Ready"

	// HostConditionFailed means the host is unusable
	HostConditionFailed HostConditionType = "Failed"

	// HostConditionAvailable means the host is available
	HostConditionAvailable HostConditionType = "Available"

	// HostConditionDeleting means the host is being deleted
	HostConditionDeleting HostConditionType = "Deleting"
)

// HostReferenceType contains a reference to the resources created by this Host
type HostReferenceType struct {
	// Namespace that contains the Host resources
	Namespace string `json:"namespace"`
	// HostPool that this host is assigned to, if any
	HostPool string `json:"hostPool,omitempty"`
}

// HostStatus defines the observed state of Host.
type HostStatus struct {
	// Phase provides a single-value overview of the state of the Host
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Progressing;Failed;Ready;Deleting
	Phase HostPhaseType `json:"phase,omitempty"`

	// State indicates the overall state of the host from the fulfillment service
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Unspecified;Progressing;Ready;Failed
	State HostStateType `json:"state,omitempty"`

	// PowerState reflects the current power state of the host
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=HOST_POWER_STATE_UNSPECIFIED;HOST_POWER_STATE_ON;HOST_POWER_STATE_OFF
	PowerState HostPowerState `json:"powerState,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the Host
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// Reference to the resources created for this Host
	// +kubebuilder:validation:Optional
	HostReference *HostReferenceType `json:"hostReference,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=h
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Power State",type=string,JSONPath=`.status.powerState`
// +kubebuilder:printcolumn:name="Host Pool",type=string,JSONPath=`.status.hostReference.hostPool`

// Host is the Schema for the hosts API
type Host struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Host
	// +required
	Spec HostSpec `json:"spec"`

	// status defines the observed state of Host
	// +optional
	Status HostStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// HostList contains a list of Host
type HostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Host `json:"items"`
}

// GetName returns the name of the Host resource
func (h *Host) GetName() string {
	return h.Name
}

func init() {
	SchemeBuilder.Register(&Host{}, &HostList{})
}
