package provisioning_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/innabox/cloudkit-operator/internal/provisioning"
)

// mockWebhookClient is a test double for WebhookClient
type mockWebhookClient struct {
	triggerWebhookFunc func(ctx context.Context, url string, resource provisioning.WebhookResource) (interface{}, error)
}

func (m *mockWebhookClient) TriggerWebhook(ctx context.Context, url string, resource provisioning.WebhookResource) (interface{}, error) {
	if m.triggerWebhookFunc != nil {
		return m.triggerWebhookFunc(ctx, url, resource)
	}
	return nil, nil
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
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource provisioning.WebhookResource) (interface{}, error) {
					Expect(url).To(Equal("http://create-url"))
					return nil, nil
				}
			})

			It("should return resource name as job ID", func() {
				jobID, err := provider.TriggerProvision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(jobID).To(Equal("test-resource"))
			})
		})

		Context("when webhook fails", func() {
			BeforeEach(func() {
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource provisioning.WebhookResource) (interface{}, error) {
					return nil, errors.New("webhook error")
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
	})

	Describe("GetProvisionStatus", func() {
		It("should always return running state", func() {
			status, err := provider.GetProvisionStatus(ctx, "job-123")
			Expect(err).NotTo(HaveOccurred())
			Expect(status.JobID).To(Equal("job-123"))
			Expect(status.State).To(Equal(provisioning.JobStateRunning))
		})
	})

	Describe("TriggerDeprovision", func() {
		Context("when webhook succeeds", func() {
			BeforeEach(func() {
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource provisioning.WebhookResource) (interface{}, error) {
					Expect(url).To(Equal("http://delete-url"))
					return nil, nil
				}
			})

			It("should return resource name as job ID", func() {
				jobID, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(jobID).To(Equal("test-resource"))
			})
		})

		Context("when webhook fails", func() {
			BeforeEach(func() {
				webhookClient.triggerWebhookFunc = func(ctx context.Context, url string, resource provisioning.WebhookResource) (interface{}, error) {
					return nil, errors.New("webhook error")
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
				provider = provisioning.NewEDAProvider(webhookClient, "http://create-url", "")
			})

			It("should return error", func() {
				_, err := provider.TriggerDeprovision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("delete webhook URL not configured"))
			})
		})
	})

	Describe("GetDeprovisionStatus", func() {
		It("should always return running state", func() {
			status, err := provider.GetDeprovisionStatus(ctx, "job-456")
			Expect(err).NotTo(HaveOccurred())
			Expect(status.JobID).To(Equal("job-456"))
			Expect(status.State).To(Equal(provisioning.JobStateRunning))
		})
	})
})
