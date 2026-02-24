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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

const testTemplateParams = `{"key": "value"}`

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
			By("creating the custom resource for the Kind ComputeInstance")
			err := k8sClient.Get(ctx, typeNamespacedName, computeInstance)
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
				controllerReconciler := &ComputeInstanceReconciler{
					Client:               k8sClient,
					Scheme:               k8sClient.Scheme(),
					ProvisioningProvider: &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)},
					StatusPollInterval:   100 * time.Millisecond,
				}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				return err
			}).Should(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ComputeInstanceReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)},
				StatusPollInterval:   100 * time.Millisecond,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking that a tenant was created")
			expectedTenantName := getTenantObjectName(tenantName)
			tenant := &osacv1alpha1.Tenant{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      expectedTenantName,
				Namespace: namespaceName,
			}, tenant)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying tenant has correct spec name")
			Expect(tenant.Spec.Name).To(Equal(tenantName))

			By("Verifying tenant has correct labels")
			Expect(tenant.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", osacAppName))

			By("Verifying tenant has owner reference to ComputeInstance")
			Expect(tenant.OwnerReferences).NotTo(BeEmpty())
			Expect(tenant.OwnerReferences[0].Name).To(Equal(resourceName))

			// verify that finalizer is set
			By("Verifying the finalizer is set on the ComputeInstance resource")
			vm := &osacv1alpha1.ComputeInstance{}
			err = k8sClient.Get(ctx, typeNamespacedName, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Finalizers).To(ContainElement(osacComputeInstanceFinalizer))

			By("Verifying tenant reference is set on ComputeInstance status")
			Expect(vm.Status.TenantReference).NotTo(BeNil())
			Expect(vm.Status.TenantReference.Name).To(Equal(tenant.Name))
			Expect(vm.Status.TenantReference.Namespace).To(Equal(tenant.Namespace))
		})
	})

	Context("handleDesiredConfigVersion", func() {
		var reconciler *ComputeInstanceReconciler
		ctx := context.Background()

		BeforeEach(func() {
			reconciler = &ComputeInstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
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

	Context("handleReconciledConfigVersion", func() {
		var reconciler *ComputeInstanceReconciler
		ctx := context.Background()

		BeforeEach(func() {
			reconciler = &ComputeInstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
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
		Describe("findJobByID", func() {
			It("should return nil when jobs slice is empty", func() {
				jobs := []osacv1alpha1.JobStatus{}
				result := findJobByID(jobs, "job-123")
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
				result := findJobByID(jobs, "job-999")
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
				result := findJobByID(jobs, "job-2")
				Expect(result).NotTo(BeNil())
				Expect(result.JobID).To(Equal("job-2"))
				Expect(result.State).To(Equal(osacv1alpha1.JobStateRunning))
				Expect(result.Message).To(Equal("Second job"))
			})
		})

		Describe("appendJob", func() {
			var reconciler *ComputeInstanceReconciler

			BeforeEach(func() {
				reconciler = &ComputeInstanceReconciler{
					MaxJobHistory: 3, // Use 3 for easier testing
				}
			})

			It("should append job to empty slice", func() {
				jobs := []osacv1alpha1.JobStatus{}
				newJob := osacv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     osacv1alpha1.JobStatePending,
				}
				result := reconciler.appendJob(jobs, newJob)
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
				result := reconciler.appendJob(jobs, newJob)
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
				result := reconciler.appendJob(jobs, newJob)
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
				jobs = reconciler.appendJob(jobs, osacv1alpha1.JobStatus{
					JobID:     "job-4",
					Type:      osacv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     osacv1alpha1.JobStatePending,
				})
				Expect(jobs).To(HaveLen(3))
				Expect(jobs[0].JobID).To(Equal("job-2"))

				// Add job-5 (removes job-2)
				jobs = reconciler.appendJob(jobs, osacv1alpha1.JobStatus{
					JobID:     "job-5",
					Type:      osacv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(baseTime.Add(4 * time.Second)),
					State:     osacv1alpha1.JobStateRunning,
				})
				Expect(jobs).To(HaveLen(3))
				Expect(jobs[0].JobID).To(Equal("job-3"))
				Expect(jobs[1].JobID).To(Equal("job-4"))
				Expect(jobs[2].JobID).To(Equal("job-5"))
			})

			It("should use default max history when set", func() {
				reconciler := &ComputeInstanceReconciler{
					MaxJobHistory: DefaultMaxJobHistory, // Use default (10)
				}
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
					jobs = reconciler.appendJob(jobs, newJob)
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
				result := updateJob(jobs, updatedJob)
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
				result := updateJob(jobs, updatedJob)
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
				result := updateJob(jobs, updatedJob)
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
				result := updateJob(jobs, updatedJob)
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
				result := updateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				Expect(jobs[0].Type).To(Equal(osacv1alpha1.JobTypeDeprovision))
				Expect(jobs[0].State).To(Equal(osacv1alpha1.JobStateFailed))
				Expect(jobs[0].Message).To(Equal("Failed with error"))
				Expect(jobs[0].BlockDeletionOnFailure).To(BeTrue())
			})
		})

		Describe("needsProvisionJob", func() {
			var reconciler *ComputeInstanceReconciler

			BeforeEach(func() {
				reconciler = &ComputeInstanceReconciler{}
			})

			It("should return true when no job exists", func() {
				instance := &osacv1alpha1.ComputeInstance{}
				Expect(reconciler.needsProvisionJob(instance, nil)).To(BeTrue())
			})

			It("should return true when job has empty ID", func() {
				instance := &osacv1alpha1.ComputeInstance{}
				job := &osacv1alpha1.JobStatus{JobID: ""}
				Expect(reconciler.needsProvisionJob(instance, job)).To(BeTrue())
			})

			It("should return false when job is still running", func() {
				instance := &osacv1alpha1.ComputeInstance{}
				job := &osacv1alpha1.JobStatus{
					JobID: "job-1",
					State: osacv1alpha1.JobStateRunning,
				}
				Expect(reconciler.needsProvisionJob(instance, job)).To(BeFalse())
			})

			It("should return false when job is pending", func() {
				instance := &osacv1alpha1.ComputeInstance{}
				job := &osacv1alpha1.JobStatus{
					JobID: "job-1",
					State: osacv1alpha1.JobStatePending,
				}
				Expect(reconciler.needsProvisionJob(instance, job)).To(BeFalse())
			})

			It("should return false when job succeeded and config versions match", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "abc123",
						ReconciledConfigVersion: "abc123",
					},
				}
				job := &osacv1alpha1.JobStatus{
					JobID: "job-1",
					State: osacv1alpha1.JobStateSucceeded,
				}
				Expect(reconciler.needsProvisionJob(instance, job)).To(BeFalse())
			})

			It("should return true when job succeeded but config versions differ", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "new-version",
						ReconciledConfigVersion: "old-version",
					},
				}
				job := &osacv1alpha1.JobStatus{
					JobID: "job-1",
					State: osacv1alpha1.JobStateSucceeded,
				}
				Expect(reconciler.needsProvisionJob(instance, job)).To(BeTrue())
			})

			It("should return true when job failed and config versions differ", func() {
				instance := &osacv1alpha1.ComputeInstance{
					Status: osacv1alpha1.ComputeInstanceStatus{
						DesiredConfigVersion:    "new-version",
						ReconciledConfigVersion: "old-version",
					},
				}
				job := &osacv1alpha1.JobStatus{
					JobID: "job-1",
					State: osacv1alpha1.JobStateFailed,
				}
				Expect(reconciler.needsProvisionJob(instance, job)).To(BeTrue())
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

			controllerReconciler := &ComputeInstanceReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)},
				StatusPollInterval:   100 * time.Millisecond,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			ci := &osacv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
			Expect(ci.Status.Phase).To(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
		})

		It("should not regress phase to Starting when ReconciledConfigVersion is set", func() {
			const resourceName = "test-phase-no-regress"
			const tenantName = "tenant-phase-noregress"
			defer deleteCI(resourceName)

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

			// Simulate a previously provisioned instance by setting status
			ci := &osacv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
			ci.Status.Phase = osacv1alpha1.ComputeInstancePhaseRunning
			ci.Status.ReconciledConfigVersion = "some-version"
			Expect(k8sClient.Status().Update(ctx, ci)).To(Succeed())

			controllerReconciler := &ComputeInstanceReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)},
				StatusPollInterval:   100 * time.Millisecond,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
			// Phase should NOT have regressed to Starting
			Expect(ci.Status.Phase).NotTo(Equal(osacv1alpha1.ComputeInstancePhaseStarting))
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

		deleteTenant := func(name string) {
			tenant := &osacv1alpha1.Tenant{}
			nn := types.NamespacedName{Name: name, Namespace: namespaceName}
			if err := k8sClient.Get(ctx, nn, tenant); err == nil {
				tenant.Finalizers = nil
				_ = k8sClient.Update(ctx, tenant)
				_ = k8sClient.Delete(ctx, tenant)
			}
		}

		It("should clear tenant reference and requeue when tenant has DeletionTimestamp", func() {
			const resourceName = "test-tenant-gc-clear"
			const tenantName = "tenant-gc-clear"
			tenantObjName := getTenantObjectName(tenantName)
			defer deleteCI(resourceName)
			defer deleteTenant(tenantObjName)

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

			controllerReconciler := &ComputeInstanceReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)},
				StatusPollInterval:   100 * time.Millisecond,
			}

			// First reconcile: creates tenant and sets reference
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Verify tenant was created and reference was set
			ci := &osacv1alpha1.ComputeInstance{}
			Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
			Expect(ci.Status.TenantReference.Name).NotTo(BeEmpty())

			// Add finalizer to tenant to keep it in terminating state
			tenant := &osacv1alpha1.Tenant{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tenantObjName, Namespace: namespaceName}, tenant)).To(Succeed())
			tenant.Finalizers = append(tenant.Finalizers, "osac.openshift.io/test")
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())

			// Delete the tenant - it will be stuck in terminating due to finalizer
			Expect(k8sClient.Delete(ctx, tenant)).To(Succeed())

			// Reconcile again - should detect terminating tenant
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))

			// Verify tenant reference was cleared
			Expect(k8sClient.Get(ctx, nn, ci)).To(Succeed())
			Expect(ci.Status.TenantReference.Name).To(BeEmpty())
			Expect(ci.Status.TenantReference.Namespace).To(BeEmpty())
		})

		It("should not create tenant when existing tenant with same name is being deleted", func() {
			const resourceName = "test-tenant-gc-block"
			const tenantName = "tenant-gc-block"
			tenantObjName := getTenantObjectName(tenantName)
			defer deleteCI(resourceName)
			defer deleteTenant(tenantObjName)

			nn := types.NamespacedName{Name: resourceName, Namespace: namespaceName}

			// Pre-create a tenant with a finalizer, then delete it to put it in terminating state
			tenant := &osacv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:       tenantObjName,
					Namespace:  namespaceName,
					Finalizers: []string{"osac.openshift.io/test"},
				},
				Spec: osacv1alpha1.TenantSpec{
					Name: tenantName,
				},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tenant)).To(Succeed())

			// Create a ComputeInstance that would use the same tenant name
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

			controllerReconciler := &ComputeInstanceReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{name: string(provisioning.ProviderTypeAAP)},
				StatusPollInterval:   100 * time.Millisecond,
			}

			// Reconcile should fail because createOrUpdateTenant detects the terminating tenant
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is being deleted"))
		})
	})
})
