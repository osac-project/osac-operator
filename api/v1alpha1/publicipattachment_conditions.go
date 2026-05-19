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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PublicIPAttachmentConditionType is a valid value for .status.conditions.type
type PublicIPAttachmentConditionType string

const (
	// PublicIPAttachmentConditionConfigurationApplied indicates whether the controller
	// has processed the current spec version
	PublicIPAttachmentConditionConfigurationApplied PublicIPAttachmentConditionType = "ConfigurationApplied"
)

// GetPublicIPAttachmentStatusCondition returns the condition with the specified type from the PublicIPAttachment status
func GetPublicIPAttachmentStatusCondition(attachment *PublicIPAttachment, conditionType PublicIPAttachmentConditionType) *metav1.Condition {
	return meta.FindStatusCondition(attachment.Status.Conditions, string(conditionType))
}

// SetPublicIPAttachmentStatusCondition sets the condition with the specified type in the PublicIPAttachment status
func SetPublicIPAttachmentStatusCondition(attachment *PublicIPAttachment, condition metav1.Condition) {
	meta.SetStatusCondition(&attachment.Status.Conditions, condition)
}
