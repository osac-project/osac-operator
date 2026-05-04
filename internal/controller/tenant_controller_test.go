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
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

func makeSC(name, tenant, tier string) *storagev1.StorageClass {
	labels := map[string]string{}
	if tenant != "" {
		labels[osacTenantAnnotation] = tenant
	}
	if tier != "" {
		labels[osacStorageTierLabel] = tier
	}
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Provisioner: "kubernetes.io/no-provisioner",
	}
}

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

		It("should transition through all Ready/Progressing phases with multi-tier StorageClasses", func() {
			fakeRecorder := events.NewFakeRecorder(100)
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

			reconcileAndAssertPhase := func(
				expectedPhase v1alpha1.TenantPhaseType,
				expectedSCs []v1alpha1.ResolvedStorageClass,
				expectedNSStatus metav1.ConditionStatus,
				expectedNSReason string,
				expectedSCStatus metav1.ConditionStatus,
				expectedSCReason string,
			) {
				Eventually(func(g Gomega) {
					_ = doReconcile()
					g.Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
					g.Expect(tenant.Status.Phase).To(Equal(expectedPhase))
					if expectedSCs == nil {
						g.Expect(tenant.Status.StorageClass).To(BeEmpty())
						g.Expect(tenant.Status.StorageClasses).To(BeNil())
					} else {
						g.Expect(tenant.Status.StorageClasses).To(ConsistOf(expectedSCs))
						g.Expect(tenant.Status.StorageClass).To(Equal(expectedSCs[0].Name))
					}

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

			// ── Step 1: no namespace → Progressing ──────────────────────────
			By("reconciling when namespace does not exist")
			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseProgressing, nil,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			// ── Step 2: namespace exists, no SCs → Progressing, NotFound ────
			By("creating the namespace")
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
			}
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, namespace) })

			By("reconciling with namespace but no StorageClasses")
			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseProgressing, nil,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			// ── Step 3: SC without storage-tier label → ignored ─────────────
			By("creating a StorageClass without storage-tier label (should be ignored)")
			scNoTier := makeSC("sc-no-tier", resourceName, "")
			Expect(k8sClient.Create(ctx, scNoTier)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, scNoTier) })

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseProgressing, nil,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			// ── Step 4: shared Default SC with tier → Ready via fallback ─────
			By("creating a shared Default StorageClass with tier label")
			defaultSC := makeSC("shared-default-sc", defaultStorageClassSentinel, "default")
			Expect(k8sClient.Create(ctx, defaultSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, defaultSC) })

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: "shared-default-sc", Tier: "default"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 5: tenant-specific SC takes priority over Default ───────
			By("creating a tenant-specific StorageClass for the same tier")
			tenantSC := makeSC(resourceName+"-default-sc", resourceName, "default")
			Expect(k8sClient.Create(ctx, tenantSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, tenantSC) })

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: resourceName + "-default-sc", Tier: "default"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)
			assertSCConditionMessage("tenant-specific")

			// ── Step 6: add a second tier → two resolved entries ─────────────
			By("adding a fast tier StorageClass for the tenant")
			tenantFastSC := makeSC(resourceName+"-fast-sc", resourceName, "fast")
			Expect(k8sClient.Create(ctx, tenantFastSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, tenantFastSC) })

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: resourceName + "-default-sc", Tier: "default"},
					{Name: resourceName + "-fast-sc", Tier: "fast"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 7: duplicate within one tier does not affect the other ──
			By("creating a duplicate fast tier StorageClass")
			extraFastSC := makeSC(resourceName+"-fast-sc-extra", resourceName, "fast")
			Expect(k8sClient.Create(ctx, extraFastSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, extraFastSC) })

			By("verifying tenant is still Ready (default tier resolves, fast tier has error)")
			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: resourceName + "-default-sc", Tier: "default"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)
			assertSCConditionMessage("fast", "multiple")

			By("verifying a DuplicateStorageClass warning event was emitted")
			Eventually(fakeRecorder.Events).Should(Receive(And(
				ContainSubstring("Warning"),
				ContainSubstring(eventReasonDuplicateStorageClass),
				ContainSubstring("fast"),
			)))

			// ── Step 8: remove duplicate → both tiers resolve again ──────────
			By("removing the extra fast StorageClass")
			Expect(k8sClient.Delete(ctx, extraFastSC)).To(Succeed())

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: resourceName + "-default-sc", Tier: "default"},
					{Name: resourceName + "-fast-sc", Tier: "fast"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 9: mixed resolution — tenant fast, Default for standard ─
			By("adding a shared Default standard tier")
			defaultStandardSC := makeSC("shared-standard-sc", defaultStorageClassSentinel, "standard")
			Expect(k8sClient.Create(ctx, defaultStandardSC)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, defaultStandardSC) })

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: resourceName + "-default-sc", Tier: "default"},
					{Name: resourceName + "-fast-sc", Tier: "fast"},
					{Name: "shared-standard-sc", Tier: "standard"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 10: remove all tenant SCs → falls back to Default per tier
			By("removing tenant-specific StorageClasses")
			Expect(k8sClient.Delete(ctx, tenantSC)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tenantFastSC)).To(Succeed())

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseReady,
				[]v1alpha1.ResolvedStorageClass{
					{Name: "shared-default-sc", Tier: "default"},
					{Name: "shared-standard-sc", Tier: "standard"},
				},
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
			)

			// ── Step 11: remove all SCs → Progressing, NotFound ──────────────
			By("removing all StorageClasses")
			Expect(k8sClient.Delete(ctx, defaultSC)).To(Succeed())
			Expect(k8sClient.Delete(ctx, defaultStandardSC)).To(Succeed())

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseProgressing, nil,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonNotFound,
			)

			By("verifying a StorageClassNotReady warning event was emitted")
			Eventually(fakeRecorder.Events).Should(Receive(And(
				ContainSubstring("Warning"),
				ContainSubstring(eventReasonStorageClassNotReady),
			)))

			// ── Step 12: all tiers have duplicates → Progressing, MultipleFound
			By("creating two Default SCs for the same tier (both duplicate)")
			dupSC1 := makeSC("dup-default-1", defaultStorageClassSentinel, "default")
			dupSC2 := makeSC("dup-default-2", defaultStorageClassSentinel, "default")
			Expect(k8sClient.Create(ctx, dupSC1)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dupSC1) })
			Expect(k8sClient.Create(ctx, dupSC2)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dupSC2) })

			reconcileAndAssertPhase(
				v1alpha1.TenantPhaseProgressing, nil,
				metav1.ConditionTrue, v1alpha1.TenantReasonFound,
				metav1.ConditionFalse, v1alpha1.TenantReasonMultipleFound,
			)
			assertSCConditionMessage("dup-default-1", "dup-default-2")
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

