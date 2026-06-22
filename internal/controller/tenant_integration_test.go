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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/pkg/dbwatch"
)

// controllableQuerier implements dbwatch.Querier with thread-safe mutation
// methods for simulating database state changes during integration tests.
type controllableQuerier struct {
	mu      sync.RWMutex
	tenants map[string]dbwatch.TenantRecord
	err     error
	closed  bool
}

func newControllableQuerier() *controllableQuerier {
	return &controllableQuerier{tenants: make(map[string]dbwatch.TenantRecord)}
}

func (q *controllableQuerier) ListTenants(_ context.Context) ([]dbwatch.TenantRecord, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.err != nil {
		return nil, q.err
	}
	result := make([]dbwatch.TenantRecord, 0, len(q.tenants))
	for _, t := range q.tenants {
		result = append(result, t)
	}
	return result, nil
}

func (q *controllableQuerier) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	return nil
}

func (q *controllableQuerier) addTenant(t dbwatch.TenantRecord) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tenants[t.ID] = t
}

func (q *controllableQuerier) removeTenant(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.tenants, id)
}

func (q *controllableQuerier) updateTenant(id string, fn func(*dbwatch.TenantRecord)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.tenants[id]; ok {
		fn(&t)
		q.tenants[id] = t
	}
}

func (q *controllableQuerier) setError(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.err = err
}

func (q *controllableQuerier) clearError() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.err = nil
}

