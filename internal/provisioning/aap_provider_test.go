package provisioning_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
	"github.com/innabox/cloudkit-operator/internal/aap"
	"github.com/innabox/cloudkit-operator/internal/provisioning"
)

// mockAAPClient is a test double for aap.Client
type mockAAPClient struct {
	getTemplateFunc            func(ctx context.Context, templateName string) (*aap.Template, error)
	launchJobTemplateFunc      func(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error)
	launchWorkflowTemplateFunc func(ctx context.Context, req aap.LaunchWorkflowTemplateRequest) (*aap.LaunchWorkflowTemplateResponse, error)
	getJobFunc                 func(ctx context.Context, jobID string) (*aap.Job, error)
	cancelJobFunc              func(ctx context.Context, jobID string) error
}

func (m *mockAAPClient) GetTemplate(ctx context.Context, templateName string) (*aap.Template, error) {
	if m.getTemplateFunc != nil {
		return m.getTemplateFunc(ctx, templateName)
	}
	return &aap.Template{ID: 1, Name: templateName, Type: aap.TemplateTypeJob}, nil
}

func (m *mockAAPClient) LaunchJobTemplate(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error) {
	if m.launchJobTemplateFunc != nil {
		return m.launchJobTemplateFunc(ctx, req)
	}
	return &aap.LaunchJobTemplateResponse{JobID: 123}, nil
}

func (m *mockAAPClient) LaunchWorkflowTemplate(ctx context.Context, req aap.LaunchWorkflowTemplateRequest) (*aap.LaunchWorkflowTemplateResponse, error) {
	if m.launchWorkflowTemplateFunc != nil {
		return m.launchWorkflowTemplateFunc(ctx, req)
	}
	return &aap.LaunchWorkflowTemplateResponse{JobID: 456}, nil
}

func (m *mockAAPClient) GetJob(ctx context.Context, jobID string) (*aap.Job, error) {
	if m.getJobFunc != nil {
		return m.getJobFunc(ctx, jobID)
	}
	// Convert jobID string to int for the ID field
	var id int
	if _, err := fmt.Sscanf(jobID, "%d", &id); err != nil {
		id = 123 // default
	}
	return &aap.Job{
		ID:       id,
		Status:   "successful",
		Started:  time.Now().UTC(),
		Finished: time.Now().UTC().Add(time.Minute),
	}, nil
}

func (m *mockAAPClient) CancelJob(ctx context.Context, jobID string) error {
	if m.cancelJobFunc != nil {
		return m.cancelJobFunc(ctx, jobID)
	}
	return nil
}

// extractEDAPayload extracts the payload from EDA event structure in extra_vars.
// This helper function is used to verify the EDA compatibility wrapper.
func extractEDAPayload(extraVars map[string]any) map[string]any {
	edaEvent := extraVars["ansible_eda"].(map[string]any)
	return edaEvent["event"].(map[string]any)["payload"].(map[string]any)
}

