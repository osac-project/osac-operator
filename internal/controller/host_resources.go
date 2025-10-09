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

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
)

func (r *HostReconciler) newHostNamespace(ctx context.Context, instance *v1alpha1.Host) (*appResource, error) {
	log := ctrllog.FromContext(ctx)

	var namespaceList corev1.NamespaceList
	var namespaceName string

	if err := r.List(ctx, &namespaceList, labelSelectorFromHostInstance(instance)); err != nil {
		log.Error(err, "failed to list namespaces")
		return nil, err
	}

	if len(namespaceList.Items) > 1 {
		return nil, fmt.Errorf("found multiple matching namespaces for %s", instance.GetName())
	}

	if len(namespaceList.Items) == 0 {
		namespaceName = generateHostNamespaceName(instance)
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
			Labels: commonLabelsFromHost(instance),
		},
	}

	mutateFn := func() error {
		ensureCommonLabelsForHost(instance, namespace)
		instance.SetHostReferenceNamespace(namespaceName)
		return nil
	}

	return &appResource{
		namespace,
		mutateFn,
	}, nil
}

func ensureCommonLabelsForHost(instance *v1alpha1.Host, obj client.Object) {
	requiredLabels := commonLabelsFromHost(instance)
	objLabels := obj.GetLabels()
	if objLabels == nil {
		objLabels = make(map[string]string)
	}
	maps.Copy(objLabels, requiredLabels)
	obj.SetLabels(objLabels)
}

func commonLabelsFromHost(instance *v1alpha1.Host) map[string]string {
	key := client.ObjectKeyFromObject(instance)
	return map[string]string{
		"app.kubernetes.io/name": cloudkitAppName,
		cloudkitHostNameLabel:    key.Name,
	}
}

func labelSelectorFromHostInstance(instance *v1alpha1.Host) client.MatchingLabels {
	return client.MatchingLabels{
		cloudkitHostNameLabel: instance.GetName(),
	}
}