var _ = Describe("Tenant Lifecycle Integration", Ordered, func() {
	const (
		integNamespace = "integ-tenant-test"
		pollInterval   = 100 * time.Millisecond
		reconcileIvl   = 500 * time.Millisecond
		timeout        = 15 * time.Second
		polling        = 200 * time.Millisecond
	)

	var (
		integCtx    context.Context
		integCancel context.CancelFunc
		querier     *controllableQuerier
	)

	BeforeAll(func() {
		integCtx, integCancel = context.WithCancel(context.Background())

		By("creating integration test namespace")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: integNamespace}}
		Expect(k8sClient.Create(integCtx, ns)).To(Succeed())

		By("setting up controllable querier")
		querier = newControllableQuerier()

		By("creating event channel and watcher")
		ch := make(chan event.TypedGenericEvent[string], 256)
		enqueuer := func(tenantName string) {
			select {
			case ch <- event.TypedGenericEvent[string]{Object: tenantName}:
			default:
			}
		}
		watcher := dbwatch.New(querier, pollInterval, enqueuer, ctrl.Log)

		By("creating dedicated manager for integration tests")
		localMgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())

		integMcMgr, err := mcmanager.WithMultiCluster(localMgr, nil)
		Expect(err).NotTo(HaveOccurred())

		By("creating and registering tenant reconciler")
		reconciler := NewTenantReconciler(
			integMcMgr,
			integNamespace,
			"",
			noopProvisioningProvider{},
			30*time.Second,
			5,
			ch,
			watcher,
			reconcileIvl,
		)
		Expect(reconciler.SetupWithManager(integMcMgr)).To(Succeed())

		By("adding watcher to manager")
		Expect(localMgr.Add(watcher)).To(Succeed())

		By("starting manager")
		go func() {
			defer GinkgoRecover()
			_ = localMgr.Start(integCtx)
		}()
		Expect(localMgr.GetCache().WaitForCacheSync(integCtx)).To(BeTrue())
	})

	AfterAll(func() {
		By("stopping integration test manager")
		integCancel()

		By("cleaning up integration test namespace")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: integNamespace}}
		_ = k8sClient.Delete(context.Background(), ns)
	})

	Context("DB-driven lifecycle", func() {
		It("creates CR when tenant added to database", func() {
			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-create-1", Name: "integ-create-tenant",
				DisplayName: "Integration Create", EmailDomains: []string{"create.test"},
			})

			Eventually(func(g Gomega) {
				var tenant v1alpha1.Tenant
				g.Expect(k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-create-tenant", Namespace: integNamespace,
				}, &tenant)).To(Succeed())
				g.Expect(tenant.Spec.DisplayName).To(Equal("Integration Create"))
				g.Expect(tenant.Spec.EmailDomains).To(ContainElement("create.test"))
				g.Expect(tenant.Annotations[osacManagedByAnnotation]).To(Equal(osacManagedByValue))
			}, timeout, polling).Should(Succeed())
		})

		It("deletes CR when tenant removed from database", func() {
			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-delete-1", Name: "integ-delete-tenant",
				DisplayName: "To Delete", EmailDomains: []string{"delete.test"},
			})

			Eventually(func() error {
				return k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-delete-tenant", Namespace: integNamespace,
				}, &v1alpha1.Tenant{})
			}, timeout, polling).Should(Succeed())

			querier.removeTenant("integ-delete-1")

			Eventually(func() bool {
				err := k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-delete-tenant", Namespace: integNamespace,
				}, &v1alpha1.Tenant{})
				return err != nil
			}, timeout, polling).Should(BeTrue())
		})

		It("updates CR when tenant modified in database", func() {
			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-update-1", Name: "integ-update-tenant",
				DisplayName: "Before Update", EmailDomains: []string{"update.test"},
			})

			Eventually(func(g Gomega) {
				var tenant v1alpha1.Tenant
				g.Expect(k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-update-tenant", Namespace: integNamespace,
				}, &tenant)).To(Succeed())
				g.Expect(tenant.Spec.DisplayName).To(Equal("Before Update"))
			}, timeout, polling).Should(Succeed())

			querier.updateTenant("integ-update-1", func(t *dbwatch.TenantRecord) {
				t.DisplayName = "After Update"
			})

			Eventually(func(g Gomega) {
				var tenant v1alpha1.Tenant
				g.Expect(k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-update-tenant", Namespace: integNamespace,
				}, &tenant)).To(Succeed())
				g.Expect(tenant.Spec.DisplayName).To(Equal("After Update"))
			}, timeout, polling).Should(Succeed())
		})
	})

	Context("Reconciliation recovery", func() {
		It("recreates CR after manual deletion", func() {
			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-recreate-1", Name: "integ-recreate-tenant",
				DisplayName: "Recreate Me", EmailDomains: []string{"recreate.test"},
			})

			Eventually(func() error {
				return k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-recreate-tenant", Namespace: integNamespace,
				}, &v1alpha1.Tenant{})
			}, timeout, polling).Should(Succeed())

			var tenant v1alpha1.Tenant
			Expect(k8sClient.Get(integCtx, types.NamespacedName{
				Name: "integ-recreate-tenant", Namespace: integNamespace,
			}, &tenant)).To(Succeed())
			Expect(k8sClient.Delete(integCtx, &tenant)).To(Succeed())

			Eventually(func(g Gomega) {
				var recreated v1alpha1.Tenant
				g.Expect(k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-recreate-tenant", Namespace: integNamespace,
				}, &recreated)).To(Succeed())
				g.Expect(recreated.Annotations[osacManagedByAnnotation]).To(Equal(osacManagedByValue))
			}, timeout, polling).Should(Succeed())
		})

		It("ignores CRs without managed-by annotation", func() {
			manual := &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "integ-manual-tenant",
					Namespace: integNamespace,
				},
				Spec: v1alpha1.TenantSpec{
					DisplayName:  "Manual Tenant",
					EmailDomains: []string{"manual.test"},
				},
			}
			Expect(k8sClient.Create(integCtx, manual)).To(Succeed())

			time.Sleep(2 * reconcileIvl)

			var tenant v1alpha1.Tenant
			Expect(k8sClient.Get(integCtx, types.NamespacedName{
				Name: "integ-manual-tenant", Namespace: integNamespace,
			}, &tenant)).To(Succeed())
			Expect(tenant.Annotations).NotTo(HaveKey(osacManagedByAnnotation))
		})
	})

	Context("Failure recovery", func() {
		It("recovers from database connection failure", func() {
			querier.setError(fmt.Errorf("connection refused"))

			time.Sleep(3 * pollInterval)

			querier.clearError()
			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-recover-1", Name: "integ-recover-tenant",
				DisplayName: "Recovered", EmailDomains: []string{"recover.test"},
			})

			Eventually(func() error {
				return k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-recover-tenant", Namespace: integNamespace,
				}, &v1alpha1.Tenant{})
			}, timeout, polling).Should(Succeed())
		})
	})

	Context("Legacy CR adoption", func() {
		It("adopts pre-existing legacy CR and backfills fields", func() {
			legacy := &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "integ-adopt-tenant",
					Namespace: integNamespace,
				},
				Spec: v1alpha1.TenantSpec{},
			}
			Expect(k8sClient.Create(integCtx, legacy)).To(Succeed())

			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-adopt-1", Name: "integ-adopt-tenant",
				DisplayName: "Adopted Tenant", EmailDomains: []string{"adopt.test"},
			})

			Eventually(func(g Gomega) {
				var tenant v1alpha1.Tenant
				g.Expect(k8sClient.Get(integCtx, types.NamespacedName{
					Name: "integ-adopt-tenant", Namespace: integNamespace,
				}, &tenant)).To(Succeed())
				g.Expect(tenant.Annotations[osacManagedByAnnotation]).To(Equal(osacManagedByValue))
				g.Expect(tenant.Spec.DisplayName).To(Equal("Adopted Tenant"))
				g.Expect(tenant.Spec.EmailDomains).To(ContainElement("adopt.test"))
			}, timeout, polling).Should(Succeed())
		})

		It("leaves CR unmanaged when DB record has empty displayName", func() {
			unmanaged := &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "integ-incomplete-tenant",
					Namespace: integNamespace,
				},
				Spec: v1alpha1.TenantSpec{DisplayName: "Existing"},
			}
			Expect(k8sClient.Create(integCtx, unmanaged)).To(Succeed())

			querier.addTenant(dbwatch.TenantRecord{
				ID: "integ-incomplete-1", Name: "integ-incomplete-tenant",
				DisplayName: "", EmailDomains: []string{},
			})

			time.Sleep(2 * reconcileIvl)

			var tenant v1alpha1.Tenant
			Expect(k8sClient.Get(integCtx, types.NamespacedName{
				Name: "integ-incomplete-tenant", Namespace: integNamespace,
			}, &tenant)).To(Succeed())
			Expect(tenant.Annotations).NotTo(HaveKey(osacManagedByAnnotation))
		})

		It("leaves CR unmanaged when no DB match exists", func() {
			noMatch := &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "integ-nomatch-tenant",
					Namespace: integNamespace,
				},
				Spec: v1alpha1.TenantSpec{DisplayName: "No Match"},
			}
			Expect(k8sClient.Create(integCtx, noMatch)).To(Succeed())

			time.Sleep(2 * reconcileIvl)

			var tenant v1alpha1.Tenant
			Expect(k8sClient.Get(integCtx, types.NamespacedName{
				Name: "integ-nomatch-tenant", Namespace: integNamespace,
			}, &tenant)).To(Succeed())
			Expect(tenant.Annotations).NotTo(HaveKey(osacManagedByAnnotation))
		})
	})
})
