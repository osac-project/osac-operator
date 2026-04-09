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

// HostLeaseSpec defines the desired state of HostLease.
type HostLeaseSpec struct {
	// HostType is the resource class/type of the host.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="field is immutable"
	HostType string `json:"hostType"`
	// ExternalID is the host ID from external inventory (used by Host Management Operator as node identifier).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="oldSelf == '' || self == oldSelf",message="field is immutable once set"
	ExternalID string `json:"externalID"`
	// ExternalName is the host name from external inventory.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	ExternalName string `json:"externalName,omitempty"`
	// HostClass is host management backend class (e.g. openstack).
	HostClass string `json:"hostClass,omitempty"`
	// NetworkClass is the network class for this host (e.g. openstack).
	NetworkClass string `json:"networkClass,omitempty"`
	// Selector defines additional host selection filters.
	// hostSelector accepts arbitrary key/value selectors such as managedBy or topology.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="field is immutable"
	Selector HostSelectorSpec `json:"selector,omitempty"`
	// TemplateID is the unique identifier of the host template to use.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=^[a-zA-Z_][a-zA-Z0-9._]*$
	TemplateID string `json:"templateID"`
	// TemplateParameters is a JSON-encoded map of the parameter values for the
	// selected host template.
	// +kubebuilder:validation:Optional
	TemplateParameters string `json:"templateParameters,omitempty"`
	// PoweredOn is the desired power state (true = powered on, false = powered off).
	// This represents the user's intent; the actual observed state is in status.poweredOn.
	PoweredOn bool `json:"poweredOn"`
	// NetworkInterfaces lists the host's network interfaces with desired network binding.
	NetworkInterfaces []NetworkInterfaceSpec `json:"networkInterfaces,omitempty"`
	// Provisioning holds the desired host state and
	// when active, image-based URL and provisioning network (e.g. external).
	Provisioning *ProvisioningSpec `json:"provisioning,omitempty"`
}

// Provisioning state values for spec.provisioning.provisioningState.
const (
	// ProvisioningStateActive means the host is fully provisioned.
	ProvisioningStateActive = "active"
	// ProvisioningStateAvailable means the host is available to be provisioned.
	ProvisioningStateAvailable = "available"
)

// HostLeaseConditionType is a valid value for .status.conditions.type
type HostLeaseConditionType string

const (
	// HostConditionAllocated means the host has been allocated.
	HostConditionAllocated HostLeaseConditionType = "Allocated"

	// HostConditionAvailable means the host is available for provisioning.
	HostConditionAvailable HostLeaseConditionType = "Available"

	// HostConditionPowerSynced tracks the host power synchronization state.
	// Set condition status to True and reason to PowerOn when power on is successful.
	// Set condition status to True and reason to PowerOff when power off is successful.
	// Set condition status to False and reason to IronicAPIFailure when the operation fails.
	HostConditionPowerSynced HostLeaseConditionType = "PowerSynced"

	// HostConditionProvisioned means the host provisioning is complete.
	HostConditionProvisioned HostLeaseConditionType = "Provisioned"

	// HostConditionDeprovisioned means the host deprovisioning is complete.
	HostConditionDeprovisioned HostLeaseConditionType = "Deprovisioned"

	// HostConditionProvisionTemplateComplete tracks provision template completion.
	// Set condition status True on success.
	// Set condition status False with reason Progressing or TemplateFailed while not complete.
	HostConditionProvisionTemplateComplete HostLeaseConditionType = "ProvisionTemplateComplete"

	// HostConditionDeprovisionTemplateComplete tracks deprovision template completion.
	// Set condition status True on success.
	// Set condition status False with reason Progressing or TemplateFailed while not complete.
	HostConditionDeprovisionTemplateComplete HostLeaseConditionType = "DeprovisionTemplateComplete"
)

// Host condition reason values
const (
	// HostConditionReasonProgressing indicates the template workflow is still running.
	HostConditionReasonProgressing = "Progressing"

	// HostConditionReasonTemplateFailed indicates the template workflow failed.
	HostConditionReasonTemplateFailed = "TemplateFailed"

	// HostConditionReasonPowerOn indicates the host is powered on successfully.
	HostConditionReasonPowerOn = "PowerOn"

	// HostConditionReasonPowerOff indicates the host is powered off successfully.
	HostConditionReasonPowerOff = "PowerOff"

	// HostConditionReasonIronicAPIFailure indicates a power operation failed due to Ironic API error.
	HostConditionReasonIronicAPIFailure = "IronicAPIFailure"
)

