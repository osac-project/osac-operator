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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/pkg/provisioning"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

func storageReconcileRequest(nn types.NamespacedName) mcreconcile.Request {
	return mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}}
}

func createReadyTenantForStorage(ctx context.Context, name, namespace string) {
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	Expect(k8sClient.Create(ctx, tenant)).To(Succeed())

	Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, tenant)
	}, 5*time.Second, 10*time.Millisecond).Should(Succeed())

	tenant.Status.Phase = v1alpha1.TenantPhaseReady
	tenant.Status.Namespace = name
	Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())

	Eventually(func(g Gomega) {
		t := &v1alpha1.Tenant{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, t)).To(Succeed())
		g.Expect(t.Status.Phase).To(Equal(v1alpha1.TenantPhaseReady))
	}, 5*time.Second, 10*time.Millisecond).Should(Succeed())
}

func createHubSecret(ctx context.Context, tenantName, namespace string) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("vast-tenant-config-%s", tenantName),
			Namespace: namespace,
			Labels: map[string]string{
				osacTenantKey: tenantName,
			},
		},
		Data: map[string][]byte{
			"vast_tenant_id": []byte("123"),
		},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
}

func createLabeledStorageClass(ctx context.Context, name, tenant, tier string) {
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				osacTenantKey:        tenant,
				osacStorageTierLabel: tier,
			},
		},
		Provisioner: "kubernetes.io/no-provisioner",
	}
	Expect(k8sClient.Create(ctx, sc)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sc))).To(Succeed())
	})
}

func newClusterOrder(name, namespace string, annotations map[string]string) *v1alpha1.ClusterOrder {
	return &v1alpha1.ClusterOrder{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: v1alpha1.ClusterOrderSpec{TemplateID: "test_template"},
	}
}

