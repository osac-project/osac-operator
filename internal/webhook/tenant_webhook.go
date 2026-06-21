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

// Package webhook implements admission webhooks for OSAC custom resources.
package webhook

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-osac-openshift-io-v1alpha1-tenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=osac.openshift.io,resources=tenants,verbs=create;update,versions=v1alpha1,name=vtenant.kb.io,admissionReviewVersions=v1

// TenantValidator validates Tenant custom resources.
type TenantValidator struct{}

func (v *TenantValidator) ValidateCreate(_ context.Context, obj *v1alpha1.Tenant) (admission.Warnings, error) {
	allErrs := validateEmailDomains(obj.Spec.EmailDomains)
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

func (v *TenantValidator) ValidateUpdate(_ context.Context, _ *v1alpha1.Tenant, newObj *v1alpha1.Tenant) (admission.Warnings, error) {
	allErrs := validateEmailDomains(newObj.Spec.EmailDomains)
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

func (v *TenantValidator) ValidateDelete(_ context.Context, _ *v1alpha1.Tenant) (admission.Warnings, error) {
	return nil, nil
}

func validateEmailDomains(domains []string) field.ErrorList {
	fldPath := field.NewPath("spec", "emailDomains")
	var allErrs field.ErrorList

	for i, domain := range domains {
		if err := validateDNSDomain(domain); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i), domain, err.Error()))
		}
	}

	return allErrs
}

func validateDNSDomain(domain string) error {
	if domain == "" {
		return fmt.Errorf("domain must not be empty")
	}

	if len(domain) > 253 {
		return fmt.Errorf("domain must be at most 253 characters")
	}

	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return fmt.Errorf("domain must not start or end with a dot")
	}

	if strings.Contains(domain, "..") {
		return fmt.Errorf("domain must not contain consecutive dots")
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("domain must have at least two labels (e.g., example.com)")
	}

	for _, label := range labels {
		if err := validateDNSLabel(label); err != nil {
			return fmt.Errorf("invalid label %q: %w", label, err)
		}
	}

	return nil
}

func validateDNSLabel(label string) error {
	if len(label) == 0 {
		return fmt.Errorf("label must not be empty")
	}
	if len(label) > 63 {
		return fmt.Errorf("label must be at most 63 characters")
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("label must not start or end with a hyphen")
	}
	for _, c := range label {
		if !isValidLabelChar(c) {
			return fmt.Errorf("label contains invalid character %q", c)
		}
	}
	return nil
}

func isValidLabelChar(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
}
