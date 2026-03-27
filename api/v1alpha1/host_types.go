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

// HostSpec defines the desired state of Host.
type HostSpec struct {
	// HostClass is the resource class of the host.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="field is immutable"
	HostClass string `json:"hostClass"`
	// ID is the host ID from inventory (used by Host Management Operator as node identifier).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="oldSelf == '' || self == oldSelf",message="field is immutable once set"
	// +kubebuilder:validation:Format=uuid
	ID string `json:"id"`
	// Name is the host name from inventory.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="oldSelf == '' || self == oldSelf",message="field is immutable once set"
	Name string `json:"name"`
	// HostManagementClass is set by the Host Inventory Operator (e.g. openstack).
	HostManagementClass string `json:"hostManagementClass,omitempty"`
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
	// Online is the desired power state (true = on, false = off).
	Online bool `json:"online"`
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

// HostConditionType is a valid value for .status.conditions.type
type HostConditionType string

const (
	// HostConditionAllocated means the host has been allocated.
	HostConditionAllocated HostConditionType = "Allocated"

	// HostConditionAvailable means the host is available for provisioning.
	HostConditionAvailable HostConditionType = "Available"

	// HostConditionProvisioned means the host provisioning is complete.
	HostConditionProvisioned HostConditionType = "Provisioned"

	// HostConditionDeprovisioned means the host deprovisioning is complete.
	HostConditionDeprovisioned HostConditionType = "Deprovisioned"

	// HostConditionProvisionTemplateStart means the provision template has started.
	HostConditionProvisionTemplateStart HostConditionType = "ProvisionTemplateStart"

	// HostConditionProvisionTemplateComplete means the provision template run is complete.
	HostConditionProvisionTemplateComplete HostConditionType = "ProvisionTemplateComplete"

	// HostConditionProvisionTemplateSuccess means the provision template completed successfully.
	HostConditionProvisionTemplateSuccess HostConditionType = "ProvisionTemplateSuccess"

	// HostConditionProvisionTemplateFailed means the provision template failed.
	HostConditionProvisionTemplateFailed HostConditionType = "ProvisionTemplateFailed"

	// HostConditionDeprovisionTemplateStart means the deprovision template has started.
	HostConditionDeprovisionTemplateStart HostConditionType = "DeprovisionTemplateStart"

	// HostConditionDeprovisionTemplateComplete means the deprovision template run is complete.
	HostConditionDeprovisionTemplateComplete HostConditionType = "DeprovisionTemplateComplete"

	// HostConditionDeprovisionTemplateSuccess means the deprovision template completed successfully.
	HostConditionDeprovisionTemplateSuccess HostConditionType = "DeprovisionTemplateSuccess"

	// HostConditionDeprovisionTemplateFailed means the deprovision template failed.
	HostConditionDeprovisionTemplateFailed HostConditionType = "DeprovisionTemplateFailed"
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

// ProvisioningSpec holds desired provisioning parameters.
// +kubebuilder:validation:XValidation:rule="self.provisioningState != 'active' || (size(self.url) > 0 && size(self.provisioningNetwork) > 0)",message="when provisioningState is active, url and provisioningNetwork must be set"
type ProvisioningSpec struct {
	// ProvisioningState is the desired provisioning outcome: active (deployed) or available (in pool).
	// +kubebuilder:validation:Enum=active;available
	ProvisioningState string `json:"provisioningState,omitempty"`
	// URL is set when state is active for image-based provisioning.
	URL string `json:"url,omitempty"`
	// ProvisioningNetwork identifies the provisioning network.
	ProvisioningNetwork string `json:"provisioningNetwork,omitempty"`
}

// HostStatus defines the observed state of Host.
type HostStatus struct {
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

// GetPoolID returns the owning BareMetalPool UID if the Host is owned by a BareMetalPool.
func (h *Host) GetPoolID() (string, bool) {
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

// Host is the Schema for the hosts API.
type Host struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostSpec   `json:"spec,omitempty"`
	Status HostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HostList contains a list of Host.
type HostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Host `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Host{}, &HostList{})
}
