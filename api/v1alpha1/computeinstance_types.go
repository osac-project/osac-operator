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

// ComputeInstanceSpec defines the desired state of ComputeInstance
type ComputeInstanceSpec struct {
	// TemplateID is the unique identifier of the compute instance template to use when creating this compute instance
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=^[a-zA-Z_][a-zA-Z0-9._]*$
	TemplateID string `json:"templateID,omitempty"`
	// TemplateParameters is a JSON-encoded map of the parameter values for the
	// selected compute instance template.
	// +kubebuilder:validation:Optional
	TemplateParameters string `json:"templateParameters,omitempty"`

	// RestartRequestedAt is a timestamp signal to request a VM restart.
	//
	// Set this field to the current time (usually NOW) to request a restart.
	// The controller will execute the restart if this timestamp is greater than
	// status.lastRestartedAt.
	//
	// This is a declarative signal mechanism - the timestamp is a monotonically
	// increasing value to detect new restart requests, not a scheduled time.
	// Typically set to the current time for immediate restarts.
	//
	// External schedulers can set this field on a schedule to implement
	// scheduled maintenance windows if needed.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	RestartRequestedAt *metav1.Time `json:"restartRequestedAt,omitempty"`
}

// ComputeInstancePhaseType is a valid value for .status.phase
type ComputeInstancePhaseType string

const (
	// ComputeInstancePhaseProgressing means an update is in progress
	ComputeInstancePhaseProgressing ComputeInstancePhaseType = "Progressing"

	// ComputeInstancePhaseFailed means the compute instance deployment or update has failed
	ComputeInstancePhaseFailed ComputeInstancePhaseType = "Failed"

	// ComputeInstancePhaseReady means the compute instance and all associated resources are ready
	ComputeInstancePhaseReady ComputeInstancePhaseType = "Ready"

	// ComputeInstancePhaseDeleting means there has been a request to delete the ComputeInstance
	ComputeInstancePhaseDeleting ComputeInstancePhaseType = "Deleting"
)

// ComputeInstanceConditionType is a valid value for .status.conditions.type
type ComputeInstanceConditionType string

const (
	// ComputeInstanceConditionAccepted means the order has been accepted but work has not yet started
	ComputeInstanceConditionAccepted ComputeInstanceConditionType = "Accepted"

	// ComputeInstanceConditionProgressing means that an update is in progress
	ComputeInstanceConditionProgressing ComputeInstanceConditionType = "Progressing"

	// ComputeInstanceConditionAvailable means the compute instance is available
	ComputeInstanceConditionAvailable ComputeInstanceConditionType = "Available"

	// ComputeInstanceConditionDeleting means the compute instance is being deleted
	ComputeInstanceConditionDeleting ComputeInstanceConditionType = "Deleting"

	// ComputeInstanceConditionRestartInProgress indicates a restart is in progress
	ComputeInstanceConditionRestartInProgress ComputeInstanceConditionType = "RestartInProgress"

	// ComputeInstanceConditionRestartFailed indicates a restart request has failed
	ComputeInstanceConditionRestartFailed ComputeInstanceConditionType = "RestartFailed"
)

// VirtualMachineReferenceType contains a reference to the KubeVirt VirtualMachine CR created by this ComputeInstance
type VirtualMachineReferenceType struct {
	// Namespace that contains the VirtualMachine resources
	Namespace                  string `json:"namespace"`
	KubeVirtVirtualMachineName string `json:"kubeVirtVirtualMachineName"`
}

// TenantReferenceType contains a reference to the tenant that contains the ComputeInstance resources
type TenantReferenceType struct {
	// Name of the tenant
	Name string `json:"name"`
	// Namespace of the tenant
	Namespace string `json:"namespace"`
}

// ComputeInstanceStatus defines the observed state of ComputeInstance.
type ComputeInstanceStatus struct {
	// Phase provides a single-value overview of the state of the ComputeInstance
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Progressing;Failed;Ready;Deleting
	Phase ComputeInstancePhaseType `json:"phase,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the ComputeInstance
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// Reference to the KubeVirt VirtualMachine CR created by this ComputeInstance
	// +kubebuilder:validation:Optional
	VirtualMachineReference *VirtualMachineReferenceType `json:"virtualMachineReference,omitempty"`

	// Reference to the tenant that contains the ComputeInstance resources
	// +kubebuilder:validation:Optional
	TenantReference *TenantReferenceType `json:"tenantReference,omitempty"`

	// DesiredConfigVersion is the version (hash) of the desired configuration of the ComputeInstance
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	DesiredConfigVersion string `json:"desiredConfigVersion,omitempty"`

	// ReconciledConfigVersion is the version (hash) of the reconciled configuration of the ComputeInstance
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	ReconciledConfigVersion string `json:"reconciledConfigVersion,omitempty"`

	// LastRestartedAt records when the last restart was initiated by the controller.
	//
	// This is set to spec.restartRequestedAt when the controller processes a restart request.
	// It will be empty if no restart has been performed yet.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	LastRestartedAt *metav1.Time `json:"lastRestartedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ci
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.templateID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// ComputeInstance is the Schema for the computeinstances API
type ComputeInstance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ComputeInstance
	// +required
	Spec ComputeInstanceSpec `json:"spec"`

	// status defines the observed state of ComputeInstance
	// +optional
	Status ComputeInstanceStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ComputeInstanceList contains a list of ComputeInstance
type ComputeInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ComputeInstance `json:"items"`
}

// GetName returns the name of the ComputeInstance resource
func (ci *ComputeInstance) GetName() string {
	return ci.ObjectMeta.Name
}

func init() {
	SchemeBuilder.Register(&ComputeInstance{}, &ComputeInstanceList{})
}
