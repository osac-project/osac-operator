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
// DesiredConfigVersion and ReconciledConfigVersion are value snapshots captured at
// construction time — they are not updated if the instance status changes afterward.
type State struct {
	Jobs                    *[]v1alpha1.JobStatus
	DesiredConfigVersion    string
	ReconciledConfigVersion string
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
	default:
		return nil
	}
}

// EvaluateAction determines the next provisioning action based on job history and config versions.
func EvaluateAction(provState *State, checkAPIServer func() bool) (Action, *v1alpha1.JobStatus) {
	latestJob := FindLatestJobByType(*provState.Jobs, v1alpha1.JobTypeProvision)

	if !HasJobID(latestJob) {
		if provState.DesiredConfigVersion == provState.ReconciledConfigVersion {
			return Skip, latestJob
		}
	} else if !latestJob.State.IsTerminal() {
		return Poll, latestJob
	} else if latestJob.ConfigVersion != "" {
		if latestJob.ConfigVersion == provState.DesiredConfigVersion {
			if latestJob.State == v1alpha1.JobStateSucceeded {
				return Skip, latestJob
			}
			return Backoff, latestJob
		}
	} else if provState.DesiredConfigVersion == provState.ReconciledConfigVersion {
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
	// OnSuccess is called when the job succeeds (e.g. to update ReconciledVersion).
	OnSuccess func(status ProvisionStatus)
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
		updatedJob.Message = status.Message
		if status.ErrorDetails != "" {
			updatedJob.Message = fmt.Sprintf("%s: %s", status.Message, status.ErrorDetails)
		}
		UpdateJob(*provState.Jobs, updatedJob)

		if status.State == v1alpha1.JobStateFailed {
			log.Info("provision job failed", "jobID", latestJob.JobID)
			if callbacks != nil && callbacks.OnFailed != nil {
				callbacks.OnFailed(updatedJob.Message)
			}
		}
	}

	if !status.State.IsTerminal() {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	if status.State.IsSuccessful() && callbacks != nil && callbacks.OnSuccess != nil {
		callbacks.OnSuccess(status)
	}
	return ctrl.Result{}, nil
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

// SyncReconciledConfigVersion returns the reconciled config version from the given annotation key, or empty string if not set.
func SyncReconciledConfigVersion(ctx context.Context, annotations map[string]string, annotationKey string) string {
	log := ctrllog.FromContext(ctx)
	if version, exists := annotations[annotationKey]; exists {
		log.V(1).Info("copied reconciled config version from annotation", "version", version)
		return version
	}
	return ""
}
