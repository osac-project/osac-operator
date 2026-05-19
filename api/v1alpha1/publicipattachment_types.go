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

// PublicIPAttachmentSpec defines the desired state of PublicIPAttachment.
// The entire spec is immutable after creation: to change the target, delete
// the attachment and create a new one.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable after creation"
type PublicIPAttachmentSpec struct {
	// PublicIP is the name of the PublicIP resource to attach.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PublicIP string `json:"publicIP"`

	// ComputeInstance is the name of the ComputeInstance to attach to.
	// Exactly one target field must be set.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	ComputeInstance *string `json:"computeInstance,omitempty"`
}

// PublicIPAttachmentPhaseType is a valid value for .status.phase
type PublicIPAttachmentPhaseType string

const (
	// PublicIPAttachmentPhaseProgressing means the attach or detach operation is in progress
	PublicIPAttachmentPhaseProgressing PublicIPAttachmentPhaseType = "Progressing"

	// PublicIPAttachmentPhaseFailed means the attach or detach operation has failed
	PublicIPAttachmentPhaseFailed PublicIPAttachmentPhaseType = "Failed"

	// PublicIPAttachmentPhaseReady means the public IP is attached and routing traffic
	PublicIPAttachmentPhaseReady PublicIPAttachmentPhaseType = "Ready"

	// PublicIPAttachmentPhaseDeleting means a detach operation is in progress
	PublicIPAttachmentPhaseDeleting PublicIPAttachmentPhaseType = "Deleting"
)

// PublicIPAttachmentStatus defines the observed state of PublicIPAttachment
type PublicIPAttachmentStatus struct {
	// Phase provides a single-value overview of the state of the PublicIPAttachment
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Progressing;Failed;Ready;Deleting
	Phase PublicIPAttachmentPhaseType `json:"phase,omitempty"`

	// DesiredConfigVersion is a hash of the spec, used to detect spec changes and control retry behavior.
	// +kubebuilder:validation:Optional
	DesiredConfigVersion string `json:"desiredConfigVersion,omitempty"`

	// Jobs holds an array of JobStatus tracking provisioning and deprovisioning operations
	// +kubebuilder:validation:Optional
	Jobs []JobStatus `json:"jobs,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the PublicIPAttachment
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=publicipattachment
// +kubebuilder:printcolumn:name="PublicIP",type=string,JSONPath=`.spec.publicIP`
// +kubebuilder:printcolumn:name="ComputeInstance",type=string,JSONPath=`.spec.computeInstance`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PublicIPAttachment is the Schema for the publicipattachments API
type PublicIPAttachment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of PublicIPAttachment
	// +required
	Spec PublicIPAttachmentSpec `json:"spec"`

	// status defines the observed state of PublicIPAttachment
	// +optional
	Status PublicIPAttachmentStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// PublicIPAttachmentList contains a list of PublicIPAttachment
type PublicIPAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PublicIPAttachment `json:"items"`
}

// GetName returns the name of the PublicIPAttachment resource
func (p *PublicIPAttachment) GetName() string {
	return p.ObjectMeta.Name
}
