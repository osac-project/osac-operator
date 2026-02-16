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
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac/osac-operator/api/v1alpha1"
)

func (r *HostPoolReconciler) newHostPoolNamespace(ctx context.Context, instance *v1alpha1.HostPool) (*appResource, error) {
	log := ctrllog.FromContext(ctx)

	var namespaceList corev1.NamespaceList
	var namespaceName string

	if err := r.List(ctx, &namespaceList, labelSelectorFromHostPoolInstance(instance)); err != nil {
		log.Error(err, "failed to list namespaces")
		return nil, err
	}

	if len(namespaceList.Items) > 1 {
		return nil, fmt.Errorf("found multiple matching namespaces for %s", instance.GetName())
	}

	if len(namespaceList.Items) == 0 {
		namespaceName = generateHostPoolNamespaceName(instance)
		if namespaceName == "" {
			return nil, fmt.Errorf("failed to generate namespace name")
		}
	} else {
		namespaceName = namespaceList.Items[0].GetName()
	}

	namespace := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespaceName,
			Labels: commonLabelsFromHostPool(instance),
		},
	}

	mutateFn := func() error {
		ensureCommonLabelsForHostPool(instance, namespace)
		instance.SetHostPoolReferenceNamespace(namespaceName)
		return nil
	}

	return &appResource{
		namespace,
		mutateFn,
	}, nil
}

func ensureCommonLabelsForHostPool(instance *v1alpha1.HostPool, obj client.Object) {
	requiredLabels := commonLabelsFromHostPool(instance)
	objLabels := obj.GetLabels()
	if objLabels == nil {
		objLabels = make(map[string]string)
	}
	maps.Copy(objLabels, requiredLabels)
	obj.SetLabels(objLabels)
}

func commonLabelsFromHostPool(instance *v1alpha1.HostPool) map[string]string {
	key := client.ObjectKeyFromObject(instance)
	return map[string]string{
		"app.kubernetes.io/name":  cloudkitAppName,
		cloudkitHostPoolNameLabel: key.Name,
	}
}

func labelSelectorFromHostPoolInstance(instance *v1alpha1.HostPool) client.MatchingLabels {
	return client.MatchingLabels{
		cloudkitHostPoolNameLabel: instance.GetName(),
	}
}

// Note: HostPool reference helper functions are now provided as methods on the HostPool type
// in api/v1alpha1/hostpool_hostpoolreference.go for consistency with ComputeInstance pattern.
