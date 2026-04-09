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

// BareMetalPoolSpec defines the desired state of BareMetalPool.
type BareMetalPoolSpec struct {
	// HostSets defines the number of hosts needed for each host class.
	// +kubebuilder:validation:Required
	// +listType=map
	// +listMapKey=hostClass
	// +kubebuilder:validation:items:XValidation:rule="self.replicas > 0"
	HostSets []BareMetalHostSet `json:"hostSets"`

	// Profile specifies configuration for the bare metal pool, such as setup jobs
	// and setting additional metadata labels
	// +kubebuilder:validation:Optional
	Profile *ProfileSpec `json:"profile,omitempty"`
}

// BareMetalPoolStatus defines the observed state of BareMetalPool.
type BareMetalPoolStatus struct {
	// HostSets shows the current allocation of hosts
	// +kubebuilder:validation:Optional
	HostSets []BareMetalHostSet `json:"hostSets,omitempty"`

	// Jobs tracks the history of provision and deprovision operations
	// Ordered chronologically, with latest operations at the end
	// Limited to the last N jobs (configurable via OSAC_MAX_JOB_HISTORY, default 10)
	// +kubebuilder:validation:Optional
	Jobs []JobStatus `json:"jobs,omitempty"`

	// LastUpdated is the timestamp when the status was last updated
	// +kubebuilder:validation:Optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Conditions represent the latest available observations of the BareMetalPool state
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

type BareMetalHostSet struct {
	// HostClass specifies the class of the host
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	HostType string `json:"hostClass"`

	// Replicas specifies the number of hosts required for this host class
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
}

// ProfileSpec defines a name and input associated with a profile
type ProfileSpec struct {
	// Name is the name of the profile
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// TemplateParameters is a JSON-encoded map of the parameter values for the
	// selected profile.
	// +kubebuilder:validation:Optional
	TemplateParameters string `json:"templateParameters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bmp;bmpool
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type == 'Ready')].reason"
// +kubebuilder:printcolumn:name="Profile",type="string",JSONPath=".spec.profile.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// BareMetalPool is the Schema for the baremetalpools API.
type BareMetalPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BareMetalPoolSpec   `json:"spec,omitempty"`
	Status BareMetalPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BareMetalPoolList contains a list of BareMetalPool.
type BareMetalPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BareMetalPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BareMetalPool{}, &BareMetalPoolList{})
}