var _ = Describe("AAPProvider", func() {
	var (
		provider  *provisioning.AAPProvider
		aapClient *mockAAPClient
		ctx       context.Context
		resource  *mockResource
	)

	BeforeEach(func() {
		ctx = context.Background()
		aapClient = &mockAAPClient{}
		resource = &mockResource{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource",
				Namespace: "default",
			},
		}
	})

	Describe("TriggerProvision", func() {
		Context("with job template", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "provision-job", "deprovision-job")
				aapClient.getTemplateFunc = func(ctx context.Context, templateName string) (*aap.Template, error) {
					return &aap.Template{ID: 1, Name: templateName, Type: aap.TemplateTypeJob}, nil
				}
				aapClient.launchJobTemplateFunc = func(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error) {
					Expect(req.TemplateName).To(Equal("provision-job"))
					// Verify EDA event structure for compatibility with EDA-designed templates
					Expect(req.ExtraVars).To(HaveKey("ansible_eda"))
					payload := extractEDAPayload(req.ExtraVars)
					// Verify serialized resource contains the ObjectMeta fields
					// Note: mockResource embeds ObjectMeta, so fields are at top level
					Expect(payload).To(HaveKeyWithValue("name", "test-resource"))
					Expect(payload).To(HaveKeyWithValue("namespace", "default"))
					return &aap.LaunchJobTemplateResponse{JobID: 123}, nil
				}
			})

			It("should launch job template and return job ID", func() {
				result, err := provider.TriggerProvision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("123"))
				Expect(result.InitialState).To(Equal(provisioning.JobStatePending))
				Expect(result.Message).To(Equal("Provisioning job triggered"))
			})
		})

		Context("with workflow template", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "provision-workflow", "deprovision-workflow")
				aapClient.getTemplateFunc = func(ctx context.Context, templateName string) (*aap.Template, error) {
					return &aap.Template{ID: 2, Name: templateName, Type: aap.TemplateTypeWorkflow}, nil
				}
				aapClient.launchWorkflowTemplateFunc = func(ctx context.Context, req aap.LaunchWorkflowTemplateRequest) (*aap.LaunchWorkflowTemplateResponse, error) {
					Expect(req.TemplateName).To(Equal("provision-workflow"))
					// Verify EDA event structure for compatibility with EDA-designed templates
					Expect(req.ExtraVars).To(HaveKey("ansible_eda"))
					payload := extractEDAPayload(req.ExtraVars)
					// Verify serialized resource contains the ObjectMeta fields
					// Note: mockResource embeds ObjectMeta, so fields are at top level
					Expect(payload).To(HaveKeyWithValue("namespace", "default"))
					return &aap.LaunchWorkflowTemplateResponse{JobID: 456}, nil
				}
			})

			It("should launch workflow template and return job ID", func() {
				result, err := provider.TriggerProvision(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.JobID).To(Equal("456"))
				Expect(result.InitialState).To(Equal(provisioning.JobStatePending))
				Expect(result.Message).To(Equal("Provisioning job triggered"))
			})
		})

		Context("when template not configured", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "", "deprovision-job")
			})

			It("should return error", func() {
				_, err := provider.TriggerProvision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("provision template not configured"))
			})
		})

		Context("when template detection fails", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "provision-job", "deprovision-job")
				aapClient.getTemplateFunc = func(ctx context.Context, templateName string) (*aap.Template, error) {
					return nil, errors.New("template not found")
				}
			})

			It("should return error", func() {
				_, err := provider.TriggerProvision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to get template"))
			})
		})

		Context("when job launch fails", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "provision-job", "deprovision-job")
				aapClient.getTemplateFunc = func(ctx context.Context, templateName string) (*aap.Template, error) {
					return &aap.Template{ID: 1, Name: templateName, Type: aap.TemplateTypeJob}, nil
				}
				aapClient.launchJobTemplateFunc = func(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error) {
					return nil, errors.New("AAP API error")
				}
			})

			It("should return error", func() {
				_, err := provider.TriggerProvision(ctx, resource)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to launch job template"))
			})
		})
	})

	Describe("GetProvisionStatus", func() {
		BeforeEach(func() {
			provider = provisioning.NewAAPProvider(aapClient, "provision-job", "deprovision-job")
		})

		Context("when job is successful", func() {
			BeforeEach(func() {
				aapClient.getJobFunc = func(ctx context.Context, jobID string) (*aap.Job, error) {
					var id int
					if _, err := fmt.Sscanf(jobID, "%d", &id); err != nil {
						id = 789 // default
					}
					return &aap.Job{
						ID:       id,
						Status:   "successful",
						Started:  time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
						Finished: time.Date(2024, 1, 1, 12, 5, 0, 0, time.UTC),
					}, nil
				}
			})

			It("should return succeeded state", func() {
				status, err := provider.GetProvisionStatus(ctx, "789")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.JobID).To(Equal("789"))
				Expect(status.State).To(Equal(provisioning.JobStateSucceeded))
				Expect(status.Message).To(Equal("successful"))
			})
		})

		Context("when job is running", func() {
			BeforeEach(func() {
				aapClient.getJobFunc = func(ctx context.Context, jobID string) (*aap.Job, error) {
					var id int
					if _, err := fmt.Sscanf(jobID, "%d", &id); err != nil {
						id = 789 // default
					}
					return &aap.Job{
						ID:      id,
						Status:  "running",
						Started: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
					}, nil
				}
			})

			It("should return running state", func() {
				status, err := provider.GetProvisionStatus(ctx, "789")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(provisioning.JobStateRunning))
			})
		})

		Context("when job failed with traceback", func() {
			BeforeEach(func() {
				aapClient.getJobFunc = func(ctx context.Context, jobID string) (*aap.Job, error) {
					var id int
					if _, err := fmt.Sscanf(jobID, "%d", &id); err != nil {
						id = 789 // default
					}
					return &aap.Job{
						ID:              id,
						Status:          "failed",
						Started:         time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
						Finished:        time.Date(2024, 1, 1, 12, 1, 0, 0, time.UTC),
						ResultTraceback: "Error: Connection timeout",
					}, nil
				}
			})

			It("should return failed state with error details", func() {
				status, err := provider.GetProvisionStatus(ctx, "789")
				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(provisioning.JobStateFailed))
				Expect(status.ErrorDetails).To(Equal("Error: Connection timeout"))
			})
		})

		Context("when job ID is invalid", func() {
			BeforeEach(func() {
				aapClient.getJobFunc = func(ctx context.Context, jobID string) (*aap.Job, error) {
					return nil, errors.New("received non-success status code 404: job not found")
				}
			})

			It("should return error", func() {
				_, err := provider.GetProvisionStatus(ctx, "invalid")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to get job"))
			})
		})

		Context("when AAP API fails", func() {
			BeforeEach(func() {
				aapClient.getJobFunc = func(ctx context.Context, jobID string) (*aap.Job, error) {
					return nil, errors.New("AAP connection error")
				}
			})

			It("should return error", func() {
				_, err := provider.GetProvisionStatus(ctx, "789")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to get job"))
			})
		})
	})

	Describe("TriggerDeprovision", func() {
		var instance *v1alpha1.ComputeInstance

		BeforeEach(func() {
			instance = &v1alpha1.ComputeInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-instance",
					Namespace: "default",
				},
			}
		})

		Context("with job template", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "provision-job", "deprovision-job")
				aapClient.getTemplateFunc = func(ctx context.Context, templateName string) (*aap.Template, error) {
					return &aap.Template{ID: 1, Name: templateName, Type: aap.TemplateTypeJob}, nil
				}
				aapClient.launchJobTemplateFunc = func(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error) {
					Expect(req.TemplateName).To(Equal("deprovision-job"))
					return &aap.LaunchJobTemplateResponse{JobID: 999}, nil
				}
			})

			It("should launch job template and return job ID", func() {
				result, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Action).To(Equal(provisioning.DeprovisionTriggered))
				Expect(result.JobID).To(Equal("999"))
				Expect(result.BlockDeletionOnFailure).To(BeTrue())
			})
		})

		Context("when template not configured", func() {
			BeforeEach(func() {
				provider = provisioning.NewAAPProvider(aapClient, "provision-job", "")
			})

			It("should return error", func() {
				_, err := provider.TriggerDeprovision(ctx, instance)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("deprovision template not configured"))
			})
		})
	})

	Describe("GetDeprovisionStatus", func() {
		BeforeEach(func() {
			provider = provisioning.NewAAPProvider(aapClient, "provision-job", "deprovision-job")
			aapClient.getJobFunc = func(ctx context.Context, jobID string) (*aap.Job, error) {
				var id int
				if _, err := fmt.Sscanf(jobID, "%d", &id); err != nil {
					id = 888 // default
				}
				return &aap.Job{
					ID:       id,
					Status:   "successful",
					Started:  time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
					Finished: time.Date(2024, 1, 1, 12, 3, 0, 0, time.UTC),
				}, nil
			}
		})

		It("should return job status", func() {
			status, err := provider.GetDeprovisionStatus(ctx, "888")
			Expect(err).NotTo(HaveOccurred())
			Expect(status.JobID).To(Equal("888"))
			Expect(status.State).To(Equal(provisioning.JobStateSucceeded))
		})
	})

	Describe("Name", func() {
		It("should return provider name", func() {
			Expect(provider.Name()).To(Equal(provisioning.ProviderTypeAAP))
		})
	})
})
