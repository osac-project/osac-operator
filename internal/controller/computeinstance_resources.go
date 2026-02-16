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

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac/osac-operator/api/v1alpha1"
)

// getTenant gets the tenant object from the cluster
// If the tenant is not found, return nil and no error
func (r *ComputeInstanceReconciler) getTenant(ctx context.Context, instance *v1alpha1.ComputeInstance) (*v1alpha1.Tenant, error) {
	if instance.GetTenantReferenceName() == "" || instance.GetTenantReferenceNamespace() == "" {
		// tenant reference is not set because it doesn't exist yet
		return nil, nil
	}

	tenant := &v1alpha1.Tenant{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.GetTenantReferenceNamespace(), Name: instance.GetTenantReferenceName()}, tenant)
	if err != nil {
		return nil, client.IgnoreNotFound(err)
	}

	return tenant, nil
}

// createOrUpdateTenant creates or updates the tenant object in the cluster in the namespace where the compute instance lives
func (r *ComputeInstanceReconciler) createOrUpdateTenant(ctx context.Context, instance *v1alpha1.ComputeInstance) error {
	tenantName, exists := instance.GetAnnotations()[cloudkitTenantAnnotation]
	if !exists || tenantName == "" {
		return fmt.Errorf("tenant name not found")
	}

	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getTenantObjectName(tenantName),
			Namespace: instance.GetNamespace(),
			Labels: map[string]string{
				"app.kubernetes.io/name": cloudkitAppName,
			},
		},
		Spec: v1alpha1.TenantSpec{
			Name: tenantName,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, tenant, func() error {
		err := controllerutil.SetOwnerReference(instance, tenant, r.Scheme)
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Update the tenant reference
	instance.SetTenantReferenceName(tenant.Name)
	instance.SetTenantReferenceNamespace(tenant.GetNamespace())
	return nil
}

func getTenantObjectName(tenantName string) string {
	return fmt.Sprintf("tenant-%s", encodeTenantName(tenantName))
}

func labelSelectorFromComputeInstanceInstance(instance *v1alpha1.ComputeInstance) client.MatchingLabels {
	return client.MatchingLabels{
		cloudkitComputeInstanceNameLabel: instance.GetName(),
	}
}
