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

package provisioning

import (
	"context"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

type contextKey int

const (
	tenantStorageClassesKey contextKey = iota
	adminKubeconfigKey
)

// WithTenantStorageClasses returns a context carrying the tenant's resolved
// storage classes. The AAP provider reads this when building extra_vars.
func WithTenantStorageClasses(ctx context.Context, scs []v1alpha1.ResolvedStorageClass) context.Context {
	return context.WithValue(ctx, tenantStorageClassesKey, scs)
}

// TenantStorageClassesFromContext retrieves the tenant storage classes from the
// context, or nil if not set.
func TenantStorageClassesFromContext(ctx context.Context) []v1alpha1.ResolvedStorageClass {
	scs, _ := ctx.Value(tenantStorageClassesKey).([]v1alpha1.ResolvedStorageClass)
	return scs
}

// WithAdminKubeconfig returns a context carrying the admin kubeconfig for a
// CaaS cluster. The AAP provider reads this when building extra_vars.
func WithAdminKubeconfig(ctx context.Context, kubeconfig string) context.Context {
	return context.WithValue(ctx, adminKubeconfigKey, kubeconfig)
}

// AdminKubeconfigFromContext retrieves the admin kubeconfig from the context,
// or empty string if not set.
func AdminKubeconfigFromContext(ctx context.Context) string {
	kc, _ := ctx.Value(adminKubeconfigKey).(string)
	return kc
}
