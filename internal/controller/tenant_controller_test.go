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
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

		It("should transition through all Ready/Progressing phases with conditions", func() {
			fakeRecorder := record.NewFakeRecorder(10)
			controllerReconciler := NewTenantReconciler(testMcManager, "default", mcmanager.LocalCluster)
			controllerReconciler.Recorder = fakeRecorder

			By("waiting for the Tenant to appear in the controller's cache")
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, typeNamespacedName, &v1alpha1.Tenant{})
			}, 5*time.Second, 10*time.Millisecond).Should(Succeed())

			doReconcile := func() error {
				_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{
					NamespacedName: typeNamespacedName,
				}})
				return err
			}
			assertStatus := func(
				expectedPhase v1alpha1.TenantPhaseType,
				expectedSC string,
				expectedNSStatus metav1.ConditionStatus,
				expectedNSReason string,
				expectedSCStatus metav1.ConditionStatus,
				expectedSCReason string,
			) {
				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
					g.Expect(tenant.Status.Phase).To(Equal(expectedPhase))
					g.Expect(tenant.Status.StorageClass).To(Equal(expectedSC))

					nsCond := tenant.GetStatusCondition(v1alpha1.TenantConditionNamespaceReady)
					g.Expect(nsCond).NotTo(BeNil())
					g.Expect(nsCond.Status).To(Equal(expectedNSStatus))
					g.Expect(nsCond.Reason).To(Equal(expectedNSReason))

					scCond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageClassReady)
					g.Expect(scCond).NotTo(BeNil())
					g.Expect(scCond.Status).To(Equal(expectedSCStatus))
					g.Expect(scCond.Reason).To(Equal(expectedSCReason))
				}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
			}
			assertSCConditionMessage := func(substrings ...string) {
				Eventually(func(g Gomega) {
					g.Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
					scCond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageClassReady)
					g.Expect(scCond).NotTo(BeNil())
					for _, s := range substrings {
						g.Expect(scCond.Message).To(ContainSubstring(s))
					}
				}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
			}

			// ── Step 1: no namespace → Progressing, NamespaceReady=False ──────────
			By("reconciling when namespace does not exist")
			_ = doReconcile()
			assertStatus(
				v1alpha1.TenantPhaseProgressing, "",
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			// ── Step 2: namespace exists, no SC → Progressing, SC NotFound ────────
			By("creating the namespace")
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
			}
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, namespace) })

			By("reconciling with namespace but no StorageClass")
			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseProgressing, "",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			// ── Step 3: Default SC only → Ready via SharedDefault ─────────────────
			By("creating a shared Default StorageClass")
			defaultSC := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "shared-default-sc",
					Labels: map[string]string{osacTenantAnnotation: defaultStorageClassSentinel},
				},
				Provisioner: "kubernetes.io/no-provisioner",
			}
			Expect(k8sClient.Create(ctx, defaultSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, defaultSC) })

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseReady, "shared-default-sc",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonSharedDefault,
			)

			// ── Step 4: tenant SC added → Ready via Found (priority over Default) ─
			By("creating a tenant-specific StorageClass")
			tenantSC := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:   resourceName + "-sc",
					Labels: map[string]string{osacTenantAnnotation: resourceName},
				},
				Provisioner: "kubernetes.io/no-provisioner",
			}
			Expect(k8sClient.Create(ctx, tenantSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, tenantSC) })

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseReady, resourceName+"-sc",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 5: multiple tenant SCs → Progressing, MultipleFound ──────────
			By("creating a second tenant StorageClass (misconfiguration)")
			extraSC := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:   resourceName + "-sc-extra",
					Labels: map[string]string{osacTenantAnnotation: resourceName},
				},
				Provisioner: "kubernetes.io/no-provisioner",
			}
			Expect(k8sClient.Create(ctx, extraSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, extraSC) })

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseProgressing, "",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonMultipleFound,
			)
			By("verifying condition message names the conflicting StorageClasses")
			assertSCConditionMessage(resourceName+"-sc", resourceName+"-sc-extra")

			By("verifying a DuplicateStorageClass warning event was emitted")
			Expect(fakeRecorder.Events).To(Receive(And(
				ContainSubstring("Warning"),
				ContainSubstring("DuplicateStorageClass"),
				ContainSubstring(resourceName+"-sc"),
				ContainSubstring(resourceName+"-sc-extra"),
			)))

			// ── Step 6: remove extra SC → back to Ready (tenant SC) ───────────────
			By("removing the extra tenant StorageClass")
			Expect(k8sClient.Delete(ctx, extraSC)).To(Succeed())

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseReady, resourceName+"-sc",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 7: remove tenant SC → falls back to Default SC ───────────────
			By("removing the tenant-specific StorageClass")
			Expect(k8sClient.Delete(ctx, tenantSC)).To(Succeed())

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseReady, "shared-default-sc",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonSharedDefault,
			)

			// ── Step 8: remove Default SC → Progressing, NotFound ─────────────────
			By("removing the shared Default StorageClass")
			Expect(k8sClient.Delete(ctx, defaultSC)).To(Succeed())

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseProgressing, "",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			// ── Step 9: multiple Default SCs → Progressing, MultipleDefaultsFound ─
			By("creating two Default StorageClasses (ambiguous)")
			defaultSC1 := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "shared-default-sc-1",
					Labels: map[string]string{osacTenantAnnotation: defaultStorageClassSentinel},
				},
				Provisioner: "kubernetes.io/no-provisioner",
			}
			defaultSC2 := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "shared-default-sc-2",
					Labels: map[string]string{osacTenantAnnotation: defaultStorageClassSentinel},
				},
				Provisioner: "kubernetes.io/no-provisioner",
			}
			Expect(k8sClient.Create(ctx, defaultSC1)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, defaultSC1) })
			Expect(k8sClient.Create(ctx, defaultSC2)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, defaultSC2) })

			Expect(doReconcile()).NotTo(HaveOccurred())
			assertStatus(
				v1alpha1.TenantPhaseProgressing, "",
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonMultipleDefaultsFound,
			)
			By("verifying condition message names the conflicting Default StorageClasses and affected tenant")
			assertSCConditionMessage("shared-default-sc-1", "shared-default-sc-2", resourceName)

			By("verifying a DuplicateStorageClass warning event was emitted for Default SCs")
			Expect(fakeRecorder.Events).To(Receive(And(
				ContainSubstring("Warning"),
				ContainSubstring("DuplicateStorageClass"),
				ContainSubstring("shared-default-sc-1"),
				ContainSubstring("shared-default-sc-2"),
				ContainSubstring(resourceName),
			)))
		})
	})
})

var _ = Describe("joinStorageClassNames", func() {
	It("returns empty joined string and empty slice for nil or empty input", func() {
		joined, names := joinStorageClassNames(nil)
		Expect(joined).To(BeEmpty())
		Expect(names).To(BeEmpty())

		joined, names = joinStorageClassNames([]storagev1.StorageClass{})
		Expect(joined).To(BeEmpty())
		Expect(names).To(BeEmpty())
	})

	It("returns a single name", func() {
		joined, names := joinStorageClassNames([]storagev1.StorageClass{
			{ObjectMeta: metav1.ObjectMeta{Name: "sc-a"}},
		})
		Expect(joined).To(Equal("sc-a"))
		Expect(names).To(Equal([]string{"sc-a"}))
	})

	It("joins multiple names in list order", func() {
		joined, names := joinStorageClassNames([]storagev1.StorageClass{
			{ObjectMeta: metav1.ObjectMeta{Name: "sc-a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "sc-b"}},
		})
		Expect(joined).To(Equal("sc-a, sc-b"))
		Expect(names).To(Equal([]string{"sc-a", "sc-b"}))
	})
})
