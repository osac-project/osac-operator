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

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

func newTenant(domains []string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tenant",
			Namespace: "default",
		},
		Spec: v1alpha1.TenantSpec{
			DisplayName:  "Test Tenant",
			EmailDomains: domains,
		},
	}
}

func TestValidateCreate_ValidDomains(t *testing.T) {
	tests := []struct {
		name    string
		domains []string
	}{
		{"simple domain", []string{"example.com"}},
		{"subdomain", []string{"sub.example.com"}},
		{"deep subdomain", []string{"a.b.c.example.com"}},
		{"hyphenated domain", []string{"my-company.com"}},
		{"hyphenated subdomain", []string{"my-sub.my-company.com"}},
		{"multiple valid domains", []string{"example.com", "example.org"}},
		{"numeric labels", []string{"123.example.com"}},
		{"single char labels", []string{"a.b.com"}},
		{"long tld", []string{"example.museum"}},
	}

	v := &TenantValidator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := newTenant(tt.domains)
			warnings, err := v.ValidateCreate(context.Background(), tenant)
			if err != nil {
				t.Errorf("expected no error for domains %v, got: %v", tt.domains, err)
			}
			if len(warnings) > 0 {
				t.Errorf("expected no warnings, got: %v", warnings)
			}
		})
	}
}

func TestValidateCreate_InvalidDomains(t *testing.T) {
	tests := []struct {
		name    string
		domains []string
		wantMsg string
	}{
		{"no TLD", []string{"example"}, "example"},
		{"leading dot", []string{".example.com"}, ".example.com"},
		{"trailing dot", []string{"example.com."}, "example.com."},
		{"consecutive dots", []string{"ex..ample.com"}, "ex..ample.com"},
		{"space in domain", []string{"exam ple.com"}, "exam ple.com"},
		{"at sign", []string{"user@example.com"}, "user@example.com"},
		{"wildcard", []string{"*.example.com"}, "*.example.com"},
		{"label starts with hyphen", []string{"-example.com"}, "-example.com"},
		{"label ends with hyphen", []string{"example-.com"}, "example-.com"},
		{"empty string", []string{""}, ""},
		{"underscore", []string{"my_domain.com"}, "my_domain.com"},
		{"exclamation mark", []string{"bad!.com"}, "bad!.com"},
		{"label too long", []string{strings.Repeat("a", 64) + ".com"}, "at most 63 characters"},
		{"domain too long total", []string{strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 63) + ".com"}, "253"},
	}

	v := &TenantValidator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := newTenant(tt.domains)
			_, err := v.ValidateCreate(context.Background(), tenant)
			if err == nil {
				t.Errorf("expected error for domains %v, got nil", tt.domains)
				return
			}
			if tt.wantMsg != "" {
				errStr := err.Error()
				if !strings.Contains(errStr, tt.wantMsg) {
					t.Errorf("error message should contain %q, got: %s", tt.wantMsg, errStr)
				}
			}
		})
	}
}

func TestValidateCreate_MultipleInvalidDomains(t *testing.T) {
	v := &TenantValidator{}
	tenant := newTenant([]string{"valid.com", "-bad.com", "also bad!.com"})
	_, err := v.ValidateCreate(context.Background(), tenant)
	if err == nil {
		t.Fatal("expected error for mixed valid/invalid domains, got nil")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "-bad.com") {
		t.Errorf("error should mention -bad.com, got: %s", errStr)
	}
	if !strings.Contains(errStr, "also bad!.com") {
		t.Errorf("error should mention also bad!.com, got: %s", errStr)
	}
}

func TestValidateUpdate_ValidDomains(t *testing.T) {
	v := &TenantValidator{}
	oldTenant := newTenant([]string{"old.com"})
	newTenant := newTenant([]string{"new.com", "sub.new.com"})
	warnings, err := v.ValidateUpdate(context.Background(), oldTenant, newTenant)
	if err != nil {
		t.Errorf("expected no error for valid update, got: %v", err)
	}
	if len(warnings) > 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateUpdate_InvalidDomains(t *testing.T) {
	v := &TenantValidator{}
	oldTenant := newTenant([]string{"old.com"})
	newTenant := newTenant([]string{"-invalid.com"})
	_, err := v.ValidateUpdate(context.Background(), oldTenant, newTenant)
	if err == nil {
		t.Error("expected error for invalid update domains, got nil")
	}
}

func TestValidateUpdate_UnchangedDomains(t *testing.T) {
	v := &TenantValidator{}
	tenant := newTenant([]string{"example.com"})
	warnings, err := v.ValidateUpdate(context.Background(), tenant, tenant)
	if err != nil {
		t.Errorf("expected no error for unchanged domains, got: %v", err)
	}
	if len(warnings) > 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateDelete_AlwaysSucceeds(t *testing.T) {
	v := &TenantValidator{}
	tenant := newTenant([]string{"example.com"})
	warnings, err := v.ValidateDelete(context.Background(), tenant)
	if err != nil {
		t.Errorf("expected no error for delete, got: %v", err)
	}
	if len(warnings) > 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}
