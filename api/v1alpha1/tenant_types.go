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

// TenantSpec defines the desired state of Tenant.
type TenantSpec struct {
	// DisplayName is the tenant's display name
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// EmailDomains is the list of email domains for tenant users
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
	EmailDomains []string `json:"emailDomains"`
}

type TenantPhaseType string

const (
	TenantPhaseProgressing TenantPhaseType = "Progressing"
	TenantPhaseReady       TenantPhaseType = "Ready"
	TenantPhaseFailed      TenantPhaseType = "Failed"
	TenantPhaseDeleting    TenantPhaseType = "Deleting"
)

// TenantConditionType is a valid value for .status.conditions.type
type TenantConditionType string

const (
	// TenantConditionNamespaceReady indicates whether the tenant namespace
	// exists on the target cluster.
	TenantConditionNamespaceReady TenantConditionType = "NamespaceReady"

	// TenantConditionStorageClassReady indicates whether a valid StorageClass
	// has been found for the tenant (tenant-specific or shared Default).
	TenantConditionStorageClassReady TenantConditionType = "StorageClassReady"
)

// Reason constants for Tenant conditions
const (
	TenantReasonFound         = "Found"
	TenantReasonNotFound      = "NotFound"
	TenantReasonMultipleFound = "MultipleFound"
)

// ResolvedStorageClass captures a single resolved StorageClass for a specific
// storage tier. The Tenant controller populates one entry per tier.
type ResolvedStorageClass struct {
	// Name is the name of the resolved Kubernetes StorageClass.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Tier is the storage tier this StorageClass provides,
	// taken from the osac.openshift.io/storage-tier label.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`
	Tier string `json:"tier"`
}

// TenantStatus defines the observed state of Tenant.
type TenantStatus struct {
	// Phase is the phase of the tenant
	Phase TenantPhaseType `json:"phase,omitempty"`

	// Namespace is the namespace allocated to the tenant on the target cluster
	Namespace string `json:"namespace,omitempty"`

	// StorageClasses lists all resolved StorageClass mappings for the tenant,
	// one per storage tier.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=tier
	StorageClasses []ResolvedStorageClass `json:"storageClasses,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the Tenant
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// Jobs holds the history of provisioning jobs triggered for this tenant
	Jobs []JobStatus `json:"jobs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DisplayName",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tenant is the Schema for the tenants API.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// GetStatusJobs returns the job history for provisioning provider integration.
func (t *Tenant) GetStatusJobs() []JobStatus {
	return t.Status.Jobs
}

// +kubebuilder:object:root=true

// TenantList contains a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

