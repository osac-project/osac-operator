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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

var _ = Describe("ClusterOrder Integration Tests", func() {
	const (
		clusterOrderTestNamespace = "default"
		statusPollInterval        = 100 * time.Millisecond
	)

	var (
		reconciler *ClusterOrderReconciler
		provider   *controllableProvider
	)

	BeforeEach(func() {
		provider = newControllableProvider()
		reconciler = NewClusterOrderReconciler(
			k8sClient, k8sClient, k8sClient.Scheme(),
			clusterOrderTestNamespace, provider,
			statusPollInterval, DefaultMaxJobHistory,
		)
	})

	ctx := context.Background()

	newTestClusterOrder := func(name string) *osacv1alpha1.ClusterOrder {
		return &osacv1alpha1.ClusterOrder{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: clusterOrderTestNamespace,
			},
			Spec: osacv1alpha1.ClusterOrderSpec{
				TemplateID: "test.template",
			},
		}
	}

	getClusterOrder := func(name string) *osacv1alpha1.ClusterOrder {
		instance := &osacv1alpha1.ClusterOrder{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{
			Name: name, Namespace: clusterOrderTestNamespace,
		}, instance)).To(Succeed())
		return instance
	}

	countProvisionJobs := func(instance *osacv1alpha1.ClusterOrder) int {
		count := 0
		for _, j := range instance.Status.Jobs {
			if j.Type == osacv1alpha1.JobTypeProvision {
				count++
			}
		}
		return count
	}

	Context("Provisioning workflow", func() {
		It("should provision through the full lifecycle: trigger, running, succeeded", func() {
			const name = "cluster-order-provision-success"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// First reconcile — trigger provision job
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			job := osacv1alpha1.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job).NotTo(BeNil())
			Expect(job.JobID).To(Equal("prov-job-" + name))
			Expect(job.State).To(Equal(osacv1alpha1.JobStatePending))
			Expect(job.ConfigVersion).To(Equal("v1"), "job should record the DesiredConfigVersion it was triggered for")

			// Simulate running
			provider.setProvisionJobState(osacv1alpha1.JobStateRunning, "Running")
			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			job = osacv1alpha1.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job.State).To(Equal(osacv1alpha1.JobStateRunning))

			// Simulate succeeded
			provider.setProvisionJobState(osacv1alpha1.JobStateSucceeded, "Completed")
			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should not requeue after terminal success")

			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())
			instance = getClusterOrder(name)
			job = osacv1alpha1.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job.State).To(Equal(osacv1alpha1.JobStateSucceeded))
		})

		It("should set Failed phase when provision job fails", func() {
			const name = "cluster-order-provision-failure"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Trigger and fail
			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setProvisionJobState(osacv1alpha1.JobStateFailed, "No agents available")
			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(instance.Status.Phase).To(Equal(osacv1alpha1.ClusterOrderPhaseFailed))
		})

		It("should not trigger duplicate jobs on rapid reconciles (stale cache guard)", func() {
			const name = "cluster-order-no-duplicate"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// First reconcile triggers and persists
			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Simulate stale cache: in-memory instance has no jobs
			staleInstance := &osacv1alpha1.ClusterOrder{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: clusterOrderTestNamespace,
				},
				Status: osacv1alpha1.ClusterOrderStatus{
					DesiredConfigVersion: "v1",
				},
			}

			action, _ := reconciler.shouldTriggerProvision(ctx, staleInstance)
			Expect(action).To(Equal(provisionRequeue), "should detect non-terminal job via API server and requeue")
		})
	})

	Context("Deprovisioning workflow", func() {
		It("should deprovision successfully", func() {
			const name = "cluster-order-deprovision-success"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			// Trigger deprovision
			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			job := osacv1alpha1.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeDeprovision)
			Expect(job).NotTo(BeNil())
			Expect(job.BlockDeletionOnFailure).To(BeTrue())

			// Simulate succeeded
			provider.setDeprovisionJobState(osacv1alpha1.JobStateSucceeded, "Completed")
			instance = getClusterOrder(name)
			result, err = reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should proceed with deletion")
		})

		It("should block deletion when deprovision fails with BlockDeletionOnFailure", func() {
			const name = "cluster-order-deprovision-blocked"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			_, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setDeprovisionJobState(osacv1alpha1.JobStateFailed, "Cleanup failed")
			instance = getClusterOrder(name)
			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval), "should requeue to block deletion")
		})
	})

	Context("Config version tracking", func() {
		It("should produce consistent hash for same spec", func() {
			instance := newTestClusterOrder("cluster-order-hash-idempotent")
			Expect(reconciler.handleDesiredConfigVersion(instance)).To(Succeed())
			firstHash := instance.Status.DesiredConfigVersion
			Expect(firstHash).NotTo(BeEmpty())

			Expect(reconciler.handleDesiredConfigVersion(instance)).To(Succeed())
			Expect(instance.Status.DesiredConfigVersion).To(Equal(firstHash))
		})

		It("should produce different hash when spec changes", func() {
			instance1 := newTestClusterOrder("cluster-order-hash-diff-a")
			instance1.Spec.TemplateID = "template.alpha"
			instance2 := newTestClusterOrder("cluster-order-hash-diff-b")
			instance2.Spec.TemplateID = "template.beta"

			Expect(reconciler.handleDesiredConfigVersion(instance1)).To(Succeed())
			Expect(reconciler.handleDesiredConfigVersion(instance2)).To(Succeed())
			Expect(instance1.Status.DesiredConfigVersion).NotTo(Equal(instance2.Status.DesiredConfigVersion))
		})

		It("should copy reconciled config version from annotation", func() {
			instance := newTestClusterOrder("cluster-order-reconciled-copy")
			instance.Annotations = map[string]string{
				osacReconciledConfigVersionAnnotation: "abc123",
			}
			reconciler.handleReconciledConfigVersion(ctx, instance)
			Expect(instance.Status.ReconciledConfigVersion).To(Equal("abc123"))
		})

		It("should clear reconciled config version when annotation is missing", func() {
			instance := newTestClusterOrder("cluster-order-reconciled-clear")
			instance.Status.ReconciledConfigVersion = "old-version"
			reconciler.handleReconciledConfigVersion(ctx, instance)
			Expect(instance.Status.ReconciledConfigVersion).To(BeEmpty())
		})

		It("should skip provisioning when config versions match", func() {
			instance := newTestClusterOrder("cluster-order-skip-match")
			instance.Status.DesiredConfigVersion = "v1"
			instance.Status.ReconciledConfigVersion = "v1"

			action, _ := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionSkip))
		})

		It("should skip provisioning after config versions match even with terminal job", func() {
			instance := newTestClusterOrder("cluster-order-skip-terminal")
			instance.Status.DesiredConfigVersion = "v1"
			instance.Status.ReconciledConfigVersion = "v1"
			instance.Status.Jobs = []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateSucceeded},
			}

			action, job := reconciler.shouldTriggerProvision(ctx, instance)
			Expect(action).To(Equal(provisionSkip))
			Expect(job).NotTo(BeNil())
		})
	})

	Context("Infinite retry prevention", func() {
		// Known bug: when a provision job fails and the AAP playbook never sets the
		It("should not create additional provision jobs when previous job failed for same config", func() {
			const name = "cluster-order-no-infinite-retry"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Trigger and fail
			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setProvisionJobState(osacv1alpha1.JobStateFailed, "Failed")
			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Next reconcile should NOT trigger a new job
			instance = getClusterOrder(name)
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(countProvisionJobs(instance)).To(Equal(1), "should not create additional jobs on repeated failure")
		})
	})
})
