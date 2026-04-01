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

package provisioning

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

// State points into the resource's status fields used by the provisioning lifecycle.
// Jobs is a pointer so shared functions can modify the slice in place.
// DesiredConfigVersion is a value snapshot captured at construction time — it is
// not updated if the instance status changes afterward.
type State struct {
	Jobs                 *[]v1alpha1.JobStatus
	DesiredConfigVersion string
}

// GetJobsFromResource extracts the jobs array from a resource.
// Returns an empty slice for resource types that don't track jobs.
func GetJobsFromResource(resource client.Object) []v1alpha1.JobStatus {
	switch r := resource.(type) {
	case *v1alpha1.ComputeInstance:
		return r.Status.Jobs
	case *v1alpha1.ClusterOrder:
		return r.Status.Jobs
	case *v1alpha1.HostPool:
		return r.Status.Jobs
	case *v1alpha1.VirtualNetwork:
		return r.Status.Jobs
	case *v1alpha1.Subnet:
		return r.Status.Jobs
	case *v1alpha1.SecurityGroup:
		return r.Status.Jobs
	default:
		return nil
	}
}

// EvaluateAction determines the next provisioning action based on job history and config versions.
func EvaluateAction(provState *State, checkAPIServer func() bool) (Action, *v1alpha1.JobStatus) {
	latestJob := FindLatestJobByType(*provState.Jobs, v1alpha1.JobTypeProvision)

	if !HasJobID(latestJob) {
		// No provision job exists — trigger one.
		// This is intentional: resources without job history (new, imported, or trimmed by
		// maxJobHistory) should be provisioned. With AAP direct, job tracking is the source
		// of truth; the old annotation-based skip path has been removed.
	} else if !latestJob.State.IsTerminal() {
		return Poll, latestJob
	} else if latestJob.ConfigVersion == provState.DesiredConfigVersion {
		if latestJob.State == v1alpha1.JobStateSucceeded {
			return Skip, latestJob
		}
		return Backoff, latestJob
	} else if latestJob.ConfigVersion == "" && latestJob.State == v1alpha1.JobStateSucceeded {
		// Legacy job without ConfigVersion that succeeded — skip
		return Skip, latestJob
	}

	if checkAPIServer() {
		return Requeue, nil
	}
	return Trigger, latestJob
}

// CheckAPIServerForNonTerminalProvisionJob reads the resource directly from the API server
// and returns true if a non-terminal provision job exists.
func CheckAPIServerForNonTerminalProvisionJob(ctx context.Context, apiReader client.Reader, key client.ObjectKey, fresh client.Object) bool {
	log := ctrllog.FromContext(ctx)
	if err := apiReader.Get(ctx, key, fresh); err != nil {
		return false
	}
	freshJobs := GetJobsFromResource(fresh)
	freshJob := FindLatestJobByType(freshJobs, v1alpha1.JobTypeProvision)
	if HasJobID(freshJob) && !freshJob.State.IsTerminal() {
		log.Info("skipping provision trigger: non-terminal job found via API server", "jobID", freshJob.JobID, "state", freshJob.State)
		return true
	}
	return false
}

