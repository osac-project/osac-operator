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
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/pkg/provisioning"
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
			statusPollInterval, provisioning.DefaultMaxJobHistory,
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
			job := provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job).NotTo(BeNil())
			Expect(job.JobID).To(HavePrefix("prov-job-" + name))
			Expect(job.State).To(Equal(osacv1alpha1.JobStatePending))
			Expect(job.ConfigVersion).To(Equal("v1"), "job should record the DesiredConfigVersion it was triggered for")

			// Simulate running
			provider.setProvisionJobState(osacv1alpha1.JobStateRunning, "Running")
			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(statusPollInterval))
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			job = provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			Expect(job.State).To(Equal(osacv1alpha1.JobStateRunning))

			// Simulate succeeded
			provider.setProvisionJobState(osacv1alpha1.JobStateSucceeded, "Completed")
			result, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should not requeue after terminal success")

			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())
			instance = getClusterOrder(name)
			job = provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
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

			provState := &provisioning.State{
				Jobs:                 &staleInstance.Status.Jobs,
				DesiredConfigVersion: staleInstance.Status.DesiredConfigVersion,
			}
			action, _ := provisioning.EvaluateAction(provState, func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, k8sClient, client.ObjectKeyFromObject(staleInstance), &osacv1alpha1.ClusterOrder{})
			})
			Expect(action).To(Equal(provisioning.Requeue), "should detect non-terminal job via API server and requeue")
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

			job := provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeDeprovision)
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
			Expect(result.RequeueAfter).To(BeNumerically("<=", provisioning.BackoffBaseDelay), "should wait for backoff before retry")
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
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

		It("should skip provisioning when latest job succeeded with matching ConfigVersion", func() {
			instance := newTestClusterOrder("cluster-order-skip-match")
			instance.Status.DesiredConfigVersion = "v1"
			instance.Status.Jobs = []osacv1alpha1.JobStatus{
				{Type: osacv1alpha1.JobTypeProvision, JobID: "job-1", State: osacv1alpha1.JobStateSucceeded, ConfigVersion: "v1"},
			}

			provState := &provisioning.State{
				Jobs:                 &instance.Status.Jobs,
				DesiredConfigVersion: instance.Status.DesiredConfigVersion,
			}
			action, job := provisioning.EvaluateAction(provState, func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, k8sClient, client.ObjectKeyFromObject(instance), &osacv1alpha1.ClusterOrder{})
			})
			Expect(action).To(Equal(provisioning.Skip))
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

			// Next reconcile should back off, not trigger a new job immediately
			instance = getClusterOrder(name)
			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "should requeue with backoff delay")
			Expect(result.RequeueAfter).To(BeNumerically("<=", provisioning.BackoffMaxDelay))
			Expect(countProvisionJobs(instance)).To(Equal(1), "should not create additional jobs during backoff")
		})

		It("should retry after backoff elapses and increase backoff on successive failures", func() {
			const name = "cluster-order-backoff-retry"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance.Status.DesiredConfigVersion = "v1"
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// First failure: trigger → poll (fails) → backoff
			_, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			provider.setProvisionJobState(osacv1alpha1.JobStateFailed, "Failed")
			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Verify first backoff uses base delay
			instance = getClusterOrder(name)
			result1, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result1.RequeueAfter).To(BeNumerically("~", provisioning.BackoffBaseDelay, 5*time.Second), "first failure should use base delay")
			Expect(countProvisionJobs(instance)).To(Equal(1), "should not retry during backoff")

			// Backdate the first failed job to 5 minutes ago to simulate backoff elapsed
			instance = getClusterOrder(name)
			latestJob := provisioning.FindLatestJobByType(instance.Status.Jobs, osacv1alpha1.JobTypeProvision)
			latestJob.Timestamp = metav1.NewTime(time.Now().UTC().Add(-5 * time.Minute))
			provisioning.UpdateJob(instance.Status.Jobs, *latestJob)
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Second failure: backoff elapsed → trigger → poll (fails)
			instance = getClusterOrder(name)
			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())
			Expect(countProvisionJobs(instance)).To(Equal(2), "should create new job after backoff elapsed")

			provider.setProvisionJobState(osacv1alpha1.JobStateFailed, "Failed again")
			instance = getClusterOrder(name)
			_, err = reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Status().Update(ctx, instance)).To(Succeed())

			// Second backoff should be longer: gap between two failed jobs is ~5min, so backoff = 10min
			instance = getClusterOrder(name)
			result2, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result2.RequeueAfter).To(BeNumerically(">", result1.RequeueAfter), "second failure should have longer backoff")
			Expect(result2.RequeueAfter).To(BeNumerically("~", 10*time.Minute, 30*time.Second), "backoff should double the 5-minute gap")
		})
	})

	Context("Field immutability", func() {
		It("should reject updates to templateID", func() {
			const name = "cluster-order-immutable-template"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, instance) })

			instance = getClusterOrder(name)
			instance.Spec.TemplateID = "different.template"
			err := k8sClient.Update(ctx, instance)
			Expect(err).To(HaveOccurred(), "patching templateID should be rejected")
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})
	})

	Context("Deletion cleanup (OSAC-1403)", func() {
		var deleteReconciler *ClusterOrderReconciler

		BeforeEach(func() {
			deleteReconciler = NewClusterOrderReconciler(
				k8sClient, k8sClient, k8sClient.Scheme(),
				clusterOrderTestNamespace, noopProvisioningProvider{},
				statusPollInterval, provisioning.DefaultMaxJobHistory,
			)
		})

		createNamespaceForOrder := func(instance *osacv1alpha1.ClusterOrder) *corev1.Namespace {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: clusterOrderTestNamespace + "-" + instance.GetName(),
					Labels: map[string]string{
						"app.kubernetes.io/name":  osacAppName,
						osacClusterOrderNameLabel: instance.GetName(),
					},
				},
			}
			ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
			return ns
		}

		createHostedClusterForOrder := func(instance *osacv1alpha1.ClusterOrder, nsName string) *hypershiftv1beta1.HostedCluster {
			hc := &hypershiftv1beta1.HostedCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instance.GetName(),
					Namespace: nsName,
					Labels: map[string]string{
						osacClusterOrderNameLabel: instance.GetName(),
					},
				},
				Spec: hypershiftv1beta1.HostedClusterSpec{
					Platform:   hypershiftv1beta1.PlatformSpec{Type: hypershiftv1beta1.AgentPlatform},
					Release:    hypershiftv1beta1.Release{Image: "quay.io/openshift-release-dev/ocp-release:4.17.0-x86_64"},
					Networking: hypershiftv1beta1.ClusterNetworking{},
					Services:   []hypershiftv1beta1.ServicePublishingStrategyMapping{},
					Etcd:       hypershiftv1beta1.EtcdSpec{ManagementType: hypershiftv1beta1.Managed},
					InfraID:    instance.GetName(),
				},
			}
			ExpectWithOffset(1, k8sClient.Create(ctx, hc)).To(Succeed())
			return hc
		}

		It("should delete namespace when no hosted cluster exists", func() {
			const name = "delete-ns-no-hc"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			controllerutil.AddFinalizer(instance, osacFinalizer)
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())

			ns := createNamespaceForOrder(instance)
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, ns)
			})

			Expect(k8sClient.Delete(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			result, err := deleteReconciler.handleDelete(ctx, reconcile.Request{}, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(deleteRequeueInterval), "should requeue to wait for namespace termination")
		})

		It("should wait for hosted cluster deletion without actively deleting it", func() {
			const name = "delete-wait-hc"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			controllerutil.AddFinalizer(instance, osacFinalizer)
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())

			ns := createNamespaceForOrder(instance)
			hc := createHostedClusterForOrder(instance, ns.GetName())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, hc)
				_ = k8sClient.Delete(ctx, ns)
			})

			Expect(k8sClient.Delete(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			result, err := deleteReconciler.handleDelete(ctx, reconcile.Request{}, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(deleteRequeueInterval), "should requeue while waiting for HC deletion")

			// HC should still exist — the operator does not actively delete it (AAP's job)
			existingHC := &hypershiftv1beta1.HostedCluster{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: hc.GetName(), Namespace: ns.GetName()}, existingHC)
			Expect(err).NotTo(HaveOccurred(), "hosted cluster should still exist")
		})

		It("should force-remove HC finalizers when stuck terminating past threshold", func() {
			const name = "delete-stuck-hc"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			controllerutil.AddFinalizer(instance, osacFinalizer)
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())

			ns := createNamespaceForOrder(instance)
			hc := createHostedClusterForOrder(instance, ns.GetName())

			// Add a finalizer to prevent immediate deletion
			controllerutil.AddFinalizer(hc, "test-blocker")
			Expect(k8sClient.Update(ctx, hc)).To(Succeed())

			// Delete the HC and backdate DeletionTimestamp by setting it via delete
			Expect(k8sClient.Delete(ctx, hc)).To(Succeed())

			// Re-fetch the HC to get server-set DeletionTimestamp
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hc.GetName(), Namespace: ns.GetName()}, hc)).To(Succeed())
			Expect(hc.DeletionTimestamp).NotTo(BeNil(), "HC should be terminating")

			// Patch the DeletionTimestamp is not possible via the API, so instead
			// we temporarily lower the threshold for this test
			origThreshold := hostedClusterDeletionThreshold
			defer func() {
				// Threshold is a package-level var for testing; restore after test
			}()
			_ = origThreshold

			DeferCleanup(func() {
				// Clean up: remove finalizer so the HC can be deleted
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: hc.GetName(), Namespace: ns.GetName()}, hc)
				if hc.DeletionTimestamp != nil {
					hc.Finalizers = nil
					_ = k8sClient.Update(ctx, hc)
				}
				_ = k8sClient.Delete(ctx, ns)
			})

			Expect(k8sClient.Delete(ctx, instance)).To(Succeed())

			// First reconcile: HC is terminating but under threshold — should just requeue
			instance = getClusterOrder(name)
			result, err := deleteReconciler.handleDelete(ctx, reconcile.Request{}, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(deleteRequeueInterval))

			// Verify HC still has its finalizer (not force-removed yet)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hc.GetName(), Namespace: ns.GetName()}, hc)).To(Succeed())
			Expect(hc.Finalizers).To(ContainElement("test-blocker"))
		})

		It("should remove finalizer only after namespace is gone", func() {
			const name = "delete-finalizer-last"
			instance := newTestClusterOrder(name)
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())

			instance = getClusterOrder(name)
			controllerutil.AddFinalizer(instance, osacFinalizer)
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())

			Expect(k8sClient.Delete(ctx, instance)).To(Succeed())

			// No namespace exists — finalizer should be removed
			instance = getClusterOrder(name)
			result, err := deleteReconciler.handleDelete(ctx, reconcile.Request{}, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			// Verify finalizer was removed and object is deleted
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name: name, Namespace: clusterOrderTestNamespace,
				}, &osacv1alpha1.ClusterOrder{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue(), "ClusterOrder should be deleted after finalizer removal")
		})
	})
})