// HostSelectorSpec defines additional host selection constraints.
type HostSelectorSpec struct {
	// HostSelector is a map of arbitrary selector key/value pairs
	// (for example managedBy, topology, rack, zone).
	// +kubebuilder:validation:Optional
	HostSelector map[string]string `json:"hostSelector,omitempty"`
}

// NetworkInterfaceSpec describes a desired network interface and its network binding.
type NetworkInterfaceSpec struct {
	// MACAddress is the interface MAC address.
	MACAddress string `json:"macAddress,omitempty"`
	// Network is the network to attach this interface to (e.g. private-vlan-network).
	Network string `json:"network,omitempty"`
}

// HostLeaseImageSpec holds image provisioning details.
type HostLeaseImageSpec struct {
	// URL is the image location used for image-based provisioning.
	URL string `json:"url,omitempty"`
	// ProviderOptions is a free-form map of provider-specific image options, such as checksum.
	ProviderOptions map[string]string `json:"providerOptions,omitempty"`
}

// ProvisioningSpec holds desired provisioning parameters.
// +kubebuilder:validation:XValidation:rule="self.provisioningState != 'active' || (size(self.imageSpec.url) > 0 && size(self.provisioningNetwork) > 0)",message="when provisioningState is active, imageSpec.url and provisioningNetwork must be set"
type ProvisioningSpec struct {
	// ProvisioningState is the desired provisioning outcome: active (deployed) or available (in pool).
	// +kubebuilder:validation:Enum=active;available
	ProvisioningState string `json:"provisioningState,omitempty"`
	// ImageSpec contains image source URL and provider-specific options.
	ImageSpec HostLeaseImageSpec `json:"imageSpec,omitempty"`
	// ProvisioningNetwork is the network reference used by
	// BareMetalPool External Provisioning Profile (for example, a Neutron network name).
	ProvisioningNetwork string `json:"provisioningNetwork,omitempty"`
}

// HostLeaseStatus defines the observed state of HostLease.
type HostLeaseStatus struct {
	// Jobs tracks the history of provision and deprovision operations
	// Ordered chronologically, with latest operations at the end
	// Limited to the last N jobs (configurable via OSAC_MAX_JOB_HISTORY, default 10)
	// +kubebuilder:validation:Optional
	Jobs []JobStatus `json:"jobs,omitempty"`
	// Conditions holds an array of metav1.Condition describing host state.
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	// PoweredOn is the current power state.
	PoweredOn *bool `json:"poweredOn,omitempty"`
	// NetworkInterfaces lists the host's network interfaces (from inventory or observed).
	NetworkInterfaces []NetworkInterfaceStatus `json:"networkInterfaces,omitempty"`
	// Provisioning holds current provisioning URL and state from the backend.
	Provisioning ProvisionStatus `json:"provisioning,omitempty"`
}

// NetworkInterfaceStatus describes an observed network interface.
type NetworkInterfaceStatus struct {
	// MACAddress is the interface MAC address.
	MACAddress string `json:"macAddress,omitempty"`
}

// ProvisionStatus holds current provisioning state from the bare metal management provider.
type ProvisionStatus struct {
	// URL is the URL of the currently provisioned image.
	URL string `json:"url,omitempty"`
	// ProvisioningState is the current provisioning state (e.g. active).
	ProvisioningState string `json:"provisioningState,omitempty"`
}

// GetPoolID returns the owning BareMetalPool UID if the HostLease is owned by a BareMetalPool.
func (h *HostLease) GetPoolID() (string, bool) {
	for _, ownerReference := range h.OwnerReferences {
		if ownerReference.Controller == nil || !*ownerReference.Controller {
			continue
		}
		if ownerReference.APIVersion == h.APIVersion && ownerReference.Kind == "BareMetalPool" {
			return string(ownerReference.UID), true
		}
	}
	return "", false
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// HostLease is the Schema for the hostleases API.
type HostLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostLeaseSpec   `json:"spec,omitempty"`
	Status HostLeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HostLeaseList contains a list of HostLease.
type HostLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostLease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostLease{}, &HostLeaseList{})
}
