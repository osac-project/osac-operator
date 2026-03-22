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

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
)

var _ = Describe("HostPool Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		hostpool := &v1alpha1.HostPool{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind HostPool")
			err := k8sClient.Get(ctx, typeNamespacedName, hostpool)
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha1.HostPool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: v1alpha1.HostPoolSpec{
						HostSets: []v1alpha1.HostSet{
							{
								HostClass: "fc430",
								Size:      3,
							},
							{
								HostClass: "fc830",
								Size:      1,
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &v1alpha1.HostPool{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance HostPool")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			noopWebhookClient := &noopWebhookClientForTest{}
			controllerReconciler := &HostPoolReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: provisioning.NewEDAProvider(noopWebhookClient, "http://noop-create", "http://noop-delete"),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	Context("handleProvisioning", func() {
		var reconciler *HostPoolReconciler

		BeforeEach(func() {
			reconciler = &HostPoolReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{},
			}
		})

		ctx := context.Background()

		It("should skip provisioning when ManagementStateManual annotation is set", func() {
			instance := &v1alpha1.HostPool{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						osacHostPoolManagementStateAnnotation: ManagementStateManual,
					},
				},
				Status: v1alpha1.HostPoolStatus{DesiredConfigVersion: "v1"},
			}

			result, err := reconciler.handleProvisioning(ctx, instance)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			latestProvisionJob := v1alpha1.FindLatestJobByType(instance.Status.Jobs, v1alpha1.JobTypeProvision)
			Expect(latestProvisionJob).To(BeNil())
		})
	})

	Context("handleDeprovisioning", func() {
		var reconciler *HostPoolReconciler

		BeforeEach(func() {
			reconciler = &HostPoolReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{},
			}
		})

		ctx := context.Background()

		It("should skip deprovisioning when ManagementStateManual annotation is set", func() {
			instance := &v1alpha1.HostPool{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						osacHostPoolManagementStateAnnotation: ManagementStateManual,
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

	Context("shouldTriggerProvision", func() {
		var reconciler *HostPoolReconciler

		BeforeEach(func() {
			reconciler = &HostPoolReconciler{
				Client:               k8sClient,
				apiReader:            k8sClient,
				Scheme:               k8sClient.Scheme(),
				ProvisioningProvider: &mockProvisioningProvider{},
			}
		})

		ctx := context.Background()

		It("should backoff when latest job failed with matching ConfigVersion", func() {
			instance := &v1alpha1.HostPool{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Status: v1alpha1.HostPoolStatus{
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
	})
})
