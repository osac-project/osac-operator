package provisioning

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProvisioningProvider abstracts the mechanism for triggering infrastructure automation
// and retrieving job status. This interface allows multiple implementations (e.g., EDA webhooks,
// direct AAP API integration) to coexist and be selected via configuration.
type ProvisioningProvider interface {
	// TriggerProvision starts provisioning for a resource.
	// Returns a job ID that can be used to track status.
	TriggerProvision(ctx context.Context, resource client.Object) (jobID string, err error)

	// GetProvisionStatus checks the status of a provisioning job.
	// Returns current status and whether the job is complete.
	GetProvisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error)

	// TriggerDeprovision starts deprovisioning for a resource.
	// Returns a job ID that can be used to track status.
	TriggerDeprovision(ctx context.Context, resource client.Object) (jobID string, err error)

	// GetDeprovisionStatus checks the status of a deprovisioning job.
	// Returns current status and whether the job is complete.
	GetDeprovisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error)

	// Name returns the provider name for logging and identification.
	Name() string
}

// ProvisionStatus represents the current state of a provisioning or deprovisioning job.
type ProvisionStatus struct {
	// JobID is the unique identifier for this job.
	JobID string

	// State indicates the current state of the job.
	State JobState

	// Message provides a human-readable status message.
	Message string

	// Progress indicates completion percentage (0-100).
	// Optional: providers that don't support progress tracking should leave this at 0.
	Progress int

	// StartTime is when the job started execution.
	StartTime time.Time

	// EndTime is when the job completed (succeeded or failed).
	// Zero value indicates job is still running.
	EndTime time.Time

	// ReconciledVersion is the configuration version that was successfully applied.
	// Only populated when State is JobStateSucceeded.
	ReconciledVersion string

	// ErrorDetails contains detailed error information when State is JobStateFailed.
	ErrorDetails string
}

// JobState represents the state of a provisioning job.
type JobState string

const (
	// JobStatePending indicates the job has been created but not yet started.
	JobStatePending JobState = "Pending"

	// JobStateRunning indicates the job is currently executing.
	JobStateRunning JobState = "Running"

	// JobStateSucceeded indicates the job completed successfully.
	JobStateSucceeded JobState = "Succeeded"

	// JobStateFailed indicates the job failed.
	JobStateFailed JobState = "Failed"
)

// IsTerminal returns true if the job state is in a terminal state (succeeded or failed).
func (s JobState) IsTerminal() bool {
	return s == JobStateSucceeded || s == JobStateFailed
}

// IsSuccessful returns true if the job completed successfully.
func (s JobState) IsSuccessful() bool {
	return s == JobStateSucceeded
}
