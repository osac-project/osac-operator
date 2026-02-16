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

	ovnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac/osac-operator/api/v1alpha1"
	"github.com/samber/lo"
)

func (r *TenantReconciler) createOrUpdateTenantNamespace(ctx context.Context, instance *v1alpha1.Tenant) error {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   generateTenantNamespaceName(instance),
			Labels: lo.Assign(commonLabelsFromTenant(instance), udnNamespaceLabel()),
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, namespace, func() error {
		ensureCommonLabelsForTenant(instance, namespace)
		return nil
	})

	instance.Status.Namespace = namespace.GetName()
	return err
}

func (r *TenantReconciler) createOrUpdateTenantUDN(ctx context.Context, instance *v1alpha1.Tenant) error {
	namespaceName := instance.Status.Namespace
	if namespaceName == "" {
		return fmt.Errorf("namespace not yet created for tenant %s", instance.GetName())
	}

	udn := &ovnv1.UserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      udnName,
			Namespace: namespaceName,
			Labels:    commonLabelsFromTenant(instance),
		},
		Spec: ovnv1.UserDefinedNetworkSpec{
			Topology: ovnv1.NetworkTopologyLayer2,
			Layer2: &ovnv1.Layer2Config{
				Role: ovnv1.NetworkRolePrimary,
				IPAM: &ovnv1.IPAMConfig{
					Lifecycle: ovnv1.IPAMLifecyclePersistent,
				},
				Subnets: []ovnv1.CIDR{
					"10.200.0.0/16",
				},
			},
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, udn, func() error {
		ensureCommonLabelsForTenant(instance, udn)
		return nil
	})

	return err
}

func (r *TenantReconciler) deleteTenantUDN(ctx context.Context, instance *v1alpha1.Tenant) (*ovnv1.UserDefinedNetwork, error) {
	namespaceName := instance.Status.Namespace
	if namespaceName == "" {
		// nothing to delete
		return nil, nil
	}

	udn := &ovnv1.UserDefinedNetwork{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: udnName, Namespace: namespaceName}, udn)
	if err != nil {
		return nil, client.IgnoreNotFound(err)
	}

	return udn, r.Client.Delete(ctx, udn)
}

func (r *TenantReconciler) deleteTenantNamespace(ctx context.Context, instance *v1alpha1.Tenant) (*corev1.Namespace, error) {
	namespaceName := instance.Status.Namespace
	if namespaceName == "" {
		// nothing to delete
		return nil, nil
	}

	namespace := &corev1.Namespace{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: namespaceName}, namespace)
	if err != nil {
		return nil, client.IgnoreNotFound(err)
	}

	if namespace.Status.Phase != corev1.NamespaceTerminating {
		err = r.Client.Delete(ctx, namespace)
	}

	return namespace, err
}

// udnNamespaceLabel returns the label that is used to notify OVN that a namespace that is part of a UDN.
func udnNamespaceLabel() map[string]string {
	return map[string]string{
		"k8s.ovn.org/primary-user-defined-network": "",
	}
}

func ensureCommonLabelsForTenant(instance *v1alpha1.Tenant, obj client.Object) {
	mergedLabels := lo.Assign(obj.GetLabels(), commonLabelsFromTenant(instance))
	if !lo.ElementsMatch(lo.Entries(obj.GetLabels()), lo.Entries(mergedLabels)) {
		obj.SetLabels(mergedLabels)
	}
}

func commonLabelsFromTenant(instance *v1alpha1.Tenant) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": cloudkitAppName,
		cloudkitTenantRefLabel:   instance.GetName(),
		cloudkitProjectRefLabel:  instance.GetNamespace(),
	}
}
