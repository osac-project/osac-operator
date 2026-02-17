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

// Important: Run "make" to regenerate code after modifying this file

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostPoolSpec defines the desired state of HostPool
type HostPoolSpec struct {
	// HostSets defines the types of hosts and number of each type of host that will be allocated
	// for this host pool
	// +kubebuilder:validation:Required
	HostSets []HostSet `json:"hostSets,omitempty"`
}

type HostSet struct {
	// HostClass describes the type of host you are requesting
	// +kubebuilder:validation:Required
	HostClass string `json:"hostClass"`
	// Size describes the number of hosts you want of the given host class
	// +kubebuilder:validation:Required
	Size int `json:"size"`
}

// HostPoolPhaseType is a valid value for .status.phase
type HostPoolPhaseType string

const (
	// HostPoolPhaseProgressing means an update is in progress
	HostPoolPhaseProgressing HostPoolPhaseType = "Progressing"

	// HostPoolPhaseFailed means the host pool deployment or update has failed
	HostPoolPhaseFailed HostPoolPhaseType = "Failed"

	// HostPoolPhaseReady means the host pool and all associated resources are ready
	HostPoolPhaseReady HostPoolPhaseType = "Ready"

	// HostPoolPhaseDeleting means there has been a request to delete the HostPool
	HostPoolPhaseDeleting HostPoolPhaseType = "Deleting"
)

// HostPoolConditionType is a valid value for .status.conditions.type
type HostPoolConditionType string

const (
	// HostPoolConditionAccepted means the order has been accepted but work has not yet started
	HostPoolConditionAccepted HostPoolConditionType = "Accepted"

	// HostPoolConditionProgressing means that an update is in progress
	HostPoolConditionProgressing HostPoolConditionType = "Progressing"

	// HostPoolConditionAvailable means the host pool is available
	HostPoolConditionAvailable HostPoolConditionType = "Available"

	// HostPoolConditionDeleting means the host pool is being deleted
	HostPoolConditionDeleting HostPoolConditionType = "Deleting"
)

// HostPoolReferenceType contains a reference to the resources created by this HostPool
type HostPoolReferenceType struct {
	// Namespace that contains the HostPool resources
	Namespace string `json:"namespace"`
}

// HostPoolStatus defines the observed state of HostPool.
type HostPoolStatus struct {
	// Phase provides a single-value overview of the state of the HostPool
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Progressing;Failed;Ready;Deleting
	Phase HostPoolPhaseType `json:"phase,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the HostPool
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// Reference to the resources created for this HostPool
	// +kubebuilder:validation:Optional
	HostPoolReference *HostPoolReferenceType `json:"hostPoolReference,omitempty"`

	// HostSets reflects how many hosts are currently allocated for each host class
	HostSets []HostSet `json:"hostSets,omitempty"`

	// Jobs tracks the history of provision and deprovision operations
	// Ordered chronologically, with latest operations at the end
	// Limited to the last N jobs (configurable via CLOUDKIT_MAX_JOB_HISTORY, default 10)
	// +kubebuilder:validation:Optional
	Jobs []JobStatus `json:"jobs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hp
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Host Sets",type=integer,JSONPath=`.spec.hostSets[*].size`

// HostPool is the Schema for the hostpools API
type HostPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of HostPool
	// +required
	Spec HostPoolSpec `json:"spec"`

	// status defines the observed state of HostPool
	// +optional
	Status HostPoolStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// HostPoolList contains a list of HostPool
type HostPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostPool `json:"items"`
}

// GetName returns the name of the HostPool resource
func (hp *HostPool) GetName() string {
	return hp.ObjectMeta.Name
}

func init() {
	SchemeBuilder.Register(&HostPool{}, &HostPoolList{})
}
