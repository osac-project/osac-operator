package provisioning_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
	"github.com/innabox/cloudkit-operator/internal/provisioning"
	"github.com/innabox/cloudkit-operator/internal/webhook"
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

// mockResource is a test double for a Kubernetes resource that implements WebhookResource
type mockResource struct {
	metav1.TypeMeta
	metav1.ObjectMeta
}

func (m *mockResource) GetObjectKind() schema.ObjectKind {
	return &m.TypeMeta
}

func (m *mockResource) DeepCopyObject() runtime.Object {
	return &mockResource{
		TypeMeta:   m.TypeMeta,
		ObjectMeta: m.ObjectMeta,
	}
}

var _ = Describe("EDAProvider", func() {
	var (
		provider      *provisioning.EDAProvider
		webhookClient *mockWebhookClient
		ctx           context.Context
		resource      *mockResource
	)

	BeforeEach(func() {
		ctx = context.Background()
		webhookClient = &mockWebhookClient{}
		provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "http://delete-url")
		resource = &mockResource{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-resource",
			},
		}
	})

	Describe("TriggerProvision", func() {
		Context("when webhook succeeds", func() {
			BeforeEach(func() {
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					Expect(url).To(Equal("http://create-url"))
					return 0, nil
				}
			})

			It("should return eda-webhook as job ID", func() {
				result, err := provider.TriggerProvision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("eda-webhook"))
				Expect(result.InitialState).To(Equal(v1alpha1.JobStateRunning))
				Expect(result.Message).To(Equal("Webhook sent to EDA, provisioning in progress"))
			})
		})

		Context("when webhook fails", func() {
			BeforeEach(func() {
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, errors.New("webhook error")
				}
			})

			It("should return error", func() {
				_, err := provider.TriggerProvision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to trigger create webhook"))
			})
		})

		Context("when create URL is empty", func() {
			BeforeEach(func() {
				provider = provisioning.NewEDAProvider(webhookClient, "", "http://delete-url")
			})

			It("should return error", func() {
				_, err := provider.TriggerProvision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("create webhook URL not configured"))
			})
		})

		Context("when webhook is rate-limited", func() {
			BeforeEach(func() {
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "http://delete-url")
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 5 * time.Second, nil
				}
			})

			It("should return RateLimitError", func() {
				_, err := provider.TriggerProvision(ctx, resource)
				Expect(err).To(HaveOccurred())

				var rateLimitErr *provisioning.RateLimitError
				Expect(errors.As(err, &rateLimitErr)).To(BeTrue())
				Expect(rateLimitErr.RetryAfter).To(Equal(5 * time.Second))
			})
		})
	})

	Describe("GetProvisionStatus", func() {
		It("should always return unknown state", func() {
			status, err := provider.GetProvisionStatus(ctx, resource, "job-123")
			Expect(err).NotTo(HaveOccurred())
			Expect(status.JobID).To(Equal("job-123"))
			Expect(status.State).To(Equal(v1alpha1.JobStateUnknown))
			Expect(status.Message).To(Equal("EDA provider does not support status polling"))
		})
	})

	Describe("TriggerDeprovision", func() {
		Context("when webhook succeeds and AAP finalizer exists", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{"cloudkit.openshift.io/computeinstance-aap"}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					Expect(url).To(Equal("http://delete-url"))
					return 0, nil
				}
			})

			It("should return eda-webhook as job ID", func() {
				result, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Action).To(Equal(provisioning.DeprovisionTriggered))
				Expect(result.JobID).To(Equal("eda-webhook"))
				Expect(result.BlockDeletionOnFailure).To(BeFalse())
			})
		})

		Context("when AAP finalizer does not exist", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{}
			})

			It("should skip deprovisioning", func() {
				result, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Action).To(Equal(provisioning.DeprovisionSkipped))
				Expect(result.JobID).To(BeEmpty())
				Expect(result.BlockDeletionOnFailure).To(BeFalse())
			})
		})

		Context("when webhook fails", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{"cloudkit.openshift.io/computeinstance-aap"}
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 0, errors.New("webhook error")
				}
			})

			It("should return error", func() {
				_, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to trigger delete webhook"))
			})
		})

		Context("when delete URL is empty", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{"cloudkit.openshift.io/computeinstance-aap"}
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "")
			})

			It("should return error", func() {
				_, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("delete webhook URL not configured"))
			})
		})

		Context("when webhook is rate-limited", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{"cloudkit.openshift.io/computeinstance-aap"}
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "http://delete-url")
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource webhook.Resource) (time.Duration, error) {
					return 3 * time.Second, nil
				}
			})

			It("should return RateLimitError", func() {
				_, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).To(HaveOccurred())

				var rateLimitErr *provisioning.RateLimitError
				Expect(errors.As(err, &rateLimitErr)).To(BeTrue())
				Expect(rateLimitErr.RetryAfter).To(Equal(3 * time.Second))
			})
		})
	})

	Describe("GetDeprovisionStatus", func() {
		Context("when AAP finalizer is present", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{"cloudkit.openshift.io/computeinstance-aap"}
			})

			It("should return running state", func() {
				status, err := provider.GetDeprovisionStatus(ctx, resource, "job-456")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.JobID).To(Equal("job-456"))
				Expect(status.State).To(Equal(v1alpha1.JobStateRunning))
				Expect(status.Message).To(Equal("Waiting for AAP playbook to complete"))
			})
		})

		Context("when AAP finalizer has been removed", func() {
			BeforeEach(func() {
				resource.Finalizers = []string{}
			})

			It("should return succeeded state", func() {
				status, err := provider.GetDeprovisionStatus(ctx, resource, "job-456")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.JobID).To(Equal("job-456"))
				Expect(status.State).To(Equal(v1alpha1.JobStateSucceeded))
				Expect(status.Message).To(Equal("AAP playbook completed (finalizer removed)"))
			})
		})
	})
})
