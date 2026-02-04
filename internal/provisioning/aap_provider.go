package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/innabox/cloudkit-operator/internal/aap"
)

// AAPClient is the interface for AAP operations used by the provider.
type AAPClient interface {
	GetTemplate(ctx context.Context, templateName string) (*aap.Template, error)
	LaunchJobTemplate(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error)
	LaunchWorkflowTemplate(ctx context.Context, req aap.LaunchWorkflowTemplateRequest) (*aap.LaunchWorkflowTemplateResponse, error)
	GetJob(ctx context.Context, jobID int) (*aap.Job, error)
}

// AAPProvider implements ProvisioningProvider using direct AAP REST API integration.
type AAPProvider struct {
	client              AAPClient
	provisionTemplate   string
	deprovisionTemplate string
}

// NewAAPProvider creates a new AAP provider.
func NewAAPProvider(client AAPClient, provisionTemplate, deprovisionTemplate string) *AAPProvider {
	return &AAPProvider{
		client:              client,
		provisionTemplate:   provisionTemplate,
		deprovisionTemplate: deprovisionTemplate,
	}
}

// TriggerProvision triggers provisioning via AAP API.
// Autodetects whether the template is a job_template or workflow_job_template.
func (p *AAPProvider) TriggerProvision(ctx context.Context, resource client.Object) (string, error) {
	if p.provisionTemplate == "" {
		return "", fmt.Errorf("provision template not configured")
	}

	template, err := p.client.GetTemplate(ctx, p.provisionTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to get template: %w", err)
	}

	// Extract extra vars from resource
	extraVars, err := extractExtraVars(resource)
	if err != nil {
		return "", fmt.Errorf("failed to extract extra vars: %w", err)
	}

	// Launch appropriate template type
	var jobID int
	switch template.Type {
	case aap.TemplateTypeJob:
		resp, err := p.client.LaunchJobTemplate(ctx, aap.LaunchJobTemplateRequest{
			TemplateName: p.provisionTemplate,
			ExtraVars:    extraVars,
		})
		if err != nil {
			return "", fmt.Errorf("failed to launch job template: %w", err)
		}
		jobID = resp.JobID
	case aap.TemplateTypeWorkflow:
		resp, err := p.client.LaunchWorkflowTemplate(ctx, aap.LaunchWorkflowTemplateRequest{
			TemplateName: p.provisionTemplate,
			ExtraVars:    extraVars,
		})
		if err != nil {
			return "", fmt.Errorf("failed to launch workflow template: %w", err)
		}
		jobID = resp.JobID
	default:
		return "", fmt.Errorf("unknown template type: %s", template.Type)
	}

	return strconv.Itoa(jobID), nil
}

// GetProvisionStatus checks provisioning job status via AAP API.
func (p *AAPProvider) GetProvisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error) {
	return p.getJobStatus(ctx, jobID)
}

// TriggerDeprovision triggers deprovisioning via AAP API.
// Autodetects whether the template is a job_template or workflow_job_template.
func (p *AAPProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (string, error) {
	if p.deprovisionTemplate == "" {
		return "", fmt.Errorf("deprovision template not configured")
	}

	template, err := p.client.GetTemplate(ctx, p.deprovisionTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to get template: %w", err)
	}

	// Extract extra vars from resource
	extraVars, err := extractExtraVars(resource)
	if err != nil {
		return "", fmt.Errorf("failed to extract extra vars: %w", err)
	}

	// Launch appropriate template type
	var jobID int
	switch template.Type {
	case aap.TemplateTypeJob:
		resp, err := p.client.LaunchJobTemplate(ctx, aap.LaunchJobTemplateRequest{
			TemplateName: p.deprovisionTemplate,
			ExtraVars:    extraVars,
		})
		if err != nil {
			return "", fmt.Errorf("failed to launch job template: %w", err)
		}
		jobID = resp.JobID
	case aap.TemplateTypeWorkflow:
		resp, err := p.client.LaunchWorkflowTemplate(ctx, aap.LaunchWorkflowTemplateRequest{
			TemplateName: p.deprovisionTemplate,
			ExtraVars:    extraVars,
		})
		if err != nil {
			return "", fmt.Errorf("failed to launch workflow template: %w", err)
		}
		jobID = resp.JobID
	default:
		return "", fmt.Errorf("unknown template type: %s", template.Type)
	}

	return strconv.Itoa(jobID), nil
}

// GetDeprovisionStatus checks deprovisioning job status via AAP API.
func (p *AAPProvider) GetDeprovisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error) {
	return p.getJobStatus(ctx, jobID)
}

