// Package provisioning provides an abstraction layer for infrastructure provisioning through multiple backends.
package provisioning

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ProviderTypeEDA identifies the EDA webhook-based provider
	ProviderTypeEDA = "eda"

	// ProviderTypeAAP identifies the AAP REST API direct provider
	ProviderTypeAAP = "aap"
)

// ProvisionResult contains the result of triggering a provision operation.
type ProvisionResult struct {
	// JobID is the identifier for the triggered job
	JobID string

	// InitialState is the initial state of the job (typically Pending or Running)
	InitialState JobState

	// Message is a human-readable status message
	Message string
}

// DeprovisionAction represents the action taken by a provider when attempting to deprovision.
type DeprovisionAction string

const (
	// DeprovisionTriggered means deprovisioning was started and controller should poll status
	DeprovisionTriggered DeprovisionAction = "triggered"

	// DeprovisionWaiting means provider is not ready yet and controller should requeue
	DeprovisionWaiting DeprovisionAction = "waiting"

	// DeprovisionSkipped means provider determined deprovisioning is not needed
	DeprovisionSkipped DeprovisionAction = "skipped"
)

// DeprovisionResult contains the result of attempting to trigger a deprovision operation.
type DeprovisionResult struct {
	// Action indicates what the provider did
	Action DeprovisionAction

	// JobID is the tracking identifier when Action==DeprovisionTriggered
	// Used by controller to poll status via GetDeprovisionStatus()
	// Empty when Action==DeprovisionWaiting or Action==DeprovisionSkipped
	JobID string

	// BlockDeletionOnFailure indicates whether deletion should be blocked if deprovision fails
	// Stored in CR status for crash recovery (controller restart)
	// Providers set based on their cleanup guarantees
	BlockDeletionOnFailure bool
}

// ProvisioningProvider abstracts the mechanism for triggering infrastructure automation
// and retrieving job status. This interface allows multiple implementations (e.g., EDA webhooks,
// direct AAP API integration) to coexist and be selected via configuration.
type ProvisioningProvider interface {
	// TriggerProvision starts provisioning for a resource.
	// Returns a ProvisionResult with job details and initial state.
	TriggerProvision(ctx context.Context, resource client.Object) (*ProvisionResult, error)

	// GetProvisionStatus checks the status of a provisioning job.
	GetProvisionStatus(ctx context.Context, jobID string) (ProvisionStatus, error)

	// TriggerDeprovision attempts to deprovision a resource.
	// The provider performs any prerequisite checks and returns an action indicating
	// whether deprovisioning was triggered, needs to wait, or should be skipped.
	TriggerDeprovision(ctx context.Context, resource client.Object) (*DeprovisionResult, error)

	// GetDeprovisionStatus checks the status of a deprovisioning job.
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

	// JobStateCanceled indicates the job was canceled before completion.
	JobStateCanceled JobState = "Canceled"
)

// IsTerminal returns true if the job state is in a terminal state (succeeded, failed, or canceled).
func (s JobState) IsTerminal() bool {
	return s == JobStateSucceeded || s == JobStateFailed || s == JobStateCanceled
}

// IsSuccessful returns true if the job completed successfully.
func (s JobState) IsSuccessful() bool {
	return s == JobStateSucceeded
}