var _ = Describe("groupByTier", func() {
	It("ignores StorageClasses without storage-tier label", func() {
		scs := []storagev1.StorageClass{
			*makeSC("sc-no-tier", "tenant-a", ""),
			*makeSC("sc-with-tier", "tenant-a", "fast"),
		}
		groups := groupByTier(scs)
		Expect(groups).To(HaveLen(1))
		Expect(groups).To(HaveKey("fast"))
		Expect(groups["fast"]).To(HaveLen(1))
	})

	It("groups multiple SCs by tier", func() {
		scs := []storagev1.StorageClass{
			*makeSC("sc-fast-1", "tenant-a", "fast"),
			*makeSC("sc-fast-2", "tenant-a", "fast"),
			*makeSC("sc-standard", "tenant-a", "standard"),
		}
		groups := groupByTier(scs)
		Expect(groups).To(HaveLen(2))
		Expect(groups["fast"]).To(HaveLen(2))
		Expect(groups["standard"]).To(HaveLen(1))
	})

	It("returns empty map for empty input", func() {
		groups := groupByTier(nil)
		Expect(groups).To(BeEmpty())
	})

	It("normalizes tier values to lowercase", func() {
		scs := []storagev1.StorageClass{
			*makeSC("sc-upper", "tenant-a", "FAST"),
			*makeSC("sc-lower", "tenant-a", "fast"),
		}
		groups := groupByTier(scs)
		Expect(groups).To(HaveLen(1))
		Expect(groups["fast"]).To(HaveLen(2))
	})

	It("ignores tier values that don't match CRD pattern after lowercasing", func() {
		scs := []storagev1.StorageClass{
			*makeSC("sc-valid", "tenant-a", "fast"),
			*makeSC("sc-invalid-chars", "tenant-a", "not valid!"),
			*makeSC("sc-invalid-start", "tenant-a", "-leading-dash"),
			*makeSC("sc-invalid-end", "tenant-a", "trailing-dash-"),
		}
		groups := groupByTier(scs)
		Expect(groups).To(HaveLen(1))
		Expect(groups).To(HaveKey("fast"))
	})
})