// TriggerJob triggers a new provision job and updates the jobs slice in place via State.
func TriggerJob(ctx context.Context, provider ProvisioningProvider, resource client.Object, provState *State, maxHistory int, pollInterval time.Duration) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("triggering provision job")

	result, err := provider.TriggerProvision(ctx, resource)
	if err != nil {
		if rateLimitErr, ok := AsRateLimitError(err); ok {
			log.Info("provision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
			return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to trigger provision: %w", err)
	}

	*provState.Jobs = AppendJob(*provState.Jobs, v1alpha1.JobStatus{
		JobID:         result.JobID,
		Type:          v1alpha1.JobTypeProvision,
		State:         result.InitialState,
		Message:       result.Message,
		Timestamp:     metav1.NewTime(time.Now().UTC()),
		ConfigVersion: provState.DesiredConfigVersion,
	}, maxHistory)

	latestJob := FindLatestJobByType(*provState.Jobs, v1alpha1.JobTypeProvision)
	log.Info("provision job triggered", "jobID", latestJob.JobID, "configVersion", latestJob.ConfigVersion)
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// PollCallbacks holds optional callbacks for provision job state transitions.
type PollCallbacks struct {
	// OnFailed is called when the job transitions to Failed state.
	OnFailed func(message string)
	// OnSuccess is called when the job succeeds.
	OnSuccess func(status ProvisionStatus)
	// IsCompleted is called when the provider returns a non-terminal state.
	// If it returns true, the job is marked as succeeded and polling stops.
	// Used by EDA provider where GetProvisionStatus always returns Unknown.
	IsCompleted func() bool
}

// PollJob checks the status of an existing provision job and updates the jobs slice in place.
func PollJob(ctx context.Context, provider ProvisioningProvider, resource client.Object, provState *State, latestJob *v1alpha1.JobStatus, pollInterval time.Duration, callbacks *PollCallbacks) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("polling provision job status", "jobID", latestJob.JobID, "currentState", latestJob.State)

	status, err := provider.GetProvisionStatus(ctx, resource, latestJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision status", "jobID", latestJob.JobID)
		updatedJob := *latestJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		UpdateJob(*provState.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	if status.State != latestJob.State || status.Message != latestJob.Message {
		log.Info("provision job status changed", "jobID", latestJob.JobID, "oldState", latestJob.State, "newState", status.State)
		updatedJob := *latestJob
		updatedJob.State = status.State
		updatedJob.Message = status.MessageWithDetails()
		UpdateJob(*provState.Jobs, updatedJob)

		if status.State == v1alpha1.JobStateFailed {
			log.Info("provision job failed", "jobID", latestJob.JobID)
			if callbacks != nil && callbacks.OnFailed != nil {
				callbacks.OnFailed(updatedJob.Message)
			}
		}
	}

	if !status.State.IsTerminal() {
		// Check if an external signal indicates completion (e.g., EDA where
		// GetProvisionStatus always returns Unknown but the VM was created).
		if callbacks != nil && callbacks.IsCompleted != nil && callbacks.IsCompleted() {
			log.Info("provision job completed via external signal", "jobID", latestJob.JobID)
			updatedJob := *latestJob
			updatedJob.State = v1alpha1.JobStateSucceeded
			updatedJob.Message = "provision completed"
			UpdateJob(*provState.Jobs, updatedJob)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	if status.State.IsSuccessful() && callbacks != nil && callbacks.OnSuccess != nil {
		callbacks.OnSuccess(status)
	}
	return ctrl.Result{}, nil
}

// RunProvisioningLifecycle encapsulates the full provisioning flow: evaluate action,
// trigger/poll/backoff as needed. Controllers call this instead of duplicating the
// switch statement. The callbacks customize behavior on success and failure.
func RunProvisioningLifecycle(
	ctx context.Context,
	provider ProvisioningProvider,
	resource client.Object,
	provState *State,
	maxHistory int,
	pollInterval time.Duration,
	callbacks *PollCallbacks,
	checkAPIServer func() bool,
) (ctrl.Result, error) {
	action, latestJob := EvaluateAction(provState, checkAPIServer)

	trigger := func() (ctrl.Result, error) {
		return TriggerJob(ctx, provider, resource, provState, maxHistory, pollInterval)
	}

	switch action {
	case Skip:
		return ctrl.Result{}, nil
	case Trigger:
		return trigger()
	case Requeue:
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	case Backoff:
		return HandleBackoff(ctx, provState, latestJob, trigger)
	default: // Poll
		return PollJob(ctx, provider, resource, provState, latestJob, pollInterval, callbacks)
	}
}

// IsConfigApplied returns true if the latest provision job succeeded with a ConfigVersion
// matching the desired version, indicating the current spec has been applied.
// Also returns true for legacy jobs (empty ConfigVersion) that succeeded, to avoid
// re-triggering provisioning for resources provisioned before ConfigVersion tracking.
func IsConfigApplied(jobs *[]v1alpha1.JobStatus, desiredConfigVersion string) bool {
	latestJob := FindLatestJobByType(*jobs, v1alpha1.JobTypeProvision)
	if latestJob == nil || latestJob.State != v1alpha1.JobStateSucceeded {
		return false
	}
	// Legacy job without ConfigVersion — treat as applied
	if latestJob.ConfigVersion == "" {
		return true
	}
	return latestJob.ConfigVersion == desiredConfigVersion
}

// ComputeDesiredConfigVersion computes a hash of the spec and returns it.
// The caller must pass the resource's Spec field (not the entire resource).
func ComputeDesiredConfigVersion(spec any) (string, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("failed to marshal spec to JSON: %w", err)
	}
	hasher := fnv.New64a()
	if _, err := hasher.Write(specJSON); err != nil {
		return "", fmt.Errorf("failed to write to hash: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// TriggerDeprovisionJob triggers a deprovision job via the provider and handles the result.
// Updates the jobs slice in place. Returns the result for the controller to return.
func TriggerDeprovisionJob(ctx context.Context, provider ProvisioningProvider, resource client.Object,
	jobs *[]v1alpha1.JobStatus, maxHistory int, pollInterval time.Duration) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("triggering deprovision job")

	result, err := provider.TriggerDeprovision(ctx, resource)
	if err != nil {
		if rateLimitErr, ok := AsRateLimitError(err); ok {
			log.Info("deprovision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
			return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to trigger deprovision: %w", err)
	}

	switch result.Action {
	case DeprovisionSkipped:
		log.Info("deprovisioning skipped by provider")
		return ctrl.Result{}, nil

	case DeprovisionWaiting:
		log.Info("waiting for provision job to terminate before deprovisioning")
		updateProvisionJobFromDeprovisionResult(jobs, result)
		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case DeprovisionTriggered:
		log.Info("deprovision job triggered", "jobID", result.JobID)
		updateProvisionJobFromDeprovisionResult(jobs, result)
		*jobs = AppendJob(*jobs, v1alpha1.JobStatus{
			JobID:                  result.JobID,
			Type:                   v1alpha1.JobTypeDeprovision,
			State:                  v1alpha1.JobStatePending,
			Message:                "Deprovision job triggered",
			Timestamp:              metav1.NewTime(time.Now().UTC()),
			BlockDeletionOnFailure: result.BlockDeletionOnFailure,
		}, maxHistory)
		return ctrl.Result{RequeueAfter: pollInterval}, nil

	default:
		return ctrl.Result{}, fmt.Errorf("unknown deprovision action: %v", result.Action)
	}
}

// updateProvisionJobFromDeprovisionResult updates the latest provision job status
// from the deprovision result, if provided by the provider.
func updateProvisionJobFromDeprovisionResult(jobs *[]v1alpha1.JobStatus, result *DeprovisionResult) {
	if result.ProvisionJobStatus == nil {
		return
	}
	latestProvisionJob := FindLatestJobByType(*jobs, v1alpha1.JobTypeProvision)
	if latestProvisionJob == nil {
		return
	}
	updatedJob := *latestProvisionJob
	updatedJob.State = result.ProvisionJobStatus.State
	updatedJob.Message = result.ProvisionJobStatus.MessageWithDetails()
	UpdateJob(*jobs, updatedJob)
}

// PollDeprovisionJob polls the status of an existing deprovision job.
// Returns (result, done, error) where done=true means the job reached terminal state
// and the controller can proceed with finalizer removal.
func PollDeprovisionJob(ctx context.Context, provider ProvisioningProvider, resource client.Object,
	jobs *[]v1alpha1.JobStatus, latestDeprovisionJob *v1alpha1.JobStatus, pollInterval time.Duration) (ctrl.Result, bool, error) {
	log := ctrllog.FromContext(ctx)

	if latestDeprovisionJob.State.IsTerminal() {
		// Already terminal — check if deletion should be blocked
		if !latestDeprovisionJob.State.IsSuccessful() && latestDeprovisionJob.BlockDeletionOnFailure {
			log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
				"jobID", latestDeprovisionJob.JobID, "state", latestDeprovisionJob.State)
			return ctrl.Result{RequeueAfter: pollInterval}, false, nil
		}
		return ctrl.Result{}, true, nil
	}

	log.Info("polling deprovision job status", "jobID", latestDeprovisionJob.JobID, "currentState", latestDeprovisionJob.State)
	status, err := provider.GetDeprovisionStatus(ctx, resource, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get deprovision status: %v", err)
		UpdateJob(*jobs, updatedJob)
		return ctrl.Result{RequeueAfter: pollInterval}, false, nil
	}

	// Update job status if changed
	if status.State != latestDeprovisionJob.State || status.Message != latestDeprovisionJob.Message {
		log.Info("deprovision job status changed", "jobID", latestDeprovisionJob.JobID,
			"oldState", latestDeprovisionJob.State, "newState", status.State)
		updatedJob := *latestDeprovisionJob
		updatedJob.State = status.State
		updatedJob.Message = status.MessageWithDetails()
		UpdateJob(*jobs, updatedJob)
	}

	// Continue polling if still running
	if !status.State.IsTerminal() {
		return ctrl.Result{RequeueAfter: pollInterval}, false, nil
	}

	// Job reached terminal state
	if !status.State.IsSuccessful() && latestDeprovisionJob.BlockDeletionOnFailure {
		log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
			"jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: pollInterval}, false, nil
	}

	return ctrl.Result{}, true, nil
}
