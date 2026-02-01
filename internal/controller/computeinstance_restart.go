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

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/innabox/cloudkit-operator/api/v1alpha1"
)

const (
	ReasonRestartRequested  = "RestartRequested"
	ReasonRestartInProgress = "RestartInProgress"
	ReasonNoVMReference     = "NoVMReference"
	ReasonVMIDeletionFailed = "VMIDeletionFailed"
)

// handleRestartRequest processes restart requests for a ComputeInstance.
// It uses a simple declarative pattern: if spec.restartRequestedAt > status.lastRestartedAt,
// execute the restart.
func (r *ComputeInstanceReconciler) handleRestartRequest(ctx context.Context, ci *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Check if restart is requested
	if ci.Spec.RestartRequestedAt == nil {
		// No restart requested, clear any restart conditions
		r.clearRestartConditions(ctx, ci)
		// Status will be updated by the main Reconcile loop
		return ctrl.Result{}, nil
	}

	// Check if this is a new restart request
	if ci.Status.LastRestartedAt != nil &&
		!ci.Spec.RestartRequestedAt.After(ci.Status.LastRestartedAt.Time) {
		// Already processed this restart request, clear in-progress condition
		r.clearRestartConditions(ctx, ci)
		// Status will be updated by the main Reconcile loop
		return ctrl.Result{}, nil
	}

	log.Info("New restart request detected",
		"requestedAt", ci.Spec.RestartRequestedAt.Time.Format(time.RFC3339))

	// Execute the restart
	return r.performRestart(ctx, ci)
}

// performRestart executes the VM restart by deleting the VirtualMachineInstance.
// KubeVirt will automatically recreate the VMI, resulting in a restart.
func (r *ComputeInstanceReconciler) performRestart(ctx context.Context, ci *v1alpha1.ComputeInstance) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Check if VirtualMachineReference exists
	if ci.Status.VirtualMachineReference == nil {
		log.Info("No VirtualMachineReference found, cannot perform restart")
		meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
			Type:               string(v1alpha1.ComputeInstanceConditionRestartFailed),
			Status:             metav1.ConditionTrue,
			Reason:             ReasonNoVMReference,
			Message:            "No VirtualMachine reference found",
			ObservedGeneration: ci.Generation,
		})
		// Status will be updated by the main Reconcile loop
		return ctrl.Result{}, nil
	}

	// Get VirtualMachineInstance
	vmi := &kubevirtv1.VirtualMachineInstance{}
	vmiName := types.NamespacedName{
		Name:      ci.Status.VirtualMachineReference.KubeVirtVirtualMachineName,
		Namespace: ci.Status.VirtualMachineReference.Namespace,
	}

	if err := r.Get(ctx, vmiName, vmi); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("VirtualMachineInstance not found, VM may not be running")
			// Mark as restarted even though VMI doesn't exist
			ci.Status.LastRestartedAt = ci.Spec.RestartRequestedAt.DeepCopy()
			// Status will be updated by the main Reconcile loop
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get VirtualMachineInstance")
		return ctrl.Result{}, err
	}

	// Delete VMI to trigger restart (KubeVirt will recreate it)
	log.Info("Deleting VirtualMachineInstance to trigger restart", "vmi", vmiName.String())
	if err := r.Delete(ctx, vmi); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete VirtualMachineInstance")
			meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
				Type:               string(v1alpha1.ComputeInstanceConditionRestartFailed),
				Status:             metav1.ConditionTrue,
				Reason:             ReasonVMIDeletionFailed,
				Message:            fmt.Sprintf("Failed to delete VMI: %v", err),
				ObservedGeneration: ci.Generation,
			})
			// Status will be updated by the main Reconcile loop before returning error
			return ctrl.Result{}, err
		}
	}

	// Update status: mark restart as in progress and record when initiated
	meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
		Type:               string(v1alpha1.ComputeInstanceConditionRestartInProgress),
		Status:             metav1.ConditionTrue,
		Reason:             ReasonRestartInProgress,
		Message:            fmt.Sprintf("Restart initiated at %s", ci.Spec.RestartRequestedAt.Time.Format(time.RFC3339)),
		ObservedGeneration: ci.Generation,
	})

	// Record when restart was initiated
	ci.Status.LastRestartedAt = ci.Spec.RestartRequestedAt.DeepCopy()

	// Clear any previous failure conditions
	meta.RemoveStatusCondition(&ci.Status.Conditions,
		string(v1alpha1.ComputeInstanceConditionRestartFailed))

	// Status will be updated by the main Reconcile loop
	log.Info("Restart initiated successfully",
		"restartRequestedAt", ci.Spec.RestartRequestedAt.Time.Format(time.RFC3339))
	return ctrl.Result{}, nil
}

// clearRestartConditions removes restart-related conditions when no restart is in progress.
// Returns true if any conditions were removed.
func (r *ComputeInstanceReconciler) clearRestartConditions(ctx context.Context, ci *v1alpha1.ComputeInstance) bool {
	changed := meta.RemoveStatusCondition(&ci.Status.Conditions,
		string(v1alpha1.ComputeInstanceConditionRestartInProgress))

	if meta.RemoveStatusCondition(&ci.Status.Conditions,
		string(v1alpha1.ComputeInstanceConditionRestartFailed)) {
		changed = true
	}

	return changed
}
