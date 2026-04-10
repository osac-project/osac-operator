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

// PublicIPPoolConditionType is a valid value for .status.conditions.type
type PublicIPPoolConditionType string

const (
	// PublicIPPoolConditionConfigurationApplied indicates whether the controller
	// has processed the current spec version
	PublicIPPoolConditionConfigurationApplied PublicIPPoolConditionType = "ConfigurationApplied"
)

// GetPublicIPPoolStatusCondition returns the condition with the specified type from the PublicIPPool status
func GetPublicIPPoolStatusCondition(pool *PublicIPPool, conditionType PublicIPPoolConditionType) *metav1.Condition {
	return meta.FindStatusCondition(pool.Status.Conditions, string(conditionType))
}

// SetPublicIPPoolStatusCondition sets the condition with the specified type in the PublicIPPool status
func SetPublicIPPoolStatusCondition(pool *PublicIPPool, condition metav1.Condition) {
	meta.SetStatusCondition(&pool.Status.Conditions, condition)
}
