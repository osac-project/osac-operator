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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac/osac-operator/api/v1alpha1"
	ovnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
					Spec: v1alpha1.TenantSpec{
						Name: "my-tenant",
					},
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

			// envtest doesn't delete namespaces
			// https://book.kubebuilder.io/reference/envtest.html#namespace-usage-limitation
			By("Reconciling until namespace is terminating")
			Eventually(func(g Gomega) {
				controllerReconciler := NewTenantReconciler(k8sClient, k8sClient.Scheme(), "default")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				g.Expect(err).NotTo(HaveOccurred())

				namespace := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Status.Namespace}, namespace)).To(Succeed())
				g.Expect(namespace.Status.Phase).To(Equal(corev1.NamespaceTerminating))
			}).Should(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			controllerReconciler := NewTenantReconciler(k8sClient, k8sClient.Scheme(), "default")

			By("reconciling until tenant is ready")
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
				g.Expect(tenant.Status.Phase).To(Equal(v1alpha1.TenantPhaseReady))
			}).Should(Succeed())

			By("checking that the finalizer was added")
			Expect(tenant.Finalizers).To(ContainElement("cloudkit.openshift.io/tenant"))

			By("checking that the namespace was created")
			Expect(tenant.Status.Namespace).NotTo(BeEmpty())
			namespace := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tenant.Status.Namespace}, namespace)).To(Succeed())

			By("checking that the UDN was created")
			udn := &ovnv1.UserDefinedNetwork{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: tenant.Status.Namespace, Name: udnName}, udn)).To(Succeed())
		})
	})
})
