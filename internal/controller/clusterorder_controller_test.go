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

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

var _ = Describe("ClusterOrder Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		clusterorder := &v1alpha1.ClusterOrder{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ClusterOrder")
			err := k8sClient.Get(ctx, typeNamespacedName, clusterorder)
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha1.ClusterOrder{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: v1alpha1.ClusterOrderSpec{
						TemplateID: "test",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &v1alpha1.ClusterOrder{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ClusterOrder")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			noopWebhookClient := &noopWebhookClientForTest{}
			controllerReconciler := &ClusterOrderReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: provisioning.NewEDAProvider(noopWebhookClient, "http://noop-create", "http://noop-delete"),
				MaxJobHistory:        DefaultMaxJobHistory,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("shouldTriggerProvision", func() {
		var reconciler *ClusterOrderReconciler

		BeforeEach(func() {
			noopWebhookClient := &noopWebhookClientForTest{}
			reconciler = &ClusterOrderReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: provisioning.NewEDAProvider(noopWebhookClient, "http://noop-create", "http://noop-delete"),
			}
		})

		ctx := context.Background()

		It("should trigger when no job exists and config versions differ", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "abc123",
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionTrigger))
			Expect(job).To(BeNil())
		})

		It("should trigger when job has empty ID and config versions differ", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "abc123",
					Jobs:                 []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: ""}},
				},
			}
			action, _ := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionTrigger))
		})

		It("should skip when no job exists and config versions match", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion:    "abc123",
					ReconciledConfigVersion: "abc123",
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionSkip))
			Expect(job).To(BeNil())
		})

		It("should poll when job is still running", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					Jobs: []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: "job-1", State: v1alpha1.JobStateRunning}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionPoll))
			Expect(job).NotTo(BeNil())
			Expect(job.JobID).To(Equal("job-1"))
		})

		It("should poll when job is pending", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					Jobs: []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: "job-1", State: v1alpha1.JobStatePending}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionPoll))
			Expect(job).NotTo(BeNil())
		})

		It("should skip when job succeeded and config versions match", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion:    "abc123",
					ReconciledConfigVersion: "abc123",
					Jobs:                    []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: "job-1", State: v1alpha1.JobStateSucceeded}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionSkip))
			Expect(job).NotTo(BeNil())
		})

		It("should trigger when job succeeded but config versions differ", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion:    "new-version",
					ReconciledConfigVersion: "old-version",
					Jobs:                    []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: "job-1", State: v1alpha1.JobStateSucceeded}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionTrigger))
			Expect(job).NotTo(BeNil())
		})

		It("should trigger when job failed and config versions differ", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion:    "new-version",
					ReconciledConfigVersion: "old-version",
					Jobs:                    []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: "job-1", State: v1alpha1.JobStateFailed}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionTrigger))
			Expect(job).NotTo(BeNil())
		})

		It("should skip when job failed without ConfigVersion and reconciled matches desired", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion:    "abc123",
					ReconciledConfigVersion: "abc123",
					Jobs:                    []v1alpha1.JobStatus{{Type: v1alpha1.JobTypeProvision, JobID: "job-1", State: v1alpha1.JobStateFailed}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionSkip))
			Expect(job).NotTo(BeNil())
		})

		It("should skip when latest job succeeded with matching ConfigVersion", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "abc123",
					Jobs: []v1alpha1.JobStatus{{
						Type:          v1alpha1.JobTypeProvision,
						JobID:         "job-1",
						State:         v1alpha1.JobStateSucceeded,
						ConfigVersion: "abc123",
					}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionSkip))
			Expect(job).NotTo(BeNil())
		})

		It("should backoff when latest job failed with matching ConfigVersion", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "abc123",
					Jobs: []v1alpha1.JobStatus{{
						Type:          v1alpha1.JobTypeProvision,
						JobID:         "job-1",
						State:         v1alpha1.JobStateFailed,
						ConfigVersion: "abc123",
					}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionBackoff))
			Expect(job).NotTo(BeNil())
		})

		It("should trigger when latest job failed with different ConfigVersion", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "new-version",
					Jobs: []v1alpha1.JobStatus{{
						Type:          v1alpha1.JobTypeProvision,
						JobID:         "job-1",
						State:         v1alpha1.JobStateFailed,
						ConfigVersion: "old-version",
					}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionTrigger))
			Expect(job).NotTo(BeNil())
		})

		It("should trigger when latest job succeeded with different ConfigVersion", func() {
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "new-version",
					Jobs: []v1alpha1.JobStatus{{
						Type:          v1alpha1.JobTypeProvision,
						JobID:         "job-1",
						State:         v1alpha1.JobStateSucceeded,
						ConfigVersion: "old-version",
					}},
				},
			}
			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionTrigger))
			Expect(job).NotTo(BeNil())
		})

		It("should requeue when API server has non-terminal job but cache shows none", func() {
			instanceName := "test-co-api-server-check"
			apiInstance := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: "default",
				},
				Spec: v1alpha1.ClusterOrderSpec{
					TemplateID: "test",
				},
			}
			Expect(k8sClient.Create(ctx, apiInstance)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, apiInstance)
			})

			jobTimestamp := metav1.NewTime(time.Now().UTC())
			apiInstance.Status.DesiredConfigVersion = "v1"
			apiInstance.Status.Jobs = []v1alpha1.JobStatus{
				{Type: v1alpha1.JobTypeProvision, JobID: "running-job", State: v1alpha1.JobStateRunning, Timestamp: jobTimestamp},
			}
			Expect(k8sClient.Status().Update(ctx, apiInstance)).To(Succeed())

			// Simulate stale cache: in-memory instance has no jobs
			staleInstance := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: "default",
				},
				Status: v1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "v1",
				},
			}

			action, job := reconciler.shouldTriggerProvision(ctx, staleInstance)
			Expect(action).To(Equal(provisionRequeue))
			Expect(job).To(BeNil())
		})
	})

	Context("handleProvisioning", func() {
		var reconciler *ClusterOrderReconciler

		BeforeEach(func() {
			reconciler = &ClusterOrderReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{},
			}
		})

		ctx := context.Background()

		It("should skip provisioning when ManagementStateManual annotation is set", func() {
			instance := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateManual,
					},
				},
				Status: v1alpha1.ClusterOrderStatus{DesiredConfigVersion: "v1"},
			}

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).To(BeNil())
		})
	})

	Context("handleDeprovisioning", func() {
		var reconciler *ClusterOrderReconciler

		BeforeEach(func() {
			reconciler = &ClusterOrderReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{},
			}
		})

		ctx := context.Background()

		It("should skip deprovisioning when ManagementStateManual annotation is set", func() {
			instance := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						osacManagementStateAnnotation: ManagementStateManual,
					},
				},
			}

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestDeprovisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeDeprovision)
			Expect(latestDeprovisionJob).To(BeNil())
		})
	})

	Context("handleDesiredConfigVersion", func() {
		It("should produce consistent hash for same spec", func() {
			reconciler := &ClusterOrderReconciler{}
			instance := &v1alpha1.ClusterOrder{
				Spec: v1alpha1.ClusterOrderSpec{
					TemplateID: "test-template",
				},
			}
			err := reconciler.handleDesiredConfigVersion(instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(instance.Status.DesiredConfigVersion).NotTo(BeEmpty())

			firstHash := instance.Status.DesiredConfigVersion
			err = reconciler.handleDesiredConfigVersion(instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(instance.Status.DesiredConfigVersion).To(Equal(firstHash))
		})

		It("should produce different hash for different spec", func() {
			reconciler := &ClusterOrderReconciler{}
			instance1 := &v1alpha1.ClusterOrder{
				Spec: v1alpha1.ClusterOrderSpec{TemplateID: "template-a"},
			}
			instance2 := &v1alpha1.ClusterOrder{
				Spec: v1alpha1.ClusterOrderSpec{TemplateID: "template-b"},
			}
			Expect(reconciler.handleDesiredConfigVersion(instance1)).To(Succeed())
			Expect(reconciler.handleDesiredConfigVersion(instance2)).To(Succeed())
			Expect(instance1.Status.DesiredConfigVersion).NotTo(Equal(instance2.Status.DesiredConfigVersion))
		})
	})

	Context("handleReconciledConfigVersion", func() {
		ctx := context.Background()

		It("should copy version from annotation", func() {
			reconciler := &ClusterOrderReconciler{}
			instance := &v1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						osacReconciledConfigVersionAnnotation: "abc123",
					},
				},
			}
			reconciler.handleReconciledConfigVersion(ctx, instance)
			Expect(instance.Status.ReconciledConfigVersion).To(Equal("abc123"))
		})

		It("should clear version when annotation missing", func() {
			reconciler := &ClusterOrderReconciler{}
			instance := &v1alpha1.ClusterOrder{
				Status: v1alpha1.ClusterOrderStatus{
					ReconciledConfigVersion: "old-version",
				},
			}
			reconciler.handleReconciledConfigVersion(ctx, instance)
			Expect(instance.Status.ReconciledConfigVersion).To(BeEmpty())
		})
	})
})
