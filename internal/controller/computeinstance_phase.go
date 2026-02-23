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

package controller

// ComputeInstance phase state machine.
//
// Each phase maps to a resolver function that determines the next phase
// based on the current reconciliation context. Resolver functions are pure:
// they receive the relevant objects and return the next phase.
//
// Valid transitions:
//
//	"" (empty)  -> Starting     new instance, first reconcile
//	Starting    -> Running      provisioning job succeeded, config versions match
//	Starting    -> Failed       provisioning job failed
//	Running     -> Updating     spec changed, config versions differ
//	Updating    -> Running      re-provisioning job succeeded, config versions match
//	Updating    -> Failed       re-provisioning job failed
//	*           -> Deleting     deletion timestamp set (handled by handleDelete)

import (
	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

// phaseResolver determines the next phase given the instance and its latest provision job.
// The job parameter may be nil when no provision job exists.
type phaseResolver func(instance *v1alpha1.ComputeInstance, job *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType

// phaseResolvers maps each phase to its resolver function.
var phaseResolvers = map[v1alpha1.ComputeInstancePhaseType]phaseResolver{
	"":                                    resolveEmptyPhase,
	v1alpha1.ComputeInstancePhaseStarting: resolveStartingPhase,
	v1alpha1.ComputeInstancePhaseRunning:  resolveRunningPhase,
	v1alpha1.ComputeInstancePhaseUpdating: resolveUpdatingPhase,
	v1alpha1.ComputeInstancePhaseFailed:   resolveFailedPhase,
	v1alpha1.ComputeInstancePhaseDeleting: resolveDeletingPhase,
}

// resolvePhase determines the next phase for the given instance and its latest provision job.
// If the current phase has no resolver, the phase is conserved.
func resolvePhase(instance *v1alpha1.ComputeInstance, job *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	resolver, ok := phaseResolvers[instance.Status.Phase]
	if !ok {
		return instance.Status.Phase
	}
	return resolver(instance, job)
}

// resolveEmptyPhase handles the initial state of a new ComputeInstance.
// "" -> Starting
func resolveEmptyPhase(_ *v1alpha1.ComputeInstance, _ *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	return v1alpha1.ComputeInstancePhaseStarting
}

// resolveStartingPhase handles transitions from the Starting phase.
// Starting -> Failed    when provision job failed
// Starting -> Running   when config versions match and no active provision job
// Starting -> Starting  otherwise (provisioning in progress)
func resolveStartingPhase(instance *v1alpha1.ComputeInstance, job *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	if job != nil && job.State.IsTerminal() && !job.State.IsSuccessful() {
		return v1alpha1.ComputeInstancePhaseFailed
	}
	if configVersionsMatch(instance) && !hasActiveJob(job) {
		return v1alpha1.ComputeInstancePhaseRunning
	}
	return v1alpha1.ComputeInstancePhaseStarting
}

// resolveRunningPhase handles transitions from the Running phase.
// Running -> Updating  when config versions differ
// Running -> Running   otherwise (no change)
func resolveRunningPhase(instance *v1alpha1.ComputeInstance, _ *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	if !configVersionsMatch(instance) {
		return v1alpha1.ComputeInstancePhaseUpdating
	}
	return v1alpha1.ComputeInstancePhaseRunning
}

// resolveUpdatingPhase handles transitions from the Updating phase.
// Updating -> Failed    when provision job failed
// Updating -> Running   when config versions match and no active provision job
// Updating -> Updating  otherwise (re-provisioning in progress)
func resolveUpdatingPhase(instance *v1alpha1.ComputeInstance, job *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	if job != nil && job.State.IsTerminal() && !job.State.IsSuccessful() {
		return v1alpha1.ComputeInstancePhaseFailed
	}
	if configVersionsMatch(instance) && !hasActiveJob(job) {
		return v1alpha1.ComputeInstancePhaseRunning
	}
	return v1alpha1.ComputeInstancePhaseUpdating
}

// resolveFailedPhase handles the Failed phase. No outgoing transitions.
// Failed -> Failed (terminal, except deletion handled by handleDelete)
func resolveFailedPhase(_ *v1alpha1.ComputeInstance, _ *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	return v1alpha1.ComputeInstancePhaseFailed
}

// resolveDeletingPhase handles the Deleting phase. No outgoing transitions.
// Deleting -> Deleting (terminal)
func resolveDeletingPhase(_ *v1alpha1.ComputeInstance, _ *v1alpha1.JobStatus) v1alpha1.ComputeInstancePhaseType {
	return v1alpha1.ComputeInstancePhaseDeleting
}

// configVersionsMatch returns true when the desired config has been reconciled.
func configVersionsMatch(instance *v1alpha1.ComputeInstance) bool {
	return instance.Status.DesiredConfigVersion != "" &&
		instance.Status.DesiredConfigVersion == instance.Status.ReconciledConfigVersion
}

// hasActiveJob returns true when a job exists and has not reached a terminal state.
func hasActiveJob(job *v1alpha1.JobStatus) bool {
	return job != nil && !job.State.IsTerminal()
}
