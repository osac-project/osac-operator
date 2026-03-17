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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

var _ = Describe("Tenant Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		tenant := &v1alpha1.Tenant{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Tenant")
			err := k8sClient.Get(ctx, typeNamespacedName, tenant)
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha1.Tenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: v1alpha1.TenantSpec{},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &v1alpha1.Tenant{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Tenant")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			controllerReconciler := NewTenantReconciler(testMcManager, "default", mcmanager.LocalCluster)

			By("waiting for the Tenant to appear in the controller's cache")
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, typeNamespacedName, &v1alpha1.Tenant{})
			}, 5*time.Second, 10*time.Millisecond).Should(Succeed())

			By("reconciling when namespace does not exist - status becomes Progressing")
			_, _ = controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{
				NamespacedName: typeNamespacedName,
			}})
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
				g.Expect(tenant.Status.Phase).To(Equal(v1alpha1.TenantPhaseProgressing))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			By("creating the namespace on the target cluster (controller only observes it)")
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: resourceName,
				},
			}
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, namespace)
			})

			By("reconciling until tenant is Ready and status.namespace is set")
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{
					NamespacedName: typeNamespacedName,
				}})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
				g.Expect(tenant.Status.Phase).To(Equal(v1alpha1.TenantPhaseReady))
				g.Expect(tenant.Status.Namespace).To(Equal(resourceName))
			}).Should(Succeed())
		})
	})
})