var _ = Describe("Storage Controller", func() {
	const (
		testNamespace    = "default"
		secretsNamespace = "osac-system"
		pollInterval     = 1 * time.Second
	)

	ctx := context.Background()

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: secretsNamespace},
		}
		if err := k8sClient.Create(ctx, ns); err != nil {
			Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
		}
	})

	Context("Stage 1: Backend provisioning", func() {
		It("should skip reconciliation when Tenant is not Ready", func() {
			name := "storage-test-not-ready"
			tenant := &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
				},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				&mockProvisioningProvider{}, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			Eventually(func() error {
				return r.Client.Get(ctx, nn, &v1alpha1.Tenant{})
			}, 5*time.Second, 10*time.Millisecond).Should(Succeed())

			result, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.ProvisioningJobs).To(BeEmpty())
			cond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
			Expect(cond).To(BeNil())
		})

		It("should trigger backend provisioning when hub Secret is missing", func() {
			name := "storage-test-no-secret"
			createReadyTenantForStorage(ctx, name, testNamespace)

			provider := &mockProvisioningProvider{}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				provider, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			result, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(pollInterval))

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.StorageBackendJobs).To(HaveLen(1))
			Expect(tenant.Status.StorageBackendJobs[0].Type).To(Equal(v1alpha1.JobTypeProvision))
			Expect(tenant.Status.StorageBackendJobs[0].JobID).To(Equal("mock-job-id"))

			cond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("should set StorageBackendReady=True when hub Secret exists", func() {
			name := "storage-test-secret-exists"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			cond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(v1alpha1.TenantReasonFound))
		})

		It("should set StorageBackendReady=False with NoProvider when no provider configured", func() {
			name := "storage-test-no-provider"
			createReadyTenantForStorage(ctx, name, testNamespace)

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			cond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(v1alpha1.TenantReasonNoProvider))
		})

		It("should propagate trigger error without creating fake job", func() {
			name := "storage-test-prov-fail"
			createReadyTenantForStorage(ctx, name, testNamespace)

			provider := &mockProvisioningProvider{
				triggerProvisionFunc: func(_ context.Context, _ client.Object) (*provisioning.ProvisionResult, error) {
					return nil, fmt.Errorf("AAP unreachable")
				},
			}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				provider, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("AAP unreachable"))

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.StorageBackendJobs).To(BeEmpty())
		})
	})

	Context("Stage 2: Cluster storage provisioning", func() {
		It("should trigger cluster storage provisioning when Stage 1 complete but no SCs", func() {
			name := "storage-test-no-sc"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)

			clusterProvider := &mockProvisioningProvider{name: "cluster-storage-mock"}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, clusterProvider, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			result, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(pollInterval))

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			backendCond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
			Expect(backendCond).NotTo(BeNil())
			Expect(backendCond.Status).To(Equal(metav1.ConditionTrue))

			Expect(tenant.Status.ClusterStorageJobs).To(HaveLen(1))
			Expect(tenant.Status.ClusterStorageJobs[0].Type).To(Equal(v1alpha1.JobTypeProvision))
		})

		It("should set ClusterStorageReady=True when SCs are discovered", func() {
			name := "storage-test-sc-found"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)
			createLabeledStorageClass(ctx, name+"-default-sc", name, "default")

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			clusterCond := tenant.GetStatusCondition(v1alpha1.TenantConditionClusterStorageReady)
			Expect(clusterCond).NotTo(BeNil())
			Expect(clusterCond.Status).To(Equal(metav1.ConditionTrue))

			Expect(tenant.Status.StorageClasses).To(HaveLen(1))
			Expect(tenant.Status.StorageClasses[0].Name).To(Equal(name + "-default-sc"))
			Expect(tenant.Status.StorageClasses[0].Tier).To(Equal("default"))
		})
	})

	Context("Tier resolution", func() {
		It("should fall back to Default StorageClass when no tenant-specific SC", func() {
			name := "storage-test-default-fallback"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)
			createLabeledStorageClass(ctx, "shared-default-sc-"+name, defaultStorageClassSentinel, "default")

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			Expect(tenant.Status.StorageClasses).To(HaveLen(1))
			Expect(tenant.Status.StorageClasses[0].Name).To(Equal("shared-default-sc-" + name))
		})

		It("should prefer tenant-specific SC over Default", func() {
			name := "storage-test-tenant-priority"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)
			createLabeledStorageClass(ctx, "shared-sc-"+name, defaultStorageClassSentinel, "default")
			createLabeledStorageClass(ctx, name+"-tenant-sc", name, "default")

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			Expect(tenant.Status.StorageClasses).To(HaveLen(1))
			Expect(tenant.Status.StorageClasses[0].Name).To(Equal(name + "-tenant-sc"))
		})
	})

	Context("Finalizer and deletion", func() {
		It("should add storage finalizer on first reconcile", func() {
			name := "storage-test-finalizer"
			createReadyTenantForStorage(ctx, name, testNamespace)

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Finalizers).To(ContainElement(storageFinalizer))
		})

		It("should run deletion even when class provider is nil", func() {
			name := "storage-test-delete-no-class"
			createReadyTenantForStorage(ctx, name, testNamespace)

			backendProvider := &mockProvisioningProvider{name: "backend-mock"}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				backendProvider, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Finalizers).To(ContainElement(storageFinalizer))

			Expect(k8sClient.Delete(ctx, tenant)).To(Succeed())

			Eventually(func(g Gomega) {
				_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
				g.Expect(err).NotTo(HaveOccurred())

				t := &v1alpha1.Tenant{}
				err = k8sClient.Get(ctx, nn, t)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				if err == nil {
					g.Expect(t.Finalizers).NotTo(ContainElement(storageFinalizer))
				}
			}).Should(Succeed())
		})
	})

	Context("Management state", func() {
		It("should skip reconciliation when Unmanaged", func() {
			name := "storage-test-unmanaged"
			tenant := &v1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateUnmanaged,
					},
				},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())

			Eventually(func() error {
				t := &v1alpha1.Tenant{}
				return k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, t)
			}, 5*time.Second, 10*time.Millisecond).Should(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, tenant)).To(Succeed())
			tenant.Status.Phase = v1alpha1.TenantPhaseReady
			Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())

			provider := &mockProvisioningProvider{}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				provider, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			result, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.ProvisioningJobs).To(BeEmpty())
			Expect(tenant.Status.StorageBackendJobs).To(BeEmpty())
			Expect(tenant.Status.ClusterStorageJobs).To(BeEmpty())
			Expect(tenant.Finalizers).NotTo(ContainElement(storageFinalizer))
		})
	})

	Context("Job array isolation", func() {
		It("should place backend jobs in StorageBackendJobs array", func() {
			name := "storage-test-backend-array"
			createReadyTenantForStorage(ctx, name, testNamespace)

			provider := &mockProvisioningProvider{}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				provider, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.StorageBackendJobs).To(HaveLen(1))
			Expect(tenant.Status.StorageBackendJobs[0].Type).To(Equal(v1alpha1.JobTypeProvision))
			Expect(tenant.Status.ProvisioningJobs).To(BeEmpty())
			Expect(tenant.Status.ClusterStorageJobs).To(BeEmpty())
		})

		It("should place cluster storage jobs in ClusterStorageJobs array", func() {
			name := "storage-test-cs-array"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)

			clusterProvider := &mockProvisioningProvider{name: "cluster-storage-mock"}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, clusterProvider, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.ClusterStorageJobs).To(HaveLen(1))
			Expect(tenant.Status.ClusterStorageJobs[0].Type).To(Equal(v1alpha1.JobTypeProvision))
			Expect(tenant.Status.ProvisioningJobs).To(BeEmpty())
			Expect(tenant.Status.StorageBackendJobs).To(BeEmpty())
		})
	})

	Context("Cluster storage failure", func() {
		It("should record failed cluster storage provisioning job", func() {
			name := "storage-test-cs-fail"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)

			clusterProvider := &mockProvisioningProvider{
				name: "cluster-storage-mock",
				triggerProvisionFunc: func(_ context.Context, _ client.Object) (*provisioning.ProvisionResult, error) {
					return nil, fmt.Errorf("cluster storage template not found")
				},
			}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, clusterProvider, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cluster storage template not found"))

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.ClusterStorageJobs).To(BeEmpty())
		})
	})

	Context("Backend condition transition", func() {
		It("should transition StorageBackendReady from True to False when hub Secret disappears", func() {
			name := "storage-test-backend-transition"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			cond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("vast-tenant-config-%s", name),
				Namespace: secretsNamespace,
			}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())

			provider := &mockProvisioningProvider{}
			r2 := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				provider, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
				_, err := r2.Reconcile(ctx, storageReconcileRequest(nn))
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
				cond := tenant.GetStatusCondition(v1alpha1.TenantConditionStorageBackendReady)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})
	})

	Context("Deprovisioning ordering", func() {
		It("should clean up cluster storage before backend teardown", func() {
			name := "storage-test-deprov-order"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)

			var deprovisionOrder []string
			clusterProvider := &mockProvisioningProvider{
				name: "cluster-mock",
				triggerDeprovisionFunc: func(_ context.Context, _ client.Object, _ []v1alpha1.JobStatus) (*provisioning.DeprovisionResult, error) {
					deprovisionOrder = append(deprovisionOrder, "cluster-storage")
					return &provisioning.DeprovisionResult{
						Action: provisioning.DeprovisionTriggered,
						JobID:  "deprov-cs-1",
					}, nil
				},
				getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{State: v1alpha1.JobStateSucceeded, Message: "done"}, nil
				},
			}
			backendProvider := &mockProvisioningProvider{
				name: "backend-mock",
				triggerDeprovisionFunc: func(_ context.Context, _ client.Object, _ []v1alpha1.JobStatus) (*provisioning.DeprovisionResult, error) {
					deprovisionOrder = append(deprovisionOrder, "backend")
					return &provisioning.DeprovisionResult{
						Action: provisioning.DeprovisionTriggered,
						JobID:  "deprov-be-1",
					}, nil
				},
				getDeprovisionStatusFunc: func(_ context.Context, _ client.Object, _ string) (provisioning.ProvisionStatus, error) {
					return provisioning.ProvisionStatus{State: v1alpha1.JobStateSucceeded, Message: "done"}, nil
				},
			}

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				backendProvider, clusterProvider, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Finalizers).To(ContainElement(storageFinalizer))

			Expect(k8sClient.Delete(ctx, tenant)).To(Succeed())

			Eventually(func(g Gomega) {
				_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deprovisionOrder).To(HaveLen(2))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			Expect(deprovisionOrder[0]).To(Equal("cluster-storage"))
			Expect(deprovisionOrder[1]).To(Equal("backend"))
		})
	})

	Context("Duplicate StorageClass detection", func() {
		It("should set ClusterStorageReady=False with MultipleFound when duplicate SCs exist for same tier", func() {
			name := "storage-test-dup-sc"
			createReadyTenantForStorage(ctx, name, testNamespace)
			createHubSecret(ctx, name, secretsNamespace)
			createLabeledStorageClass(ctx, name+"-sc-1", name, "default")
			createLabeledStorageClass(ctx, name+"-sc-2", name, "default")

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval,
				provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: name, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())

			cond := tenant.GetStatusCondition(v1alpha1.TenantConditionClusterStorageReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(v1alpha1.TenantReasonMultipleFound))
			Expect(tenant.Status.StorageClasses).To(BeEmpty())
		})
	})

	Context("CaaS: mapClusterOrderToTenant", func() {
		It("should return reconcile request for the owning Tenant", func() {
			tenantName := "caas-map-valid"
			createReadyTenantForStorage(ctx, tenantName, testNamespace)

			co := newClusterOrder("caas-map-co-1", testNamespace, map[string]string{
				osacTenantKey: tenantName,
			})
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			requests := r.mapClusterOrderToTenant(ctx, co)
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal(tenantName))
			Expect(requests[0].Namespace).To(Equal(testNamespace))
		})

		It("should return nil when tenant annotation is missing", func() {
			co := newClusterOrder("caas-map-no-annotation", testNamespace, nil)
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			requests := r.mapClusterOrderToTenant(ctx, co)
			Expect(requests).To(BeNil())
		})

		It("should return nil when tenant annotation is empty", func() {
			co := newClusterOrder("caas-map-empty-annotation", testNamespace, map[string]string{
				osacTenantKey: "",
			})
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			requests := r.mapClusterOrderToTenant(ctx, co)
			Expect(requests).To(BeNil())
		})

		It("should return nil when referenced Tenant does not exist", func() {
			co := newClusterOrder("caas-map-no-tenant", testNamespace, map[string]string{
				osacTenantKey: "nonexistent-tenant",
			})
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			requests := r.mapClusterOrderToTenant(ctx, co)
			Expect(requests).To(BeNil())
		})
	})

	Context("CaaS: getClusterKubeconfig", func() {
		// HyperShift convention: HCP namespace = {HC-namespace}-{HC-name}
		const hcNamespace = "clusters-caas-test"
		const hcName = "test-hcp"
		const hcpNamespace = hcNamespace + "-" + hcName

		BeforeEach(func() {
			for _, nsName := range []string{hcNamespace, hcpNamespace} {
				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
				if err := k8sClient.Create(ctx, ns); err != nil {
					Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
				}
			}
		})

		It("should return kubeconfig from HostedControlPlane Secret reference", func() {
			kubeconfigData := []byte("apiVersion: v1\nclusters: []\n")

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "admin-kubeconfig",
					Namespace: hcpNamespace,
				},
				Data: map[string][]byte{
					"kubeconfig": kubeconfigData,
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

			hcp := &hypershiftv1beta1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hcName,
					Namespace: hcpNamespace,
				},
			}
			Expect(k8sClient.Create(ctx, hcp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, hcp) })

			hcp.Status.KubeConfig = &hypershiftv1beta1.KubeconfigSecretRef{
				Name: "admin-kubeconfig",
				Key:  "kubeconfig",
			}
			Expect(k8sClient.Status().Update(ctx, hcp)).To(Succeed())

			co := newClusterOrder("caas-kc-valid", testNamespace, nil)
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })
			co.Status.ClusterReference = &v1alpha1.ClusterOrderClusterReferenceType{
				Namespace:         hcNamespace,
				HostedClusterName: hcName,
			}
			Expect(k8sClient.Status().Update(ctx, co)).To(Succeed())

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			result, err := r.getClusterKubeconfig(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(kubeconfigData))
		})

		It("should return nil when ClusterReference is nil", func() {
			co := newClusterOrder("caas-kc-no-ref", testNamespace, nil)
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			result, err := r.getClusterKubeconfig(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("should return nil when HostedControlPlane does not exist", func() {
			co := newClusterOrder("caas-kc-no-hcp", testNamespace, nil)
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })
			co.Status.ClusterReference = &v1alpha1.ClusterOrderClusterReferenceType{
				Namespace:         hcNamespace,
				HostedClusterName: "nonexistent-hcp",
			}
			Expect(k8sClient.Status().Update(ctx, co)).To(Succeed())

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			result, err := r.getClusterKubeconfig(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("should return nil when kubeconfig Secret does not exist", func() {
			hcpNsNoSecret := hcNamespace + "-test-hcp-no-secret"
			nsNoSecret := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: hcpNsNoSecret}}
			if err := k8sClient.Create(ctx, nsNoSecret); err != nil {
				Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
			}

			hcp := &hypershiftv1beta1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-hcp-no-secret",
					Namespace: hcpNsNoSecret,
				},
			}
			Expect(k8sClient.Create(ctx, hcp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, hcp) })

			hcp.Status.KubeConfig = &hypershiftv1beta1.KubeconfigSecretRef{
				Name: "nonexistent-secret",
				Key:  "kubeconfig",
			}
			Expect(k8sClient.Status().Update(ctx, hcp)).To(Succeed())

			co := newClusterOrder("caas-kc-no-secret", testNamespace, nil)
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })
			co.Status.ClusterReference = &v1alpha1.ClusterOrderClusterReferenceType{
				Namespace:         hcNamespace,
				HostedClusterName: "test-hcp-no-secret",
			}
			Expect(k8sClient.Status().Update(ctx, co)).To(Succeed())

			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			result, err := r.getClusterKubeconfig(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})
	})

	Context("CaaS: provisioning trigger", func() {
		It("should trigger CaaS provisioning for Ready ClusterOrder with backend ready", func() {
			tenantName := "caas-prov-trigger"
			createReadyTenantForStorage(ctx, tenantName, testNamespace)
			createHubSecret(ctx, tenantName, secretsNamespace)
			// VMaaS StorageClasses must already exist from tenant onboarding
			// before CaaS provisioning runs, because the VMaaS resolution in
			// handleUpdate returns early if no SCs are found.
			createLabeledStorageClass(ctx, tenantName+"-vmaas-sc", tenantName, "default")

			co := newClusterOrder("caas-prov-co-1", testNamespace, map[string]string{
				osacTenantKey: tenantName,
			})
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			co.Status.Phase = v1alpha1.ClusterOrderPhaseReady
			Expect(k8sClient.Status().Update(ctx, co)).To(Succeed())

			// ClusterStorageProvider triggers provisioning; the kubeconfig
			// retrieval returns nil (no HCP) so the controller sets
			// KubeConfigNotAvailable and continues.
			clusterProvider := &mockProvisioningProvider{name: "caas-mock"}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, clusterProvider, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: tenantName, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			// ClusterOrder should have the storage finalizer and
			// KubeConfigNotAvailable condition
			updatedCO := &v1alpha1.ClusterOrder{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(co), updatedCO)).To(Succeed())
			Expect(updatedCO.Finalizers).To(ContainElement(clusterStorageFinalizer))
			Expect(updatedCO.IsStatusConditionFalse(
				string(v1alpha1.ClusterOrderConditionClusterStorageReady))).To(BeTrue())
		})

		It("should skip CaaS provisioning for non-Ready ClusterOrders", func() {
			tenantName := "caas-prov-skip-notready"
			createReadyTenantForStorage(ctx, tenantName, testNamespace)
			createHubSecret(ctx, tenantName, secretsNamespace)

			co := newClusterOrder("caas-prov-co-notready", testNamespace, map[string]string{
				osacTenantKey: tenantName,
			})
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			co.Status.Phase = v1alpha1.ClusterOrderPhaseProgressing
			Expect(k8sClient.Status().Update(ctx, co)).To(Succeed())

			clusterProvider := &mockProvisioningProvider{name: "caas-mock"}
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, clusterProvider, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: tenantName, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			updatedCO := &v1alpha1.ClusterOrder{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(co), updatedCO)).To(Succeed())
			Expect(updatedCO.Finalizers).NotTo(ContainElement(clusterStorageFinalizer))
		})
	})

	Context("CaaS: VMaaS regression", func() {
		It("should not affect VMaaS provisioning when CaaS ClusterOrders exist", func() {
			tenantName := "caas-vmaas-regression"
			createReadyTenantForStorage(ctx, tenantName, testNamespace)
			createHubSecret(ctx, tenantName, secretsNamespace)
			createLabeledStorageClass(ctx, tenantName+"-vmaas-sc", tenantName, "default")

			// Create a CaaS ClusterOrder for the same tenant
			co := newClusterOrder("caas-regression-co", testNamespace, map[string]string{
				osacTenantKey: tenantName,
			})
			Expect(k8sClient.Create(ctx, co)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, co) })

			co.Status.Phase = v1alpha1.ClusterOrderPhaseReady
			Expect(k8sClient.Status().Update(ctx, co)).To(Succeed())

			// No cluster provider: VMaaS-only setup with CaaS ClusterOrder present
			r := NewStorageReconciler(
				testMcManager, testNamespace, mcmanager.LocalCluster,
				nil, nil, pollInterval, provisioning.DefaultMaxJobHistory,
			)

			nn := types.NamespacedName{Name: tenantName, Namespace: testNamespace}
			_, err := r.Reconcile(ctx, storageReconcileRequest(nn))
			Expect(err).NotTo(HaveOccurred())

			// VMaaS StorageClass should still be resolved on the Tenant
			tenant := &v1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, nn, tenant)).To(Succeed())
			Expect(tenant.Status.StorageClasses).To(HaveLen(1))
			Expect(tenant.Status.StorageClasses[0].Name).To(Equal(tenantName + "-vmaas-sc"))

			clusterCond := tenant.GetStatusCondition(v1alpha1.TenantConditionClusterStorageReady)
			Expect(clusterCond).NotTo(BeNil())
			Expect(clusterCond.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})
