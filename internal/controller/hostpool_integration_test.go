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
	"github.com/osac-project/osac-operator/internal/provisioning"
)

var _ = Describe("HostPool Integration Tests", func() {
	const (
		hostPoolTestNamespace = "default"
		statusPollInterval    = 100 * time.Millisecond
	)

	var (
		reconciler *HostPoolReconciler
		provider   *controllableProvider
	)

	BeforeEach(func() {
		provider = newControllableProvider()
		reconciler = NewHostPoolReconciler(
			k8sClient, k8sClient, k8sClient.Scheme(),
			hostPoolTestNamespace, provider,
			statusPollInterval, DefaultMaxJobHistory,
		)
	})

	ctx := context.Background()

	newTestHostPool := func(name string) *osacv1alpha1.HostPool {
		return &osacv1alpha1.HostPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: hostPoolTestNamespace,
			},
			Spec: osacv1alpha1.HostPoolSpec{
				HostSets: []osacv1alpha1.HostSet{
					{HostClass: "default", Size: 1},
				},
			},
		}
	}

	getHostPool := func(name string) *osacv1alpha1.HostPool {
		instance := &osacv1alpha1.HostPool{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{
			Name: name, Namespace: hostPoolTestNamespace,
		}, instance)).To(Succeed())
		return instance
	}

	countProvisionJobs := func(instance *osacv1alpha1.HostPool) int {
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
			const name = "hostpool-provision-success"
			instance := newTestHostPool(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Trigger
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			instance = getHostPool(name)
			job := provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job).NotTo(BeNil())
			Expect(job.State).To(Equal(osacv1alpha1.JobStatePending))
			Expect(job.ConfigVersion).To(Equal("v1"), "job should record the DesiredConfigVersion it was triggered for")

			// Running
			provider.setProvisionJobState(osacv1alpha1.JobStateRunning, "Running")
			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Succeeded
			provider.setProvisionJobState(osacv1alpha1.JobStateSucceeded, "Completed")
			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())
			instance = getHostPool(name)
			job = provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job.State).To(Equal(osacv1alpha1.JobStateSucceeded))
		})

		It("should set Failed phase when provision job fails", func() {
			const name = "hostpool-provision-failure"
			instance := newTestHostPool(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setProvisionJobState(osacv1alpha1.JobStateFailed, "No agents")
			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(instance.Status.Phase).To(Equal(osacv1alpha1.HostPoolPhaseFailed))
		})

		It("should not trigger duplicate jobs on rapid reconciles (stale cache guard)", func() {
			const name = "hostpool-no-duplicate"
			instance := newTestHostPool(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			staleInstance := &osacv1alpha1.HostPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: hostPoolTestNamespace,
				},
				Status: osacv1alpha1.HostPoolStatus{
					DesiredConfigVersion: "v1",
				},
			}

			action, _ := reconciler.shouldTriggerProvision(ctx, staleInstance)
			Expect(action).To(Equal(provisioning.Requeue))
		})
	})

	Context("Deprovisioning workflow", func() {
		It("should deprovision successfully", func() {
			const name = "hostpool-deprovision-success"
			instance := newTestHostPool(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setDeprovisionJobState(osacv1alpha1.JobStateSucceeded, "Completed")
			instance = getHostPool(name)
			result, err = reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should block deletion when deprovision fails with BlockDeletionOnFailure", func() {
			const name = "hostpool-deprovision-blocked"
			instance := newTestHostPool(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			_, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setDeprovisionJobState(osacv1alpha1.JobStateFailed, "Cleanup failed")
			instance = getHostPool(name)
			result, err := reconciler.handleDeprovisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval), "should requeue to block deletion")
		})
	})

	Context("Infinite retry prevention", func() {
		// Known bug: when a provision job fails and the AAP playbook never sets the
		It("should not create additional provision jobs when previous job failed for same config", func() {
			const name = "hostpool-no-infinite-retry"
			instance := newTestHostPool(name)
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

			// Next reconcile should back off, not trigger a new job immediately
			instance = getHostPool(name)
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "should requeue with backoff delay")
			Expect(result.RequeueAfter).To(BeNumerically("<=", provisioning.BackoffMaxDelay))
			Expect(countProvisionJobs(instance)).To(Equal(1), "should not create additional jobs during backoff")
		})
	})
})
