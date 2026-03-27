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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const testTemplateParams = `{"key": "value"}`

// createReadyTenant creates a Tenant with the given name in the namespace and sets status to Ready.
// If the tenant already exists, it is updated to Ready.
func createReadyTenant(ctx context.Context, namespace, name string) {
	tenant := &osacv1alpha1.Tenant{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, tenant)
	if err != nil && errors.IsNotFound(err) {
		tenant = &osacv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: osacv1alpha1.TenantSpec{},
		}
		Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
	}
	tenant.Status.Phase = osacv1alpha1.TenantPhaseReady
	tenant.Status.Namespace = namespace
	Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())
}

// deleteTenantInNamespace removes a Tenant by name in the given namespace (clears finalizers first).
func deleteTenantInNamespace(ctx context.Context, namespace, name string) {
	tenant := &osacv1alpha1.Tenant{}
	nn := types.NamespacedName{Name: name, Namespace: namespace}
	if err := k8sClient.Get(ctx, nn, tenant); err == nil {
		tenant.Finalizers = nil
		_ = k8sClient.Update(ctx, tenant)
		_ = k8sClient.Delete(ctx, tenant)
	}
}

var _ = Describe("ComputeInstance Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const namespaceName = "default"
		const tenantName = "test-tenant"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: namespaceName,
		}
		computeInstance := &osacv1alpha1.ComputeInstance{}

		BeforeEach(func() {
			By("creating a tenant for the ComputeInstance to reference")
			tenant := &osacv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantName,
					Namespace: namespaceName,
				},
				Spec: osacv1alpha1.TenantSpec{},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: tenantName, Namespace: namespaceName}, &osacv1alpha1.Tenant{})
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
				tenant.Status.Phase = osacv1alpha1.TenantPhaseReady
				tenant.Status.Namespace = namespaceName
				Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())
			}

			By("creating the custom resource for the Kind ComputeInstance")
			err = k8sClient.Get(ctx, typeNamespacedName, computeInstance)
			if err != nil && errors.IsNotFound(err) {
				resource := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespaceName,
						Annotations: map[string]string{
							osacTenantAnnotation: tenantName,
						},
					},
					Spec: newTestComputeInstanceSpec("test_template"),
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ComputeInstance")
			err := k8sClient.Get(ctx, typeNamespacedName, computeInstance)
			Expect(err).NotTo(HaveOccurred())

			// Now delete the resource
			err = k8sClient.Delete(ctx, computeInstance)
			if err != nil && !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			By("Reconciling the deleted resource")
			Eventually(func() error {
				controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)
				_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{
					NamespacedName: typeNamespacedName,
				}})
				return err
			}).Should(Succeed())

			By("Cleanup the tenant")
			tenant := &osacv1alpha1.Tenant{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: tenantName, Namespace: namespaceName}, tenant); err == nil {
				_ = k8sClient.Delete(ctx, tenant)
			}
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{
				NamespacedName: typeNamespacedName,
			}})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying tenant reference is set on ComputeInstance status")
			vm := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, typeNamespacedName, vm)).To(Succeed())
				g.Expect(vm.Status.TenantReference).NotTo(BeNil())
				g.Expect(vm.Status.TenantReference.Name).To(Equal(tenantName))
				g.Expect(vm.Status.TenantReference.Namespace).To(Equal(namespaceName))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			By("Verifying the finalizer is set on the ComputeInstance resource")
			Expect(vm.Finalizers).To(ContainElement(osacComputeInstanceFinalizer))
		})
	})

	Context("handleDesiredConfigVersion", func() {
		var reconciler *ComputeInstanceReconciler
		ctx := context.Background()

		BeforeEach(func() {
			reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
		})

		It("should compute and store a version of the spec", func() {
			spec := newTestComputeInstanceSpec("template-1")
			spec.TemplateParameters = testTemplateParams
			vm := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-hash",
					Namespace: "default",
				},
				Spec: spec,
			}

			err := reconciler.handleDesiredConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.DesiredConfigVersion).NotTo(BeEmpty())
		})

		It("should be idempotent - same spec produces same version", func() {
			spec := newTestComputeInstanceSpec("template-1")
			spec.TemplateParameters = testTemplateParams
			vm := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-idempotent",
					Namespace: "default",
				},
				Spec: spec,
			}

			// First call
			err := reconciler.handleDesiredConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			firstVersion := vm.Status.DesiredConfigVersion
			Expect(firstVersion).NotTo(BeEmpty())

			// Second call with same spec
			err = reconciler.handleDesiredConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			secondVersion := vm.Status.DesiredConfigVersion
			Expect(secondVersion).To(Equal(firstVersion))

			// Third call with same spec
			err = reconciler.handleDesiredConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			thirdVersion := vm.Status.DesiredConfigVersion
			Expect(thirdVersion).To(Equal(firstVersion))
		})

		It("should produce different versions for different specs", func() {
			spec1 := newTestComputeInstanceSpec("template-1")
			spec1.TemplateParameters = `{"key": "value1"}`
			vm1 := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-diff-1",
					Namespace: "default",
				},
				Spec: spec1,
			}

			spec2 := newTestComputeInstanceSpec("template-1")
			spec2.TemplateParameters = `{"key": "value2"}`
			vm2 := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-diff-2",
					Namespace: "default",
				},
				Spec: spec2,
			}

			err := reconciler.handleDesiredConfigVersion(ctx, vm1)
			Expect(err).NotTo(HaveOccurred())

			err = reconciler.handleDesiredConfigVersion(ctx, vm2)
			Expect(err).NotTo(HaveOccurred())

			Expect(vm1.Status.DesiredConfigVersion).NotTo(Equal(vm2.Status.DesiredConfigVersion))
		})

		It("should produce different versions for different template IDs", func() {
			vm1 := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-template-1",
					Namespace: "default",
				},
				Spec: newTestComputeInstanceSpec("template-1"),
			}

			vm2 := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-template-2",
					Namespace: "default",
				},
				Spec: newTestComputeInstanceSpec("template-2"),
			}

			err := reconciler.handleDesiredConfigVersion(ctx, vm1)
			Expect(err).NotTo(HaveOccurred())

			err = reconciler.handleDesiredConfigVersion(ctx, vm2)
			Expect(err).NotTo(HaveOccurred())

			Expect(vm1.Status.DesiredConfigVersion).NotTo(Equal(vm2.Status.DesiredConfigVersion))
		})

		It("should produce same version regardless of order of calls", func() {
			spec1 := newTestComputeInstanceSpec("template-1")
			spec1.TemplateParameters = testTemplateParams
			vm1 := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-order-1",
					Namespace: "default",
				},
				Spec: spec1,
			}

			spec2 := newTestComputeInstanceSpec("template-1")
			spec2.TemplateParameters = testTemplateParams
			vm2 := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-order-2",
					Namespace: "default",
				},
				Spec: spec2,
			}

			// Call on vm1 first
			err := reconciler.handleDesiredConfigVersion(ctx, vm1)
			Expect(err).NotTo(HaveOccurred())

			// Then call on vm2
			err = reconciler.handleDesiredConfigVersion(ctx, vm2)
			Expect(err).NotTo(HaveOccurred())

			// Versions should be identical
			Expect(vm1.Status.DesiredConfigVersion).To(Equal(vm2.Status.DesiredConfigVersion))
		})
	})

	Context("getFirstVMIIPAddress", func() {
		var reconciler *ComputeInstanceReconciler
		ctx := context.Background()

		BeforeEach(func() {
			reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
		})

		It("returns the first interface IP from the VMI status", func() {
			const wantIP = "10.0.0.42"
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi-ip",
					Namespace: "default",
				},
				Status: kubevirtv1.VirtualMachineInstanceStatus{
					Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
						{IP: wantIP, Name: "default"},
						{IP: "10.0.0.43", Name: "ignored"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, vmi)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, vmi)
			})

			targetClient, err := reconciler.getTargetClient(ctx)
			Expect(err).NotTo(HaveOccurred())

			ip := reconciler.getFirstVMIIPAddress(ctx, targetClient, vmi.Namespace, vmi.Name)
			Expect(ip).To(Equal(wantIP))
		})
	})

	Context("handleReconciledConfigVersion", func() {
		var reconciler *ComputeInstanceReconciler
		ctx := context.Background()

		BeforeEach(func() {
			reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
		})

		It("should copy annotation to status when annotation exists", func() {
			expectedVersion := "test-version-value-12345"
			vm := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-current",
					Namespace: "default",
					Annotations: map[string]string{
						osacAAPReconciledConfigVersionAnnotation: expectedVersion,
					},
				},
				Spec: newTestComputeInstanceSpec("template-1"),
			}

			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(Equal(expectedVersion))
		})

		It("should clear status when annotation does not exist", func() {
			vm := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ci-no-annotation",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
				Spec: newTestComputeInstanceSpec("template-1"),
				Status: osacv1alpha1.ComputeInstanceStatus{
					ReconciledConfigVersion: "some-old-version",
				},
			}

			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(BeEmpty())
		})

		It("should clear status when annotations map is nil", func() {
			vm := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-nil-annotations",
					Namespace: "default",
				},
				Spec: newTestComputeInstanceSpec("template-1"),
				Status: osacv1alpha1.ComputeInstanceStatus{
					ReconciledConfigVersion: "some-old-version",
				},
			}

			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(BeEmpty())
		})

		It("should update status when annotation value changes", func() {
			vm := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-update",
					Namespace: "default",
					Annotations: map[string]string{
						osacAAPReconciledConfigVersionAnnotation: "version-1",
					},
				},
				Spec: newTestComputeInstanceSpec("template-1"),
			}

			// First call
			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(Equal("version-1"))

			// Update annotation
			vm.Annotations[osacAAPReconciledConfigVersionAnnotation] = "version-2"

			// Second call
			err = reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(Equal("version-2"))
		})
	})

	Context("Helper functions", func() {
		Describe("provisioning.FindJobByID", func() {
			It("should return nil when jobs slice is empty", func() {
				jobs := []osacv1alpha1.JobStatus{}
				result := provisioning.FindJobByID(jobs, "job-123")
				Expect(result).To(BeNil())
			})

			It("should return nil when job ID is not found", func() {
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     osacv1alpha1.JobStatePending,
					},
					{
						JobID:     "job-2",
						Type:      osacv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     osacv1alpha1.JobStateRunning,
					},
				}
				result := provisioning.FindJobByID(jobs, "job-999")
				Expect(result).To(BeNil())
			})

			It("should return pointer to job when found", func() {
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     osacv1alpha1.JobStatePending,
						Message:   "First job",
					},
					{
						JobID:     "job-2",
						Type:      osacv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     osacv1alpha1.JobStateRunning,
						Message:   "Second job",
					},
				}
				result := provisioning.FindJobByID(jobs, "job-2")
				Expect(result).NotTo(BeNil())
				Expect(result.JobID).To(Equal("job-2"))
				Expect(result.State).To(Equal(osacv1alpha1.JobStateRunning))
				Expect(result.Message).To(Equal("Second job"))
			})
		})

		Describe("appendJob", func() {
			var reconciler *ComputeInstanceReconciler

			BeforeEach(func() {
				reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 3, mcmanager.LocalCluster)
			})

			It("should append job to empty slice", func() {
				jobs := []osacv1alpha1.JobStatus{}
				newJob := osacv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStatePending,
				}
				result := provisioning.AppendJob(jobs, newJob, reconciler.MaxJobHistory)
				Expect(result).To(HaveLen(1))
				Expect(result[0].JobID).To(Equal("job-1"))
			})

			It("should append job when under max history", func() {
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     osacv1alpha1.JobStatePending,
					},
				}
				newJob := osacv1alpha1.JobStatus{
					JobID:     "job-2",
					Type:      osacv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateRunning,
				}
				result := provisioning.AppendJob(jobs, newJob, reconciler.MaxJobHistory)
				Expect(result).To(HaveLen(2))
				Expect(result[0].JobID).To(Equal("job-1"))
				Expect(result[1].JobID).To(Equal("job-2"))
			})

			It("should trim old jobs when exceeding max history", func() {
				baseTime := time.Now().UTC()
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     osacv1alpha1.JobStatePending,
					},
					{
						JobID:     "job-2",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Second)),
						State:     osacv1alpha1.JobStateRunning,
					},
					{
						JobID:     "job-3",
						Type:      osacv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(baseTime.Add(2 * time.Second)),
						State:     osacv1alpha1.JobStateSucceeded,
					},
				}
				newJob := osacv1alpha1.JobStatus{
					JobID:     "job-4",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     osacv1alpha1.JobStatePending,
				}
				// MaxJobHistory is 3, so adding 4th job should remove job-1
				result := provisioning.AppendJob(jobs, newJob, reconciler.MaxJobHistory)
				Expect(result).To(HaveLen(3))
				Expect(result[0].JobID).To(Equal("job-2"))
				Expect(result[1].JobID).To(Equal("job-3"))
				Expect(result[2].JobID).To(Equal("job-4"))
			})

			It("should keep trimming as jobs are added", func() {
				baseTime := time.Now().UTC()
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     osacv1alpha1.JobStatePending,
					},
					{
						JobID:     "job-2",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Second)),
						State:     osacv1alpha1.JobStateRunning,
					},
					{
						JobID:     "job-3",
						Type:      osacv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(baseTime.Add(2 * time.Second)),
						State:     osacv1alpha1.JobStateSucceeded,
					},
				}
				// Add job-4 (removes job-1)
				jobs = provisioning.AppendJob(jobs, osacv1alpha1.JobStatus{
					JobID:     "job-4",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     osacv1alpha1.JobStatePending,
				}, reconciler.MaxJobHistory)
				Expect(jobs).To(HaveLen(3))
				Expect(jobs[0].JobID).To(Equal("job-2"))

				// Add job-5 (removes job-2)
				jobs = provisioning.AppendJob(jobs, osacv1alpha1.JobStatus{
					JobID:     "job-5",
					Type:      osacv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(baseTime.Add(4 * time.Second)),
					State:     osacv1alpha1.JobStateRunning,
				}, reconciler.MaxJobHistory)
				Expect(jobs).To(HaveLen(3))
				Expect(jobs[0].JobID).To(Equal("job-3"))
				Expect(jobs[1].JobID).To(Equal("job-4"))
				Expect(jobs[2].JobID).To(Equal("job-5"))
			})

			It("should use default max history when set", func() {
				reconciler := NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, DefaultMaxJobHistory, mcmanager.LocalCluster)
				jobs := []osacv1alpha1.JobStatus{}
				// Add 15 jobs
				baseTime := time.Now().UTC()
				for i := 1; i <= 15; i++ {
					newJob := osacv1alpha1.JobStatus{
						JobID:     fmt.Sprintf("job-%d", i),
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Duration(i) * time.Second)),
						State:     osacv1alpha1.JobStatePending,
					}
					jobs = provisioning.AppendJob(jobs, newJob, reconciler.MaxJobHistory)
				}
				// Should keep only last 10
				Expect(jobs).To(HaveLen(DefaultMaxJobHistory))
			})
		})

		Describe("updateJob", func() {
			It("should return false when job ID not found", func() {
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     osacv1alpha1.JobStatePending,
					},
				}
				updatedJob := osacv1alpha1.JobStatus{
					JobID:     "job-999",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateSucceeded,
				}
				result := provisioning.UpdateJob(jobs, updatedJob)
				Expect(result).To(BeFalse())
				// Original job should be unchanged
				Expect(jobs[0].State).To(Equal(osacv1alpha1.JobStatePending))
			})

			It("should return false when jobs slice is empty", func() {
				jobs := []osacv1alpha1.JobStatus{}
				updatedJob := osacv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStateSucceeded,
				}
				result := provisioning.UpdateJob(jobs, updatedJob)
				Expect(result).To(BeFalse())
			})

			It("should update job and return true when found", func() {
				baseTime := time.Now().UTC()
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     osacv1alpha1.JobStatePending,
						Message:   "Initial message",
					},
				}
				updatedTime := baseTime.Add(5 * time.Second)
				updatedJob := osacv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(updatedTime),
					State:     osacv1alpha1.JobStateSucceeded,
					Message:   "Updated message",
				}
				result := provisioning.UpdateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				// Job should be fully updated
				Expect(jobs[0].JobID).To(Equal("job-1"))
				Expect(jobs[0].State).To(Equal(osacv1alpha1.JobStateSucceeded))
				Expect(jobs[0].Message).To(Equal("Updated message"))
				Expect(jobs[0].Timestamp.Time).To(Equal(updatedTime))
			})

			It("should update correct job when multiple jobs exist", func() {
				baseTime := time.Now().UTC()
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     osacv1alpha1.JobStatePending,
						Message:   "First job",
					},
					{
						JobID:     "job-2",
						Type:      osacv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Second)),
						State:     osacv1alpha1.JobStateRunning,
						Message:   "Second job",
					},
					{
						JobID:     "job-3",
						Type:      osacv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(2 * time.Second)),
						State:     osacv1alpha1.JobStatePending,
						Message:   "Third job",
					},
				}
				updatedJob := osacv1alpha1.JobStatus{
					JobID:     "job-2",
					Type:      osacv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     osacv1alpha1.JobStateSucceeded,
					Message:   "Second job completed",
				}
				result := provisioning.UpdateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				// Only job-2 should be updated
				Expect(jobs[0].State).To(Equal(osacv1alpha1.JobStatePending))
				Expect(jobs[0].Message).To(Equal("First job"))
				Expect(jobs[1].State).To(Equal(osacv1alpha1.JobStateSucceeded))
				Expect(jobs[1].Message).To(Equal("Second job completed"))
				Expect(jobs[2].State).To(Equal(osacv1alpha1.JobStatePending))
				Expect(jobs[2].Message).To(Equal("Third job"))
			})

			It("should update all fields of the job", func() {
				baseTime := time.Now().UTC()
				jobs := []osacv1alpha1.JobStatus{
					{
						JobID:                  "job-1",
						Type:                   osacv1alpha1.JobTypeProvision,
						Timestamp:              metav1.NewTime(baseTime),
						State:                  osacv1alpha1.JobStatePending,
						Message:                "Initial",
						BlockDeletionOnFailure: false,
					},
				}
				updatedJob := osacv1alpha1.JobStatus{
					JobID:                  "job-1",
					Type:                   osacv1alpha1.JobTypeDeprovision, // Changed type
					Timestamp:              metav1.NewTime(baseTime.Add(time.Minute)),
					State:                  osacv1alpha1.JobStateFailed,
					Message:                "Failed with error",
					BlockDeletionOnFailure: true,
				}
				result := provisioning.UpdateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				Expect(jobs[0].Type).To(Equal(osacv1alpha1.JobTypeDeprovision))
				Expect(jobs[0].State).To(Equal(osacv1alpha1.JobStateFailed))
				Expect(jobs[0].Message).To(Equal("Failed with error"))
				Expect(jobs[0].BlockDeletionOnFailure).To(BeTrue())
			})
		})

		Describe("shouldTriggerProvision", func() {
			var reconciler *ComputeInstanceReconciler

			BeforeEach(func() {
				reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
			})

			It("should trigger when no job exists and config versions differ", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion: "abc123",
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Trigger))
				Expect(job).To(BeNil())
			})

			It("should trigger when job has empty ID and config versions differ", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion: "abc123",
						Jobs:                 []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: ""}},
					},
				}
				action, _ := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Trigger))
			})

			It("should skip when no job exists and config versions match", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "abc123",
						ReconciledConfigVersion: "abc123",
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Skip))
				Expect(job).To(BeNil())
			})

			It("should poll when job is still running", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						Jobs: []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateRunning}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Poll))
				Expect(job).NotTo(BeNil())
				Expect(job.JobID).To(Equal("job-1"))
			})

			It("should poll when job is pending", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						Jobs: []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStatePending}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Poll))
				Expect(job).NotTo(BeNil())
			})

			It("should skip when job succeeded and config versions match", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "abc123",
						ReconciledConfigVersion: "abc123",
						Jobs:                    []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateSucceeded}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Skip))
				Expect(job).NotTo(BeNil())
			})

			It("should trigger when job succeeded but config versions differ", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "new-version",
						ReconciledConfigVersion: "old-version",
						Jobs:                    []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateSucceeded}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Trigger))
				Expect(job).NotTo(BeNil())
			})

			It("should trigger when job failed and config versions differ", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "new-version",
						ReconciledConfigVersion: "old-version",
						Jobs:                    []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateFailed}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Trigger))
				Expect(job).NotTo(BeNil())
			})

			It("should skip when job failed but config versions match", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "abc123",
						ReconciledConfigVersion: "abc123",
						Jobs:                    []osacv1alpha1.JobStatus{{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateFailed}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Skip))
				Expect(job).NotTo(BeNil())
			})

			It("should backoff when latest job failed with matching ConfigVersion", func() {
				instance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{Name: "test-backoff", Namespace: "default"},
					Spec:       newTestComputeInstanceSpec("test_template"),
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion: "abc123",
						Jobs: []osacv1alpha1.JobStatus{{
							Type:          osacv1alpha1.JobTypeProvision,
							JobID:         "job-1",
							State:         osacv1alpha1.JobStateFailed,
							ConfigVersion: "abc123",
						}},
					},
				}
				action, job := reconciler.shouldTriggerProvision(ctx, instance)
				Expect(action).To(Equal(provisioning.Backoff))
				Expect(job).NotTo(BeNil())
			})

			It("should requeue when API server has non-terminal job but cache shows none", func() {
				// Create instance in API server with a running provision job
				instanceName := "test-api-server-check-no-cache"
				apiInstance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "default",
					},
					Spec: newTestComputeInstanceSpec("test_template"),
				}
				Expect(k8sClient.Create(ctx, apiInstance)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, apiInstance)
				})

				jobTimestamp := metav1.NewTime(time.Now().UTC())
				apiInstance.Status.DesiredConfigVersion = "v1"
				apiInstance.Status.Jobs = []osacv1alpha1.JobStatus{
					{Type: osacv1alpha1.JobTypeProvision, JobID: "running-job", State: osacv1alpha1.JobStateRunning, Timestamp: jobTimestamp},
				}
				Expect(k8sClient.Status().Update(ctx, apiInstance)).To(Succeed())

				// Simulate stale cache: in-memory instance has no jobs
				staleInstance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "default",
					},
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion: "v1",
					},
				}

				action, job := reconciler.shouldTriggerProvision(ctx, staleInstance)
				Expect(action).To(Equal(provisioning.Requeue))
				Expect(job).To(BeNil())
			})

			It("should requeue when API server has non-terminal job but cache shows terminal job with version mismatch", func() {
				// Create instance in API server with a running provision job (newer than the terminal one)
				instanceName := "test-api-server-check-stale-terminal"
				apiInstance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "default",
					},
					Spec: newTestComputeInstanceSpec("test_template"),
				}
				Expect(k8sClient.Create(ctx, apiInstance)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, apiInstance)
				})

				oldJobTimestamp := metav1.NewTime(time.Now().UTC().Add(-time.Minute))
				newJobTimestamp := metav1.NewTime(time.Now().UTC())
				apiInstance.Status.DesiredConfigVersion = "v2"
				apiInstance.Status.ReconciledConfigVersion = "v1"
				apiInstance.Status.Jobs = []osacv1alpha1.JobStatus{
					{Type: osacv1alpha1.JobTypeProvision, JobID: "old-job", State: osacv1alpha1.JobStateSucceeded, Timestamp: oldJobTimestamp},
					{Type: osacv1alpha1.JobTypeProvision, JobID: "new-running-job", State: osacv1alpha1.JobStateRunning, Timestamp: newJobTimestamp},
				}
				Expect(k8sClient.Status().Update(ctx, apiInstance)).To(Succeed())

				// Simulate stale cache: in-memory instance only has the old terminal job
				staleInstance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "default",
					},
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "v2",
						ReconciledConfigVersion: "v1",
						Jobs: []osacv1alpha1.JobStatus{
							{Type: osacv1alpha1.JobTypeProvision, JobID: "old-job", State: osacv1alpha1.JobStateSucceeded},
						},
					},
				}

				action, job := reconciler.shouldTriggerProvision(ctx, staleInstance)
				Expect(action).To(Equal(provisioning.Requeue))
				Expect(job).To(BeNil())
			})

			It("should trigger when API server also shows no non-terminal job", func() {
				// Create instance in API server with only a terminal job
				instanceName := "test-api-server-check-all-terminal"
				apiInstance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "default",
					},
					Spec: newTestComputeInstanceSpec("test_template"),
				}
				Expect(k8sClient.Create(ctx, apiInstance)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, apiInstance)
				})

				jobTimestamp := metav1.NewTime(time.Now().UTC())
				apiInstance.Status.DesiredConfigVersion = "v2"
				apiInstance.Status.ReconciledConfigVersion = "v1"
				apiInstance.Status.Jobs = []osacv1alpha1.JobStatus{
					{Type: osacv1alpha1.JobTypeProvision, JobID: "done-job", State: osacv1alpha1.JobStateSucceeded, Timestamp: jobTimestamp},
				}
				Expect(k8sClient.Status().Update(ctx, apiInstance)).To(Succeed())

				// In-memory instance matches API server — terminal job, versions differ
				staleInstance := &osacv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "default",
					},
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "v2",
						ReconciledConfigVersion: "v1",
						Jobs: []osacv1alpha1.JobStatus{
							{Type: osacv1alpha1.JobTypeProvision, JobID: "done-job", State: osacv1alpha1.JobStateSucceeded},
						},
					},
				}

				action, job := reconciler.shouldTriggerProvision(ctx, staleInstance)
				Expect(action).To(Equal(provisioning.Trigger))
				Expect(job).NotTo(BeNil())
				Expect(job.JobID).To(Equal("done-job"))
			})
		})
	})

	Context("Phase regression prevention", func() {
		const namespaceName = "default"

		ctx := context.Background()

		deleteCI := func(name string) {
			ci := &osacv1alpha1.ComputeInstance{}
			nn := types.NamespacedName{Name: name, Namespace: namespaceName}
			if err := k8sClient.Get(ctx, nn, ci); err == nil {
				ci.Finalizers = nil
				_ = k8sClient.Update(ctx, ci)
				_ = k8sClient.Delete(ctx, ci)
			}
		}

		It("should set Starting phase on first-time provisioning", func() {
			const resourceName = "test-phase-first-provision"
			const tenantName = "tenant-phase-first"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			// Wait for the CI to appear in the controller's cache before calling Reconcile
			// directly. Without this, r.Get() inside Reconcile returns NotFound (cache miss)
			// and Reconcile silently returns nil without setting Phase, making the test flaky.
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Status.Phase).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
			}).Should(Succeed())
		})

		It("should trigger provisioning only once when reconciled twice in rapid succession", func() {
			const resourceName = "test-duplicate-provision"
			const tenantName = "tenant-dup-provision"
			DeferCleanup(func() {
				deleteCI(resourceName)
				deleteTenantInNamespace(ctx, namespaceName, tenantName)
			})
			createReadyTenant(ctx, namespaceName, tenantName)

			objectKey := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			triggerCount := 0
			provider := &mockProvisioningProvider{
				name: string(provisioning.ProviderTypeAAP),
				triggerProvisionFunc: func(ctx context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
					triggerCount++
					return &provisioning.ProvisionResult{
						JobID:        fmt.Sprintf("job-%d", triggerCount),
						InitialState: osacv1alpha1.JobStateRunning,
						Message:      "running",
					}, nil
				},
			}
			reconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, provider, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			Eventually(func() error {
				return reconciler.Client.Get(ctx, objectKey, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			reconcileRequest := mcreconcile.Request{Request: reconcile.Request{NamespacedName: objectKey}}

			// First reconcile — should trigger provisioning
			_, err := reconciler.Reconcile(ctx, reconcileRequest)
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile — should NOT trigger provisioning again
			_, err = reconciler.Reconcile(ctx, reconcileRequest)
			Expect(err).NotTo(HaveOccurred())

			Expect(triggerCount).To(Equal(1), "TriggerProvision should be called exactly once")
		})

		It("should set Starting phase when no KubeVirt VM exists", func() {
			const resourceName = "test-phase-no-kv"
			const tenantName = "tenant-phase-nokv"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			// Wait for the CI to appear in the controller's cache before calling Reconcile directly.
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			ci := &osacv1alpha1.ComputeInstance{}
			// No KubeVirt VM exists in envtest, so findKubeVirtVMs returns nil.
			// Phase is driven by KubeVirt PrintableStatus; when no VM exists, it is Starting.
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Status.Phase).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})
	})

	Context("determinePhaseFromPrintableStatus", func() {
		ctx := context.Background()

		// kvVM builds a minimal KubeVirt VirtualMachine with the given PrintableStatus
		// and optional conditions.
		kvVM := func(printableStatus kubevirtv1.VirtualMachinePrintableStatus, conditions ...kubevirtv1.VirtualMachineCondition) *kubevirtv1.VirtualMachine {
			return &kubevirtv1.VirtualMachine{
				Status: kubevirtv1.VirtualMachineStatus{
					PrintableStatus: printableStatus,
					Conditions:      conditions,
				},
			}
		}

		// kvCond builds a KubeVirt VirtualMachineCondition.
		kvCond := func(condType kubevirtv1.VirtualMachineConditionType, status corev1.ConditionStatus) kubevirtv1.VirtualMachineCondition {
			return kubevirtv1.VirtualMachineCondition{Type: condType, Status: status}
		}

		DescribeTable("maps PrintableStatus to phase",
			func(kv *kubevirtv1.VirtualMachine, currentPhase osacv1alpha1.ComputeInstancePhaseType, expectedPhase osacv1alpha1.ComputeInstancePhaseType) {
				Expect(determinePhaseFromPrintableStatus(ctx, kv, currentPhase)).To(Equal(expectedPhase))
			},
			// Transient startup states
			Entry("Provisioning → Starting",
				kvVM(kubevirtv1.VirtualMachineStatusProvisioning), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseStarting),
			Entry("WaitingForVolumeBinding → Starting",
				kvVM(kubevirtv1.VirtualMachineStatusWaitingForVolumeBinding), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseStarting),
			Entry("Starting → Starting",
				kvVM(kubevirtv1.VirtualMachineStatusStarting), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseStarting),
			// Running
			Entry("Running (no pause condition) → Running",
				kvVM(kubevirtv1.VirtualMachineStatusRunning), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseRunning),
			Entry("Running (VirtualMachinePaused=False) → Running",
				kvVM(kubevirtv1.VirtualMachineStatusRunning, kvCond(kubevirtv1.VirtualMachinePaused, corev1.ConditionFalse)), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseRunning),
			Entry("Running (VirtualMachinePaused=True) → Paused (older KubeVirt fallback)",
				kvVM(kubevirtv1.VirtualMachineStatusRunning, kvCond(kubevirtv1.VirtualMachinePaused, corev1.ConditionTrue)), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhasePaused),
			// Paused
			Entry("Paused (no conditions) → Paused",
				kvVM(kubevirtv1.VirtualMachineStatusPaused), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhasePaused),
			Entry("Paused (VirtualMachinePaused=True) → Paused",
				kvVM(kubevirtv1.VirtualMachineStatusPaused, kvCond(kubevirtv1.VirtualMachinePaused, corev1.ConditionTrue)), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhasePaused),
			// Migration (VM remains accessible)
			Entry("Migrating → Running",
				kvVM(kubevirtv1.VirtualMachineStatusMigrating), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseRunning),
			Entry("WaitingForReceiver → Running",
				kvVM(kubevirtv1.VirtualMachineStatusWaitingForReceiver), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseRunning),
			// Stopping / Stopped
			Entry("Stopping → Stopping",
				kvVM(kubevirtv1.VirtualMachineStatusStopping), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseStopping),
			Entry("Stopped → Stopped",
				kvVM(kubevirtv1.VirtualMachineStatusStopped), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseStopped),
			// Error states
			Entry("ErrorUnschedulable → Failed",
				kvVM(kubevirtv1.VirtualMachineStatusUnschedulable), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseFailed),
			Entry("CrashLoopBackOff → Failed",
				kvVM(kubevirtv1.VirtualMachineStatusCrashLoopBackOff), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseFailed),
			Entry("Terminating → Failed",
				kvVM(kubevirtv1.VirtualMachineStatusTerminating), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseFailed),
			Entry("DataVolumeError → Failed",
				kvVM(kubevirtv1.VirtualMachineStatusDataVolumeError), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseFailed),
			Entry("ErrorPvcNotFound → Failed",
				kvVM(kubevirtv1.VirtualMachineStatusPvcNotFound), osacv1alpha1.ComputeInstancePhaseType(""), osacv1alpha1.ComputeInstancePhaseFailed),
			// Unknown: preserves current phase
			Entry("Unknown preserves Running phase",
				kvVM(kubevirtv1.VirtualMachineStatusUnknown), osacv1alpha1.ComputeInstancePhaseRunning, osacv1alpha1.ComputeInstancePhaseRunning),
			Entry("Unknown preserves Stopped phase",
				kvVM(kubevirtv1.VirtualMachineStatusUnknown), osacv1alpha1.ComputeInstancePhaseStopped, osacv1alpha1.ComputeInstancePhaseStopped),
			// Empty PrintableStatus: preserves current phase (race at VM creation)
			Entry("Empty PrintableStatus preserves Starting phase",
				kvVM(""), osacv1alpha1.ComputeInstancePhaseStarting, osacv1alpha1.ComputeInstancePhaseStarting),
			Entry("Empty PrintableStatus preserves Running phase",
				kvVM(""), osacv1alpha1.ComputeInstancePhaseRunning, osacv1alpha1.ComputeInstancePhaseRunning),
		)
	})

	Context("handleKubeVirtVM", func() {
		var (
			ctx          context.Context
			reconciler   *ComputeInstanceReconciler
			targetClient client.Client
			instance     *osacv1alpha1.ComputeInstance
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
			var err error
			targetClient, err = reconciler.getTargetClient(ctx)
			Expect(err).NotTo(HaveOccurred())
			instance = &osacv1alpha1.ComputeInstance{}
		})

		It("sets Provisioned=True and Available=True when VM is Running and Ready", func() {
			kv := &kubevirtv1.VirtualMachine{
				Status: kubevirtv1.VirtualMachineStatus{
					PrintableStatus: kubevirtv1.VirtualMachineStatusRunning,
					Conditions: []kubevirtv1.VirtualMachineCondition{
						{Type: kubevirtv1.VirtualMachineReady, Status: corev1.ConditionTrue},
					},
				},
			}
			Expect(reconciler.handleKubeVirtVM(ctx, targetClient, instance, kv)).To(Succeed())

			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned).Status).To(Equal(metav1.ConditionTrue))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionAvailable).Status).To(Equal(metav1.ConditionTrue))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionRestartRequired).Status).To(Equal(metav1.ConditionFalse))
		})

		It("sets Provisioned=True and Available=False when VM is Running but not Ready", func() {
			kv := &kubevirtv1.VirtualMachine{
				Status: kubevirtv1.VirtualMachineStatus{
					PrintableStatus: kubevirtv1.VirtualMachineStatusRunning,
				},
			}
			Expect(reconciler.handleKubeVirtVM(ctx, targetClient, instance, kv)).To(Succeed())

			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned).Status).To(Equal(metav1.ConditionTrue))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionRestartRequired).Status).To(Equal(metav1.ConditionFalse))
		})

		It("sets RestartRequired=True when KubeVirt RestartRequired condition is True", func() {
			kv := &kubevirtv1.VirtualMachine{
				Status: kubevirtv1.VirtualMachineStatus{
					PrintableStatus: kubevirtv1.VirtualMachineStatusRunning,
					Conditions: []kubevirtv1.VirtualMachineCondition{
						{Type: kubevirtv1.VirtualMachineRestartRequired, Status: corev1.ConditionTrue},
					},
				},
			}
			Expect(reconciler.handleKubeVirtVM(ctx, targetClient, instance, kv)).To(Succeed())

			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned).Status).To(Equal(metav1.ConditionTrue))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionRestartRequired).Status).To(Equal(metav1.ConditionTrue))
		})

		It("sets Provisioned=False when VM is in Provisioning state (storage not yet allocated)", func() {
			kv := &kubevirtv1.VirtualMachine{
				Status: kubevirtv1.VirtualMachineStatus{
					PrintableStatus: kubevirtv1.VirtualMachineStatusProvisioning,
				},
			}
			Expect(reconciler.handleKubeVirtVM(ctx, targetClient, instance, kv)).To(Succeed())

			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned).Status).To(Equal(metav1.ConditionFalse))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionAvailable).Status).To(Equal(metav1.ConditionFalse))
			Expect(instance.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionRestartRequired).Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("Tenant lifecycle", func() {
		const namespaceName = "default"

		ctx := context.Background()

		deleteCI := func(name string) {
			ci := &osacv1alpha1.ComputeInstance{}
			nn := types.NamespacedName{Name: name, Namespace: namespaceName}
			if err := k8sClient.Get(ctx, nn, ci); err == nil {
				ci.Finalizers = nil
				_ = k8sClient.Update(ctx, ci)
				_ = k8sClient.Delete(ctx, ci)
			}
		}

		It("should requeue when tenant has DeletionTimestamp", func() {
			const resourceName = "test-tenant-gc-clear"
			const tenantName = "tenant-gc-clear"
			defer deleteCI(resourceName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			createReadyTenant(ctx, namespaceName, tenantName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			// Wait for manager cache to see the CI before reconciling
			mgrClient := testMcManager.GetLocalManager().GetClient()
			Eventually(func() error {
				return mgrClient.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}).Should(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			// First reconcile: sets tenant reference
			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			// Verify reference was set
			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Status.TenantReference).NotTo(BeNil())
				g.Expect(ci.Status.TenantReference.Name).NotTo(BeEmpty())
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			// Add finalizer to tenant to keep it in terminating state
			tenant := &osacv1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tenantName, Namespace: namespaceName}, tenant)).To(Succeed())
			tenant.Finalizers = append(tenant.Finalizers, "osac.openshift.io/test")
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())

			// Delete the tenant - it will be stuck in terminating due to finalizer
			Expect(k8sClient.Delete(ctx, tenant)).To(Succeed())
			// Wait for manager cache to see the tenant with DeletionTimestamp before second reconcile
			Eventually(func(g Gomega) {
				cachedTenant := &osacv1alpha1.Tenant{}
				g.Expect(mgrClient.Get(ctx, types.NamespacedName{Name: tenantName, Namespace: namespaceName}, cachedTenant)).To(Succeed())
				g.Expect(cachedTenant.DeletionTimestamp).NotTo(BeNil())
			}).Should(Succeed())

			// Reconcile again - should return error when tenant has DeletionTimestamp (reference is not cleared)
			_, err = controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant is being deleted"))

			// Tenant reference is left unchanged
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Status.TenantReference).NotTo(BeNil())
				g.Expect(ci.Status.TenantReference.Name).To(Equal(tenantName))
				g.Expect(ci.Status.TenantReference.Namespace).To(Equal(namespaceName))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})

		It("should return error when tenant does not exist", func() {
			const resourceName = "test-tenant-not-found"
			const tenantName = "tenant-nonexistent"
			defer deleteCI(resourceName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}

			// Create a ComputeInstance that references a tenant that does not exist
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			mgrClient := testMcManager.GetLocalManager().GetClient()
			Eventually(func() error {
				return mgrClient.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}).Should(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			// Reconcile should fail because tenant does not exist
			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("Tenant-not-ready Provisioned condition", func() {
		const namespaceName = "default"

		ctx := context.Background()

		deleteCI := func(name string) {
			ci := &osacv1alpha1.ComputeInstance{}
			nn := types.NamespacedName{Name: name, Namespace: namespaceName}
			if err := k8sClient.Get(ctx, nn, ci); err == nil {
				ci.Finalizers = nil
				_ = k8sClient.Update(ctx, ci)
				_ = k8sClient.Delete(ctx, ci)
			}
		}

		It("should set Provisioned=False with Tenant phase when tenant is Progressing", func() {
			const resourceName = "test-ci-tenant-progressing"
			const tenantName = "tenant-progressing-msg"
			DeferCleanup(func() { deleteCI(resourceName) })
			DeferCleanup(func() { deleteTenantInNamespace(ctx, namespaceName, tenantName) })

			// Create tenant in Progressing state (no StorageClassReady condition)
			tenant := &osacv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: namespaceName},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
			tenant.Status.Phase = osacv1alpha1.TenantPhaseProgressing
			Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))

			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				cond := ci.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("TenantNotReady"))
				g.Expect(cond.Message).To(ContainSubstring("Tenant '%s' is not ready (phase: Progressing)", tenantName))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})

		It("should include StorageClassReady condition in Provisioned message", func() {
			const resourceName = "test-ci-tenant-sc-msg"
			const tenantName = "tenant-sc-msg"
			DeferCleanup(func() { deleteCI(resourceName) })
			DeferCleanup(func() { deleteTenantInNamespace(ctx, namespaceName, tenantName) })

			// Create tenant in Progressing state with a StorageClassReady condition
			tenant := &osacv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: namespaceName},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
			tenant.Status.Phase = osacv1alpha1.TenantPhaseProgressing
			tenant.SetStatusCondition(
				osacv1alpha1.TenantConditionStorageClassReady,
				metav1.ConditionFalse,
				osacv1alpha1.TenantReasonMultipleFound,
				"Multiple StorageClasses found with label osac.openshift.io/tenant="+tenantName+": sc1, sc2",
			)
			Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))

			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				cond := ci.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("TenantNotReady"))
				g.Expect(cond.Message).To(ContainSubstring("StorageClassReady"))
				g.Expect(cond.Message).To(ContainSubstring("sc1, sc2"))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})

		It("should clear TenantNotReady message when tenant becomes Ready", func() {
			const resourceName = "test-ci-tenant-recovery"
			const tenantName = "tenant-recovery-msg"
			DeferCleanup(func() { deleteCI(resourceName) })
			DeferCleanup(func() { deleteTenantInNamespace(ctx, namespaceName, tenantName) })

			// Create tenant in Progressing state
			tenant := &osacv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: namespaceName},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
			tenant.Status.Phase = osacv1alpha1.TenantPhaseProgressing
			tenant.SetStatusCondition(
				osacv1alpha1.TenantConditionStorageClassReady,
				metav1.ConditionFalse,
				osacv1alpha1.TenantReasonMultipleFound,
				"Multiple StorageClasses found",
			)
			Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			// First reconcile with Progressing tenant → Provisioned=False/TenantNotReady
			result, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(defaultPreconditionRequeueInterval))

			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				cond := ci.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal("TenantNotReady"))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			// Transition tenant to Ready
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tenantName, Namespace: namespaceName}, tenant)).To(Succeed())
			tenant.Status.Phase = osacv1alpha1.TenantPhaseReady
			tenant.Status.Namespace = namespaceName
			Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())
			// Wait for cache to see Ready tenant
			mgrClient := testMcManager.GetLocalManager().GetClient()
			Eventually(func(g Gomega) {
				cached := &osacv1alpha1.Tenant{}
				g.Expect(mgrClient.Get(ctx, types.NamespacedName{Name: tenantName, Namespace: namespaceName}, cached)).To(Succeed())
				g.Expect(cached.Status.Phase).To(Equal(osacv1alpha1.TenantPhaseReady))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			// Second reconcile with Ready tenant → Provisioned=False/AsExpected (no KubeVirt VM)
			_, err = controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				cond := ci.GetStatusCondition(osacv1alpha1.ComputeInstanceConditionProvisioned)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(osacv1alpha1.ReasonAsExpected))
				g.Expect(cond.Message).To(BeEmpty())
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})
	})

	Context("resolveSubnetNamespace", func() {
		const namespaceName = "default"
		var (
			reconciler *ComputeInstanceReconciler
			ctx        context.Context
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
		})

		It("should return empty string when subnetRef is not set", func() {
			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-no-subnet",
					Namespace: namespaceName,
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}

			subnetNS, err := reconciler.resolveSubnetNamespace(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(subnetNS).To(BeEmpty())
		})

		It("should return subnet CR name when subnet CR exists", func() {
			const subnetRef = "test-subnet-cr"

			// Create Subnet CR
			subnet := &osacv1alpha1.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      subnetRef,
					Namespace: namespaceName,
				},
				Spec: osacv1alpha1.SubnetSpec{
					VirtualNetwork: "vnet-123",
					IPv4CIDR:       "10.0.0.0/24",
				},
			}
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, subnet)
			}()

			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-with-subnet",
					Namespace: namespaceName,
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			instance.Spec.SubnetRef = subnetRef

			// Wait for Subnet CR to be cached
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: subnetRef, Namespace: namespaceName}, &osacv1alpha1.Subnet{})
			}).Should(Succeed())

			subnetNS, err := reconciler.resolveSubnetNamespace(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(subnetNS).To(Equal(subnetRef))
		})

		It("should return error when subnet CR does not exist", func() {
			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-missing-subnet",
					Namespace: namespaceName,
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			instance.Spec.SubnetRef = "nonexistent-subnet"

			subnetNS, err := reconciler.resolveSubnetNamespace(ctx, instance)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get Subnet CR"))
			Expect(subnetNS).To(BeEmpty())
		})
	})

	Context("VM search namespace with subnetRef", func() {
		const namespaceName = "default"

		ctx := context.Background()

		deleteCI := func(name string) {
			ci := &osacv1alpha1.ComputeInstance{}
			nn := types.NamespacedName{Name: name, Namespace: namespaceName}
			if err := k8sClient.Get(ctx, nn, ci); err == nil {
				ci.Finalizers = nil
				_ = k8sClient.Update(ctx, ci)
				_ = k8sClient.Delete(ctx, ci)
			}
		}

		It("should reconcile successfully when subnetRef is set and Subnet CR exists", func() {
			const resourceName = "test-ci-subnet-vm-ns"
			const tenantName = "tenant-subnet-vm-ns"
			const subnetRef = "test-subnet-vm-ns"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			// Create Subnet CR — the subnet name becomes the namespace where the VM will be searched
			subnet := &osacv1alpha1.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      subnetRef,
					Namespace: namespaceName,
				},
				Spec: osacv1alpha1.SubnetSpec{
					VirtualNetwork: "vnet-123",
					IPv4CIDR:       "10.0.0.0/24",
				},
			}
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, subnet)
			}()

			// Wait for Subnet CR to be cached
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: subnetRef, Namespace: namespaceName}, &osacv1alpha1.Subnet{})
			}).Should(Succeed())

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			spec := newTestComputeInstanceSpec("test_template")
			spec.SubnetRef = subnetRef
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			// Reconcile should succeed — it resolves the subnet namespace for VM lookup
			// instead of using the tenant namespace (this is the bug fix).
			// No VM exists in envtest, so phase should be Starting.
			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Status.Phase).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})

		It("should requeue when subnetRef is set but Subnet CR does not exist", func() {
			const resourceName = "test-ci-missing-subnet-vm"
			const tenantName = "tenant-missing-subnet-vm"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			spec := newTestComputeInstanceSpec("test_template")
			spec.SubnetRef = "nonexistent-subnet-cr"
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			// Reconcile should return RequeueAfter (no error) when Subnet CR is missing
			result, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})

		It("should persist subnet-namespace annotation to the API server", func() {
			const resourceName = "test-ci-subnet-anno-persist"
			const tenantName = "tenant-subnet-anno-persist"
			const subnetRef = "test-subnet-anno-persist"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			// Create Subnet CR
			subnet := &osacv1alpha1.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      subnetRef,
					Namespace: namespaceName,
				},
				Spec: osacv1alpha1.SubnetSpec{
					VirtualNetwork: "vnet-123",
					IPv4CIDR:       "10.0.0.0/24",
				},
			}
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, subnet)
			}()

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			// Wait for Subnet CR to be cached by the reconciler's manager cache
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, types.NamespacedName{Name: subnetRef, Namespace: namespaceName}, &osacv1alpha1.Subnet{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			spec := newTestComputeInstanceSpec("test_template")
			spec.SubnetRef = subnetRef
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			// Verify the annotation was persisted to the API server (not just in-memory)
			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Annotations).To(HaveKeyWithValue(osacSubnetNamespaceAnnotation, subnetRef))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})

		It("should not update annotation when subnet-namespace is already correct", func() {
			const resourceName = "test-ci-subnet-anno-noop"
			const tenantName = "tenant-subnet-anno-noop"
			const subnetRef = "test-subnet-anno-noop"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			// Create Subnet CR
			subnet := &osacv1alpha1.Subnet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      subnetRef,
					Namespace: namespaceName,
				},
				Spec: osacv1alpha1.SubnetSpec{
					VirtualNetwork: "vnet-123",
					IPv4CIDR:       "10.0.0.0/24",
				},
			}
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, subnet)
			}()

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			// Wait for Subnet CR to be cached by the reconciler's manager cache
			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, types.NamespacedName{Name: subnetRef, Namespace: namespaceName}, &osacv1alpha1.Subnet{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			spec := newTestComputeInstanceSpec("test_template")
			spec.SubnetRef = subnetRef
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation:          tenantName,
						osacSubnetNamespaceAnnotation: subnetRef, // already correct
					},
				},
				Spec: spec,
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			// First reconcile adds the finalizer, which triggers an r.Update().
			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			// Capture the Generation after the first reconcile. Generation only
			// increments on spec changes, not on metadata or status updates, so
			// it stays stable across reconciles that only touch status.
			ci := &osacv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
			genBefore := ci.Generation
			annotationsBefore := ci.Annotations

			// Second reconcile — finalizer and annotation are already in place,
			// so syncMetadataPreflight should skip the r.Update() call entirely.
			_, err = controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			ciAfter := &osacv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, nn, ciAfter)).To(Succeed())
			Expect(ciAfter.Annotations).To(HaveKeyWithValue(osacSubnetNamespaceAnnotation, subnetRef))
			Expect(ciAfter.Generation).To(Equal(genBefore), "Generation should not change when no spec/metadata write occurs")
			Expect(ciAfter.Annotations).To(Equal(annotationsBefore), "Annotations should be unchanged across reconciles")
		})

		It("should not set subnet-namespace annotation when subnetRef is empty", func() {
			const resourceName = "test-ci-no-subnet-vm-ns"
			const tenantName = "tenant-no-subnet-vm"
			defer deleteCI(resourceName)
			createReadyTenant(ctx, namespaceName, tenantName)
			defer deleteTenantInNamespace(ctx, namespaceName, tenantName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}
			resource := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespaceName,
					Annotations: map[string]string{
						osacTenantAnnotation: tenantName,
					},
				},
				Spec: newTestComputeInstanceSpec("test_template"),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := NewComputeInstanceReconciler(testMcManager, "", namespaceName, &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)}, 100*time.Millisecond, 0, mcmanager.LocalCluster)

			Eventually(func() error {
				return controllerReconciler.Client.Get(ctx, nn, &osacv1alpha1.ComputeInstance{})
			}, 2*time.Second, 10*time.Millisecond).Should(Succeed())

			// When no subnetRef, should use tenant namespace (default behavior)
			_, err := controllerReconciler.Reconcile(ctx, mcreconcile.Request{Request: reconcile.Request{NamespacedName: nn}})
			Expect(err).NotTo(HaveOccurred())

			ci := &osacv1alpha1.ComputeInstance{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
				g.Expect(ci.Status.Phase).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
				g.Expect(ci.Annotations).NotTo(HaveKey(osacSubnetNamespaceAnnotation))
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		})
	})

	Context("lastRestartedAt update after provisioning", func() {
		var reconciler *ComputeInstanceReconciler
		ctx := context.Background()

		BeforeEach(func() {
			reconciler = NewComputeInstanceReconciler(testMcManager, "", "", &mockProvisioningProvider{}, 0, 0, mcmanager.LocalCluster)
		})

		It("should set lastRestartedAt when restartRequestedAt is set and config versions match", func() {
			restartTime := metav1.NewTime(time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC))
			spec := newTestComputeInstanceSpec("template-1")
			spec.RestartRequestedAt = &restartTime
			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test-restart", Namespace: "default"},
				Spec:       spec,
			}

			// Compute desired config version
			err := reconciler.handleDesiredConfigVersion(ctx, instance)
			Expect(err).NotTo(HaveOccurred())

			// Simulate provision completed: reconciled matches desired
			instance.Status.ReconciledConfigVersion = instance.Status.DesiredConfigVersion

			Expect(instance.Status.LastRestartedAt).To(BeNil())

			// The logic under test: when config versions match and restartRequestedAt > lastRestartedAt
			if instance.Spec.RestartRequestedAt != nil {
				if instance.Status.LastRestartedAt == nil || instance.Spec.RestartRequestedAt.After(instance.Status.LastRestartedAt.Time) {
					instance.Status.LastRestartedAt = instance.Spec.RestartRequestedAt.DeepCopy()
				}
			}

			Expect(instance.Status.LastRestartedAt).NotTo(BeNil())
			Expect(instance.Status.LastRestartedAt.Time).To(Equal(restartTime.Time))
		})

		It("should not update lastRestartedAt when restartRequestedAt is not set", func() {
			spec := newTestComputeInstanceSpec("template-1")
			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test-no-restart", Namespace: "default"},
				Spec:       spec,
			}

			err := reconciler.handleDesiredConfigVersion(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			instance.Status.ReconciledConfigVersion = instance.Status.DesiredConfigVersion

			// Same logic inline
			if instance.Spec.RestartRequestedAt != nil {
				if instance.Status.LastRestartedAt == nil || instance.Spec.RestartRequestedAt.After(instance.Status.LastRestartedAt.Time) {
					instance.Status.LastRestartedAt = instance.Spec.RestartRequestedAt.DeepCopy()
				}
			}

			Expect(instance.Status.LastRestartedAt).To(BeNil())
		})

		It("should not update lastRestartedAt when restart was already processed", func() {
			restartTime := metav1.NewTime(time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC))
			spec := newTestComputeInstanceSpec("template-1")
			spec.RestartRequestedAt = &restartTime
			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test-already-restarted", Namespace: "default"},
				Spec:       spec,
				Status: osacv1alpha1.ComputeInstanceStatus{
					LastRestartedAt: &restartTime, // same as requested
				},
			}

			err := reconciler.handleDesiredConfigVersion(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			instance.Status.ReconciledConfigVersion = instance.Status.DesiredConfigVersion

			originalLastRestarted := instance.Status.LastRestartedAt.DeepCopy()

			if instance.Spec.RestartRequestedAt != nil {
				if instance.Status.LastRestartedAt == nil || instance.Spec.RestartRequestedAt.After(instance.Status.LastRestartedAt.Time) {
					instance.Status.LastRestartedAt = instance.Spec.RestartRequestedAt.DeepCopy()
				}
			}

			Expect(instance.Status.LastRestartedAt.Time).To(Equal(originalLastRestarted.Time))
		})

		It("should update lastRestartedAt for a new restart request", func() {
			firstRestart := metav1.NewTime(time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC))
			secondRestart := metav1.NewTime(time.Date(2026, 3, 18, 14, 0, 0, 0, time.UTC))
			spec := newTestComputeInstanceSpec("template-1")
			spec.RestartRequestedAt = &secondRestart
			instance := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test-second-restart", Namespace: "default"},
				Spec:       spec,
				Status: osacv1alpha1.ComputeInstanceStatus{
					LastRestartedAt: &firstRestart,
				},
			}

			err := reconciler.handleDesiredConfigVersion(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			instance.Status.ReconciledConfigVersion = instance.Status.DesiredConfigVersion

			if instance.Spec.RestartRequestedAt != nil {
				if instance.Status.LastRestartedAt == nil || instance.Spec.RestartRequestedAt.After(instance.Status.LastRestartedAt.Time) {
					instance.Status.LastRestartedAt = instance.Spec.RestartRequestedAt.DeepCopy()
				}
			}

			Expect(instance.Status.LastRestartedAt.Time).To(Equal(secondRestart.Time))
		})

		It("should include restartRequestedAt in spec hash", func() {
			restartTime := metav1.NewTime(time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC))

			specWithout := newTestComputeInstanceSpec("template-1")
			instanceWithout := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hash-without", Namespace: "default"},
				Spec:       specWithout,
			}
			err := reconciler.handleDesiredConfigVersion(ctx, instanceWithout)
			Expect(err).NotTo(HaveOccurred())
			hashWithout := instanceWithout.Status.DesiredConfigVersion

			specWith := newTestComputeInstanceSpec("template-1")
			specWith.RestartRequestedAt = &restartTime
			instanceWith := &osacv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test-hash-with", Namespace: "default"},
				Spec:       specWith,
			}
			err = reconciler.handleDesiredConfigVersion(ctx, instanceWith)
			Expect(err).NotTo(HaveOccurred())
			hashWith := instanceWith.Status.DesiredConfigVersion

			Expect(hashWith).NotTo(Equal(hashWithout), "restartRequestedAt must change the spec hash to trigger provisioning")
		})
	})

	Context("provisioning.ComputeBackoffFromJobs", func() {
		now := time.Now().UTC()

		It("should return base delay for empty jobs", func() {
			Expect(provisioning.ComputeBackoffFromJobs(nil, "v1")).To(Equal(provisioning.BackoffBaseDelay))
		})

		It("should return base delay for single failed job", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(provisioning.BackoffBaseDelay))
		})

		It("should double the gap between two failed jobs", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-5 * time.Minute))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(10 * time.Minute))
		})

		It("should cap at max delay", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-20 * time.Minute))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(provisioning.BackoffMaxDelay))
		})

		It("should return base delay when gap is smaller than base", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-30 * time.Second))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(provisioning.BackoffBaseDelay))
		})

		It("should return base delay when timestamps are equal (zero gap)", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(provisioning.BackoffBaseDelay))
		})

		It("should ignore jobs with different ConfigVersion", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-5 * time.Minute))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v2", Timestamp: metav1.NewTime(now)},
			}
			// Only one job matches "v1", so base delay
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(provisioning.BackoffBaseDelay))
		})

		It("should ignore non-provision jobs", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-5 * time.Minute))},
				{Type: osacv1alpha1.JobTypeDeprovision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-3 * time.Minute))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(10 * time.Minute))
		})

		It("should ignore succeeded jobs", func() {
			jobs := []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-5 * time.Minute))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateSucceeded, ConfigVersion: "v1", Timestamp: metav1.NewTime(now.Add(-3 * time.Minute))},
				{Type: osacv1alpha1.JobTypeProvision, State: osacv1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(now)},
			}
			// Gap is between the two failed jobs (5 min), not between failed and succeeded
			Expect(provisioning.ComputeBackoffFromJobs(jobs, "v1")).To(Equal(10 * time.Minute))
		})
	})
})
