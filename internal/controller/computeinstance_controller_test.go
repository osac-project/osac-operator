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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudkitv1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
	"github.com/osac/osac-operator/internal/provisioning"
)

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
		computeInstance := &cloudkitv1alpha1.ComputeInstance{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ComputeInstance")
			err := k8sClient.Get(ctx, typeNamespacedName, computeInstance)
			if err != nil && errors.IsNotFound(err) {
				resource := &cloudkitv1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespaceName,
						Annotations: map[string]string{
							cloudkitTenantAnnotation: tenantName,
						},
					},
					Spec: cloudkitv1alpha1.ComputeInstanceSpec{
						TemplateID: "test_template",
					},
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
			tenant := &cloudkitv1alpha1.Tenant{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      expectedTenantName,
				Namespace: namespaceName,
			}, tenant)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying tenant has correct spec name")
			Expect(tenant.Spec.Name).To(Equal(tenantName))

			By("Verifying tenant has correct labels")
			Expect(tenant.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", cloudkitAppName))

			By("Verifying tenant has owner reference to ComputeInstance")
			Expect(tenant.OwnerReferences).NotTo(BeEmpty())
			Expect(tenant.OwnerReferences[0].Name).To(Equal(resourceName))

			// verify that finalizer is set
			By("Verifying the finalizer is set on the ComputeInstance resource")
			vm := &cloudkitv1alpha1.ComputeInstance{}
			err = k8sClient.Get(ctx, typeNamespacedName, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Finalizers).To(ContainElement(cloudkitComputeInstanceFinalizer))

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
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-hash",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template-1",
					TemplateParameters: `{"key": "value"}`,
				},
			}

			err := reconciler.handleDesiredConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.DesiredConfigVersion).NotTo(BeEmpty())
		})

		It("should be idempotent - same spec produces same version", func() {
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-idempotent",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template-1",
					TemplateParameters: `{"key": "value"}`,
				},
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
			vm1 := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-diff-1",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template-1",
					TemplateParameters: `{"key": "value1"}`,
				},
			}

			vm2 := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-diff-2",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template-1",
					TemplateParameters: `{"key": "value2"}`,
				},
			}

			err := reconciler.handleDesiredConfigVersion(ctx, vm1)
			Expect(err).NotTo(HaveOccurred())

			err = reconciler.handleDesiredConfigVersion(ctx, vm2)
			Expect(err).NotTo(HaveOccurred())

			Expect(vm1.Status.DesiredConfigVersion).NotTo(Equal(vm2.Status.DesiredConfigVersion))
		})

		It("should produce different versions for different template IDs", func() {
			vm1 := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-template-1",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "template-1",
				},
			}

			vm2 := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-template-2",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "template-2",
				},
			}

			err := reconciler.handleDesiredConfigVersion(ctx, vm1)
			Expect(err).NotTo(HaveOccurred())

			err = reconciler.handleDesiredConfigVersion(ctx, vm2)
			Expect(err).NotTo(HaveOccurred())

			Expect(vm1.Status.DesiredConfigVersion).NotTo(Equal(vm2.Status.DesiredConfigVersion))
		})

		It("should produce same version regardless of order of calls", func() {
			vm1 := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-order-1",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template-1",
					TemplateParameters: `{"key": "value"}`,
				},
			}

			vm2 := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-order-2",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID:         "template-1",
					TemplateParameters: `{"key": "value"}`,
				},
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
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-current",
					Namespace: "default",
					Annotations: map[string]string{
						cloudkitAAPReconciledConfigVersionAnnotation: expectedVersion,
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "template-1",
				},
			}

			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(Equal(expectedVersion))
		})

		It("should clear status when annotation does not exist", func() {
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ci-no-annotation",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "template-1",
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					ReconciledConfigVersion: "some-old-version",
				},
			}

			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(BeEmpty())
		})

		It("should clear status when annotations map is nil", func() {
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-nil-annotations",
					Namespace: "default",
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "template-1",
				},
				Status: cloudkitv1alpha1.ComputeInstanceStatus{
					ReconciledConfigVersion: "some-old-version",
				},
			}

			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(BeEmpty())
		})

		It("should update status when annotation value changes", func() {
			vm := &cloudkitv1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ci-update",
					Namespace: "default",
					Annotations: map[string]string{
						cloudkitAAPReconciledConfigVersionAnnotation: "version-1",
					},
				},
				Spec: cloudkitv1alpha1.ComputeInstanceSpec{
					TemplateID: "template-1",
				},
			}

			// First call
			err := reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(Equal("version-1"))

			// Update annotation
			vm.Annotations[cloudkitAAPReconciledConfigVersionAnnotation] = "version-2"

			// Second call
			err = reconciler.handleReconciledConfigVersion(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(vm.Status.ReconciledConfigVersion).To(Equal("version-2"))
		})
	})

	Context("Helper functions", func() {
		Describe("findJobByID", func() {
			It("should return nil when jobs slice is empty", func() {
				jobs := []cloudkitv1alpha1.JobStatus{}
				result := findJobByID(jobs, "job-123")
				Expect(result).To(BeNil())
			})

			It("should return nil when job ID is not found", func() {
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     cloudkitv1alpha1.JobStatePending,
					},
					{
						JobID:     "job-2",
						Type:      cloudkitv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     cloudkitv1alpha1.JobStateRunning,
					},
				}
				result := findJobByID(jobs, "job-999")
				Expect(result).To(BeNil())
			})

			It("should return pointer to job when found", func() {
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     cloudkitv1alpha1.JobStatePending,
						Message:   "First job",
					},
					{
						JobID:     "job-2",
						Type:      cloudkitv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     cloudkitv1alpha1.JobStateRunning,
						Message:   "Second job",
					},
				}
				result := findJobByID(jobs, "job-2")
				Expect(result).NotTo(BeNil())
				Expect(result.JobID).To(Equal("job-2"))
				Expect(result.State).To(Equal(cloudkitv1alpha1.JobStateRunning))
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
				jobs := []cloudkitv1alpha1.JobStatus{}
				newJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStatePending,
				}
				result := reconciler.appendJob(jobs, newJob)
				Expect(result).To(HaveLen(1))
				Expect(result[0].JobID).To(Equal("job-1"))
			})

			It("should append job when under max history", func() {
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     cloudkitv1alpha1.JobStatePending,
					},
				}
				newJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-2",
					Type:      cloudkitv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStateRunning,
				}
				result := reconciler.appendJob(jobs, newJob)
				Expect(result).To(HaveLen(2))
				Expect(result[0].JobID).To(Equal("job-1"))
				Expect(result[1].JobID).To(Equal("job-2"))
			})

			It("should trim old jobs when exceeding max history", func() {
				baseTime := time.Now().UTC()
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     cloudkitv1alpha1.JobStatePending,
					},
					{
						JobID:     "job-2",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Second)),
						State:     cloudkitv1alpha1.JobStateRunning,
					},
					{
						JobID:     "job-3",
						Type:      cloudkitv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(baseTime.Add(2 * time.Second)),
						State:     cloudkitv1alpha1.JobStateSucceeded,
					},
				}
				newJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-4",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     cloudkitv1alpha1.JobStatePending,
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
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     cloudkitv1alpha1.JobStatePending,
					},
					{
						JobID:     "job-2",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Second)),
						State:     cloudkitv1alpha1.JobStateRunning,
					},
					{
						JobID:     "job-3",
						Type:      cloudkitv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(baseTime.Add(2 * time.Second)),
						State:     cloudkitv1alpha1.JobStateSucceeded,
					},
				}
				// Add job-4 (removes job-1)
				jobs = reconciler.appendJob(jobs, cloudkitv1alpha1.JobStatus{
					JobID:     "job-4",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     cloudkitv1alpha1.JobStatePending,
				})
				Expect(jobs).To(HaveLen(3))
				Expect(jobs[0].JobID).To(Equal("job-2"))

				// Add job-5 (removes job-2)
				jobs = reconciler.appendJob(jobs, cloudkitv1alpha1.JobStatus{
					JobID:     "job-5",
					Type:      cloudkitv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(baseTime.Add(4 * time.Second)),
					State:     cloudkitv1alpha1.JobStateRunning,
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
				jobs := []cloudkitv1alpha1.JobStatus{}
				// Add 15 jobs
				baseTime := time.Now().UTC()
				for i := 1; i <= 15; i++ {
					newJob := cloudkitv1alpha1.JobStatus{
						JobID:     "job-" + string(rune('0'+i)),
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Duration(i) * time.Second)),
						State:     cloudkitv1alpha1.JobStatePending,
					}
					jobs = reconciler.appendJob(jobs, newJob)
				}
				// Should keep only last 10
				Expect(jobs).To(HaveLen(DefaultMaxJobHistory))
			})
		})

		Describe("updateJob", func() {
			It("should return false when job ID not found", func() {
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(time.Now().UTC()),
						State:     cloudkitv1alpha1.JobStatePending,
					},
				}
				updatedJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-999",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStateSucceeded,
				}
				result := updateJob(jobs, updatedJob)
				Expect(result).To(BeFalse())
				// Original job should be unchanged
				Expect(jobs[0].State).To(Equal(cloudkitv1alpha1.JobStatePending))
			})

			It("should return false when jobs slice is empty", func() {
				jobs := []cloudkitv1alpha1.JobStatus{}
				updatedJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(time.Now().UTC()),
					State:     cloudkitv1alpha1.JobStateSucceeded,
				}
				result := updateJob(jobs, updatedJob)
				Expect(result).To(BeFalse())
			})

			It("should update job and return true when found", func() {
				baseTime := time.Now().UTC()
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     cloudkitv1alpha1.JobStatePending,
						Message:   "Initial message",
					},
				}
				updatedTime := baseTime.Add(5 * time.Second)
				updatedJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-1",
					Type:      cloudkitv1alpha1.JobTypeProvision,
					Timestamp: metav1.NewTime(updatedTime),
					State:     cloudkitv1alpha1.JobStateSucceeded,
					Message:   "Updated message",
				}
				result := updateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				// Job should be fully updated
				Expect(jobs[0].JobID).To(Equal("job-1"))
				Expect(jobs[0].State).To(Equal(cloudkitv1alpha1.JobStateSucceeded))
				Expect(jobs[0].Message).To(Equal("Updated message"))
				Expect(jobs[0].Timestamp.Time).To(Equal(updatedTime))
			})

			It("should update correct job when multiple jobs exist", func() {
				baseTime := time.Now().UTC()
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:     "job-1",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime),
						State:     cloudkitv1alpha1.JobStatePending,
						Message:   "First job",
					},
					{
						JobID:     "job-2",
						Type:      cloudkitv1alpha1.JobTypeDeprovision,
						Timestamp: metav1.NewTime(baseTime.Add(time.Second)),
						State:     cloudkitv1alpha1.JobStateRunning,
						Message:   "Second job",
					},
					{
						JobID:     "job-3",
						Type:      cloudkitv1alpha1.JobTypeProvision,
						Timestamp: metav1.NewTime(baseTime.Add(2 * time.Second)),
						State:     cloudkitv1alpha1.JobStatePending,
						Message:   "Third job",
					},
				}
				updatedJob := cloudkitv1alpha1.JobStatus{
					JobID:     "job-2",
					Type:      cloudkitv1alpha1.JobTypeDeprovision,
					Timestamp: metav1.NewTime(baseTime.Add(3 * time.Second)),
					State:     cloudkitv1alpha1.JobStateSucceeded,
					Message:   "Second job completed",
				}
				result := updateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				// Only job-2 should be updated
				Expect(jobs[0].State).To(Equal(cloudkitv1alpha1.JobStatePending))
				Expect(jobs[0].Message).To(Equal("First job"))
				Expect(jobs[1].State).To(Equal(cloudkitv1alpha1.JobStateSucceeded))
				Expect(jobs[1].Message).To(Equal("Second job completed"))
				Expect(jobs[2].State).To(Equal(cloudkitv1alpha1.JobStatePending))
				Expect(jobs[2].Message).To(Equal("Third job"))
			})

			It("should update all fields of the job", func() {
				baseTime := time.Now().UTC()
				jobs := []cloudkitv1alpha1.JobStatus{
					{
						JobID:                  "job-1",
						Type:                   cloudkitv1alpha1.JobTypeProvision,
						Timestamp:              metav1.NewTime(baseTime),
						State:                  cloudkitv1alpha1.JobStatePending,
						Message:                "Initial",
						BlockDeletionOnFailure: false,
					},
				}
				updatedJob := cloudkitv1alpha1.JobStatus{
					JobID:                  "job-1",
					Type:                   cloudkitv1alpha1.JobTypeDeprovision, // Changed type
					Timestamp:              metav1.NewTime(baseTime.Add(time.Minute)),
					State:                  cloudkitv1alpha1.JobStateFailed,
					Message:                "Failed with error",
					BlockDeletionOnFailure: true,
				}
				result := updateJob(jobs, updatedJob)
				Expect(result).To(BeTrue())
				Expect(jobs[0].Type).To(Equal(cloudkitv1alpha1.JobTypeDeprovision))
				Expect(jobs[0].State).To(Equal(cloudkitv1alpha1.JobStateFailed))
				Expect(jobs[0].Message).To(Equal("Failed with error"))
				Expect(jobs[0].BlockDeletionOnFailure).To(BeTrue())
			})
		})
	})
})
