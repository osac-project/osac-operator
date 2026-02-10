package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
	"github.com/innabox/cloudkit-operator/internal/aap"
)

// AAPClient is the interface for AAP operations used by the provider.
type AAPClient interface {
	GetTemplate(ctx context.Context, templateName string) (*aap.Template, error)
	LaunchJobTemplate(ctx context.Context, req aap.LaunchJobTemplateRequest) (*aap.LaunchJobTemplateResponse, error)
	LaunchWorkflowTemplate(ctx context.Context, req aap.LaunchWorkflowTemplateRequest) (*aap.LaunchWorkflowTemplateResponse, error)
	GetJob(ctx context.Context, jobID string) (*aap.Job, error)
	CancelJob(ctx context.Context, jobID string) error
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
func (p *AAPProvider) TriggerProvision(ctx context.Context, resource client.Object) (*ProvisionResult, error) {
	jobID, err := p.launchProvisionJob(ctx, resource)
	if err != nil {
		return nil, err
	}

	return &ProvisionResult{
		JobID:        jobID,
		InitialState: JobStatePending,
		Message:      "Provisioning job triggered",
	}, nil
}

// launchProvisionJob launches the provision template and returns the job ID.
func (p *AAPProvider) launchProvisionJob(ctx context.Context, resource client.Object) (string, error) {
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
func (p *AAPProvider) GetProvisionStatus(ctx context.Context, resource client.Object, jobID string) (ProvisionStatus, error) {
	return p.getJobStatus(ctx, jobID)
}

// TriggerDeprovision attempts to start deprovisioning for a resource.
func (p *AAPProvider) TriggerDeprovision(ctx context.Context, resource client.Object) (*DeprovisionResult, error) {
	instance := resource.(*v1alpha1.ComputeInstance)

	// Check if provision job needs to be terminated first
	ready, provisionState, err := p.isReadyForDeprovision(ctx, instance)
	if err != nil {
		return nil, err
	}
	if !ready {
		return &DeprovisionResult{
			Action:                 DeprovisionWaiting,
			BlockDeletionOnFailure: true,
			ProvisionJobState:      provisionState,
		}, nil
	}

	// Launch deprovision job
	jobID, err := p.launchDeprovisionJob(ctx, resource)
	if err != nil {
		return nil, err
	}

	return &DeprovisionResult{
		Action:                 DeprovisionTriggered,
		JobID:                  jobID,
		BlockDeletionOnFailure: true,
	}, nil
}

// isReadyForDeprovision checks if provision job is terminal before deprovisioning.
// Returns (ready, currentProvisionState, error).
// - ready: true if ready to deprovision, false if need to wait for provision job cancellation
// - currentProvisionState: the actual provision job state from AAP (used to update CR status)
// - error: any error encountered during the check
func (p *AAPProvider) isReadyForDeprovision(ctx context.Context, instance *v1alpha1.ComputeInstance) (bool, JobState, error) {
	log := ctrllog.FromContext(ctx)

	// No provision job or job ID missing - ready to proceed
	if instance.Status.ProvisionJob == nil {
		log.Info("no provision job found in status, ready to deprovision")
		return true, "", nil
	}
	if instance.Status.ProvisionJob.ID == "" {
		log.Info("provision job exists but ID is empty, ready to deprovision")
		return true, "", nil
	}

	log.Info("checking provision job before deprovision", "jobID", instance.Status.ProvisionJob.ID, "currentState", instance.Status.ProvisionJob.State)

	// Check provision job status
	status, err := p.GetProvisionStatus(ctx, instance, instance.Status.ProvisionJob.ID)
	if err != nil {
		var notFoundErr *aap.NotFoundError
		if errors.As(err, &notFoundErr) {
			log.Info("AAP job not found (purged), treating as terminal", "jobID", instance.Status.ProvisionJob.ID)
			return true, "", nil
		}
		return false, "", fmt.Errorf("failed to get provision job status: %w", err)
	}

	log.Info("provision job status retrieved", "jobID", instance.Status.ProvisionJob.ID, "state", status.State, "isTerminal", status.State.IsTerminal())

	// Job already terminal - ready to proceed
	if status.State.IsTerminal() {
		log.Info("provision job is terminal, ready to deprovision", "jobID", instance.Status.ProvisionJob.ID, "state", status.State)
		return true, status.State, nil
	}

	// Job still running - cancel it
	log.Info("provision job is running, attempting to cancel", "jobID", instance.Status.ProvisionJob.ID, "state", status.State)
	if err := p.cancelProvisionJob(ctx, instance.Status.ProvisionJob.ID); err != nil {
		var methodNotAllowedErr *aap.MethodNotAllowedError
		if !errors.As(err, &methodNotAllowedErr) {
			return false, status.State, fmt.Errorf("failed to cancel provision job: %w", err)
		}
		// 405 means already terminal, proceed
		log.Info("job cancel returned 405 (already terminal), ready to deprovision", "jobID", instance.Status.ProvisionJob.ID)
		return true, status.State, nil
	}

	// Cancellation initiated - need to wait, return current state for CR update
	log.Info("provision job cancellation initiated, waiting for termination", "jobID", instance.Status.ProvisionJob.ID)
	return false, status.State, nil
}

// cancelProvisionJob attempts to cancel a running provision job via AAP API.
// Returns nil if cancellation was initiated successfully or if the job is already in a terminal state (HTTP 405).
// Note: Cancellation is asynchronous. The job status should be polled to confirm termination.
func (p *AAPProvider) cancelProvisionJob(ctx context.Context, jobID string) error {
	// Attempt to cancel the job
	// HTTP 202 → cancellation initiated
	// HTTP 405 → job already terminal (not an error)
	err := p.client.CancelJob(ctx, jobID)
	if err != nil {
		// Check if error is "Method not allowed" (405) - indicates job already terminal
		var methodNotAllowedErr *aap.MethodNotAllowedError
		if errors.As(err, &methodNotAllowedErr) {
			// Job is already in terminal state, nothing to cancel
			return nil
		}
		return fmt.Errorf("failed to cancel job: %w", err)
	}

	return nil
}

// launchDeprovisionJob launches the deprovision template and returns the job ID.
func (p *AAPProvider) launchDeprovisionJob(ctx context.Context, resource client.Object) (string, error) {
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
func (p *AAPProvider) GetDeprovisionStatus(ctx context.Context, resource client.Object, jobID string) (ProvisionStatus, error) {
	return p.getJobStatus(ctx, jobID)
}

// Name returns the provider name for logging.
func (p *AAPProvider) Name() string {
	return "aap"
}

// getJobStatus retrieves job status from AAP and converts it to ProvisionStatus.
func (p *AAPProvider) getJobStatus(ctx context.Context, jobID string) (ProvisionStatus, error) {
	job, err := p.client.GetJob(ctx, jobID)
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
	case "failed", "error":
		return JobStateFailed
	case "canceled":
		return JobStateCanceled
	case "pending":
		return JobStatePending
	case "waiting":
		return JobStateWaiting
	case "running":
		return JobStateRunning
	default:
		// Unknown states should be marked as Unknown (non-terminal) to allow continued polling
		return JobStateUnknown
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
func extractExtraVars(resource client.Object) (map[string]any, error) {
	// Convert the resource to map using JSON marshaling (respects JSON tags)
	resourceMap, err := serializeResource(resource)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize resource: %w", err)
	}

	// Wrap the full resource in EDA event structure for compatibility with EDA-designed templates
	return map[string]any{
		"ansible_eda": map[string]any{
			"event": map[string]any{
				"payload": resourceMap,
			},
		},
	}, nil
}

// serializeResource converts a Kubernetes resource to a map using JSON marshaling.
// This respects the struct's JSON tags and provides the same structure as EDA events.
func serializeResource(resource client.Object) (map[string]any, error) {
	// Marshal to JSON
	jsonBytes, err := json.Marshal(resource)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal resource to JSON: %w", err)
	}

	// Unmarshal back to map[string]any
	var resourceMap map[string]any
	if err := json.Unmarshal(jsonBytes, &resourceMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON to map: %w", err)
	}

	return resourceMap, nil
}