// Name returns the provider name for logging.
func (p *AAPProvider) Name() string {
	return "aap"
}

// getJobStatus retrieves job status from AAP and converts it to ProvisionStatus.
func (p *AAPProvider) getJobStatus(ctx context.Context, jobID string) (ProvisionStatus, error) {
	jobIDInt, err := strconv.Atoi(jobID)
	if err != nil {
		return ProvisionStatus{}, fmt.Errorf("invalid job ID: %w", err)
	}

	job, err := p.client.GetJob(ctx, jobIDInt)
	if err != nil {
		return ProvisionStatus{}, fmt.Errorf("failed to get job: %w", err)
	}

	status := ProvisionStatus{
		JobID:     jobID,
		State:     mapAAPStatusToJobState(job.Status),
		Message:   job.Status,
		StartTime: job.Started,
		EndTime:   job.Finished,
	}

	// Populate error details if job failed
	if status.State == JobStateFailed && job.ResultTraceback != "" {
		status.ErrorDetails = job.ResultTraceback
	}

	return status, nil
}

// mapAAPStatusToJobState converts AAP job status to JobState.
func mapAAPStatusToJobState(aapStatus string) JobState {
	switch aapStatus {
	case "successful":
		return JobStateSucceeded
	case "failed", "error", "canceled":
		return JobStateFailed
	case "pending", "waiting", "running":
		return JobStateRunning
	default:
		// Unknown states are treated as running to avoid premature termination
		return JobStateRunning
	}
}

// extractExtraVars extracts extra variables from a resource to pass to AAP.
//
// NOTE: The current AAP templates (innabox-create-compute-instance, innabox-delete-compute-instance)
// were designed to be triggered by EDA (Event-Driven Ansible) and expect the full Kubernetes resource
// object wrapped in an EDA event structure. To maintain compatibility with existing templates, we
// serialize the entire resource object and wrap it in the ansible_eda.event.payload structure.
//
// EDA sends the complete resource object which allows playbooks to access fields like:
//
//	ansible_eda.event.payload.spec.templateID
//	ansible_eda.event.payload.spec.templateParameters
//	ansible_eda.event.payload.metadata.name
//	ansible_eda.event.payload.metadata.namespace
//
// Future improvement: When/if we migrate away from EDA-triggered templates, this wrapper can be
// removed and parameters can be passed directly as flat key-value pairs.
func extractExtraVars(resource client.Object) (map[string]interface{}, error) {
	// Convert the resource to map using JSON marshaling (respects JSON tags)
	resourceMap, err := serializeResource(resource)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize resource: %w", err)
	}

	// Wrap the full resource in EDA event structure for compatibility with EDA-designed templates
	return map[string]interface{}{
		"ansible_eda": map[string]interface{}{
			"event": map[string]interface{}{
				"payload": resourceMap,
			},
		},
	}, nil
}

// serializeResource converts a Kubernetes resource to a map using JSON marshaling.
// This respects the struct's JSON tags and provides the same structure as EDA events.
func serializeResource(resource client.Object) (map[string]interface{}, error) {
	// Marshal to JSON
	jsonBytes, err := json.Marshal(resource)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal resource to JSON: %w", err)
	}

	// Unmarshal back to map[string]interface{}
	var resourceMap map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &resourceMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON to map: %w", err)
	}

	return resourceMap, nil
}
