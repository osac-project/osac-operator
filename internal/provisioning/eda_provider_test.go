package provisioning_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/provisioning"
	"github.com/osac-project/osac-operator/internal/webhook"
)

// mockWebhookClient is a test double for WebhookClient
type mockWebhookClient struct {
	triggerWebhookFunc func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error)
}

func (m *mockWebhookClient) TriggerWebhook(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
	if m.triggerWebhookFunc != nil {
		return m.triggerWebhookFunc(ctx, url, resource)
	}
	return 0, nil
}

var _ = Describe("EDAProvider", func() {
	var (
		provider      *provisioning.EDAProvider
		webhookClient *mockWebhookClient
		ctx           context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		webhookClient = &mockWebhookClient{}
		// Test with ComputeInstance URLs, other resource types use empty strings
		provider = provisioning.NewEDAProvider(
			webhookClient,
			"http://create-url", "http://delete-url", // ComputeInstance
			"", "", // ClusterOrder
			"", "", // HostPool
		)
	})

	Describe("TriggerProvision", func() {
		Context("when webhook succeeds with no existing jobs", func() {
			It("should generate eda-webhook-1 as first job ID", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-instance",
						Namespace: "default",
					},
					Status: v1alpha1.ComputeInstanceStatus{
						Jobs: []v1alpha1.JobStatus{},
					},
				}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					Expect(url).To(Equal("http://create-url"))
					return 0, nil
				}

				result, err := provider.TriggerProvision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("eda-webhook-1"))
				Expect(result.InitialState).To(Equal(v1alpha1.JobStateRunning))
				Expect(result.Message).To(Equal("Webhook sent to EDA, provisioning in progress"))
			})
		})

		Context("when webhook succeeds with existing jobs", func() {
			It("should increment job ID counter", func() {
				baseTime := time.Now().UTC()
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-instance",
						Namespace: "default",
					},
					Status: v1alpha1.ComputeInstanceStatus{
						Jobs: []v1alpha1.JobStatus{
							{
								JobID:     "eda-webhook-1",
								Type:      v1alpha1.JobTypeProvision,
								Timestamp: metav1.NewTime(baseTime),
								State:     v1alpha1.JobStateSucceeded,
							},
							{
								JobID:     "eda-webhook-2",
								Type:      v1alpha1.JobTypeDeprovision,
								Timestamp: metav1.NewTime(baseTime.Add(time.Minute)),
								State:     v1alpha1.JobStateSucceeded,
							},
						},
					},
				}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, nil
				}

				result, err := provider.TriggerProvision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("eda-webhook-3"))
			})
		})

		Context("when webhook succeeds with non-sequential job IDs", func() {
			It("should use max counter + 1", func() {
				baseTime := time.Now().UTC()
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-instance",
						Namespace: "default",
					},
					Status: v1alpha1.ComputeInstanceStatus{
						Jobs: []v1alpha1.JobStatus{
							{
								JobID:     "eda-webhook-1",
								Type:      v1alpha1.JobTypeProvision,
								Timestamp: metav1.NewTime(baseTime),
								State:     v1alpha1.JobStateSucceeded,
							},
							{
								JobID:     "eda-webhook-5",
								Type:      v1alpha1.JobTypeDeprovision,
								Timestamp: metav1.NewTime(baseTime.Add(time.Minute)),
								State:     v1alpha1.JobStateSucceeded,
							},
							{
								JobID:     "eda-webhook-3",
								Type:      v1alpha1.JobTypeProvision,
								Timestamp: metav1.NewTime(baseTime.Add(2 * time.Minute)),
								State:     v1alpha1.JobStateFailed,
							},
						},
					},
				}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, nil
				}

				result, err := provider.TriggerProvision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("eda-webhook-6"))
			})
		})

		Context("when webhook succeeds with mixed job types", func() {
			It("should ignore non-EDA job IDs", func() {
				baseTime := time.Now().UTC()
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-instance",
						Namespace: "default",
					},
					Status: v1alpha1.ComputeInstanceStatus{
						Jobs: []v1alpha1.JobStatus{
							{
								JobID:     "aap-job-123",
								Type:      v1alpha1.JobTypeProvision,
								Timestamp: metav1.NewTime(baseTime),
								State:     v1alpha1.JobStateSucceeded,
							},
							{
								JobID:     "eda-webhook-2",
								Type:      v1alpha1.JobTypeProvision,
								Timestamp: metav1.NewTime(baseTime.Add(time.Minute)),
								State:     v1alpha1.JobStateSucceeded,
							},
						},
					},
				}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, nil
				}

				result, err := provider.TriggerProvision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("eda-webhook-3"))
			})
		})

		Context("when webhook fails", func() {
			BeforeEach(func() {
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, errors.New("webhook error")
				}
			})

			It("should return error", func() {
				instance := &v1alpha1.ComputeInstance{}
				_, err := provider.TriggerProvision(ctx, instance)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to trigger create webhook"))
			})
		})

		Context("when create URL is empty", func() {
			BeforeEach(func() {
				provider = provisioning.NewEDAProvider(webhookClient, "", "http://delete-url", "", "", "", "")
			})

			It("should return error", func() {
				instance := &v1alpha1.ComputeInstance{}
				_, err := provider.TriggerProvision(ctx, instance)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("create webhook URL not configured"))
			})
		})

		Context("when webhook is rate-limited", func() {
			BeforeEach(func() {
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "http://delete-url", "", "", "", "")
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 5 * time.Second, nil
				}
			})

			It("should return RateLimitError", func() {
				instance := &v1alpha1.ComputeInstance{}
				_, err := provider.TriggerProvision(ctx, instance)
				Expect(err).To(HaveOccurred())

				var rateLimitErr *provisioning.RateLimitError
				Expect(errors.As(err, &rateLimitErr)).To(BeTrue())
				Expect(rateLimitErr.RetryAfter).To(Equal(5 * time.Second))
			})
		})
	})

	Describe("GetProvisionStatus", func() {
		It("should always return unknown state", func() {
			instance := &v1alpha1.ComputeInstance{}
			status, err := provider.GetProvisionStatus(ctx, instance, "job-123")
			Expect(err).NotTo(HaveOccurred())
			Expect(status.JobID).To(Equal("job-123"))
			Expect(status.State).To(Equal(v1alpha1.JobStateUnknown))
			Expect(status.Message).To(Equal("EDA provider does not support status polling"))
		})
	})

	Describe("TriggerDeprovision", func() {
		Context("when webhook succeeds and AAP finalizer exists", func() {
			It("should generate unique job ID", func() {
				baseTime := time.Now().UTC()
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{"osac.openshift.io/computeinstance-aap"},
					},
					Status: v1alpha1.ComputeInstanceStatus{
						Jobs: []v1alpha1.JobStatus{
							{
								JobID:     "eda-webhook-1",
								Type:      v1alpha1.JobTypeProvision,
								Timestamp: metav1.NewTime(baseTime),
								State:     v1alpha1.JobStateSucceeded,
							},
						},
					},
				}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					Expect(url).To(Equal("http://delete-url"))
					return 0, nil
				}

				result, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Action).To(Equal(provisioning.DeprovisionTriggered))
				Expect(result.JobID).To(Equal("eda-webhook-2"))
				Expect(result.BlockDeletionOnFailure).To(BeFalse())
			})
		})

		Context("when AAP finalizer does not exist", func() {
			It("should skip deprovisioning", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{},
					},
				}
				result, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Action).To(Equal(provisioning.DeprovisionSkipped))
				Expect(result.JobID).To(BeEmpty())
				Expect(result.BlockDeletionOnFailure).To(BeFalse())
			})
		})

		Context("when webhook fails", func() {
			It("should return error", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{"osac.openshift.io/computeinstance-aap"},
					},
				}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, errors.New("webhook error")
				}

				_, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to trigger delete webhook"))
			})
		})

		Context("when delete URL is empty", func() {
			It("should return error", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{"osac.openshift.io/computeinstance-aap"},
					},
				}
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "", "", "", "", "")

				_, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("delete webhook URL not configured"))
			})
		})

		Context("when webhook is rate-limited", func() {
			It("should return RateLimitError", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{"osac.openshift.io/computeinstance-aap"},
					},
				}
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "http://delete-url", "", "", "", "")
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 3 * time.Second, nil
				}

				_, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).To(HaveOccurred())

				var rateLimitErr *provisioning.RateLimitError
				Expect(errors.As(err, &rateLimitErr)).To(BeTrue())
				Expect(rateLimitErr.RetryAfter).To(Equal(3 * time.Second))
			})
		})
	})

	Describe("GetDeprovisionStatus", func() {
		Context("when AAP finalizer is present", func() {
			It("should return running state", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{"osac.openshift.io/computeinstance-aap"},
					},
				}
				status, err := provider.GetDeprovisionStatus(ctx, instance, "job-456")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.JobID).To(Equal("job-456"))
				Expect(status.State).To(Equal(v1alpha1.JobStateRunning))
				Expect(status.Message).To(Equal("Waiting for AAP playbook to complete"))
			})
		})

		Context("when AAP finalizer has been removed", func() {
			It("should return succeeded state", func() {
				instance := &v1alpha1.ComputeInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-instance",
						Namespace:  "default",
						Finalizers: []string{},
					},
				}
				status, err := provider.GetDeprovisionStatus(ctx, instance, "job-456")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.JobID).To(Equal("job-456"))
				Expect(status.State).To(Equal(v1alpha1.JobStateSucceeded))
				Expect(status.Message).To(Equal("AAP playbook completed (finalizer removed)"))
			})
		})
	})
})
