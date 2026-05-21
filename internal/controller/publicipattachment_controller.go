/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/pkg/provisioning"
)

const (
	osacPublicIPAttachmentFinalizer = "osac.openshift.io/publicipattachment-finalizer"
)

// PublicIPAttachmentReconciler reconciles PublicIPAttachment CRs.
//
// Creating a PublicIPAttachment triggers an attach operation (osac-attach-public-ip AAP
// template) that moves the MetalLB Service from the parking namespace to the VM namespace.
// Deleting the CR triggers detach (osac-detach-public-ip) which reverses that.
//
// The controller uses RunProvisioningLifecycle for provisioning, giving automatic
// exponential-backoff retry on failure. It also watches ComputeInstance resources to
// auto-delete the PublicIPAttachment when the target CI is deleted.
type PublicIPAttachmentReconciler struct {
	client.Client
	APIReader                client.Reader
	Scheme                   *runtime.Scheme
	mgr                      mcmanager.Manager
	NetworkingNamespace      string
	ComputeInstanceNamespace string
	ProvisioningProvider     provisioning.ProvisioningProvider
	StatusPollInterval       time.Duration
	MaxJobHistory            int
	targetCluster            mc.ClusterName
}

// NewPublicIPAttachmentReconciler creates a new reconciler for PublicIPAttachment resources.
func NewPublicIPAttachmentReconciler(
	mgr mcmanager.Manager,
	networkingNamespace string,
	computeInstanceNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
	targetCluster mc.ClusterName,
) *PublicIPAttachmentReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}
	if statusPollInterval <= 0 {
		statusPollInterval = provisioning.DefaultStatusPollInterval
	}
	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}
	if computeInstanceNamespace == "" {
		computeInstanceNamespace = defaultComputeInstanceNamespace
	}
	return &PublicIPAttachmentReconciler{
		Client:                   mgr.GetLocalManager().GetClient(),
		APIReader:                mgr.GetLocalManager().GetAPIReader(),
		Scheme:                   mgr.GetLocalManager().GetScheme(),
		mgr:                      mgr,
		NetworkingNamespace:      networkingNamespace,
		ComputeInstanceNamespace: computeInstanceNamespace,
		ProvisioningProvider:     provisioningProvider,
		StatusPollInterval:       statusPollInterval,
		MaxJobHistory:            maxJobHistory,
		targetCluster:            targetCluster,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicipattachments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicipattachments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=publicipattachments/finalizers,verbs=update

// Reconcile handles create/update/delete for a PublicIPAttachment CR.
func (r *PublicIPAttachmentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	attachment := &v1alpha1.PublicIPAttachment{}
	if err := r.Get(ctx, req.NamespacedName, attachment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := attachment.Annotations[osacManagementStateAnnotation]
	if attachment.ObjectMeta.DeletionTimestamp.IsZero() && exists && val == ManagementStateUnmanaged {
		log.Info("ignoring PublicIPAttachment due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile", "publicIP", attachment.Spec.PublicIP, "phase", attachment.Status.Phase)

	oldstatus := attachment.Status.DeepCopy()

	var res ctrl.Result
	var err error
	if attachment.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, attachment)
	} else {
		res, err = r.handleDelete(ctx, attachment)
	}

	if !equality.Semantic.DeepEqual(attachment.Status, *oldstatus) {
		log.Info("status requires update", "phase", attachment.Status.Phase)
		if updateErr := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(attachment), attachment.Status); updateErr != nil {
			return res, updateErr
		}
	}

	log.Info("end reconcile", "phase", attachment.Status.Phase)
	return res, err
}

func (r *PublicIPAttachmentReconciler) handleUpdate(ctx context.Context, attachment *v1alpha1.PublicIPAttachment) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if controllerutil.AddFinalizer(attachment, osacPublicIPAttachmentFinalizer) {
		log.Info("adding finalizer")
		if err := r.Update(ctx, attachment); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(attachment), attachment); err != nil {
			return ctrl.Result{}, err
		}
	}

	if attachment.Status.Phase == "" {
		attachment.Status.Phase = v1alpha1.PublicIPAttachmentPhaseProgressing
	}

	// Resolve parent PublicIP by name
	publicIP := &v1alpha1.PublicIP{}
	if err := r.Get(ctx, types.NamespacedName{Name: attachment.Spec.PublicIP, Namespace: attachment.Namespace}, publicIP); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("parent PublicIP not found, requeueing", "publicIP", attachment.Spec.PublicIP)
			return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
		}
		return ctrl.Result{}, err
	}

	// Resolve parent PublicIPPool by UUID label
	poolList := &v1alpha1.PublicIPPoolList{}
	if err := r.List(ctx, poolList,
		client.InNamespace(attachment.Namespace),
		client.MatchingLabels{osacPublicIPPoolIDLabel: publicIP.Spec.Pool},
	); err != nil {
		return ctrl.Result{}, err
	}
	if len(poolList.Items) == 0 {
		log.Info("parent PublicIPPool not found, requeueing", "poolUUID", publicIP.Spec.Pool)
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}
	pool := &poolList.Items[0]

	implementationStrategy := pool.Spec.ImplementationStrategy
	if implementationStrategy == "" {
		implementationStrategy = defaultPublicIPPoolImplementationStrategy
	}

	// Resolve target ComputeInstance
	var ci *v1alpha1.ComputeInstance
	if attachment.Spec.ComputeInstance != nil {
		ci = &v1alpha1.ComputeInstance{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      *attachment.Spec.ComputeInstance,
			Namespace: r.ComputeInstanceNamespace,
		}, ci); err != nil {
			if client.IgnoreNotFound(err) == nil {
				log.Info("ComputeInstance not found, requeueing", "computeInstance", *attachment.Spec.ComputeInstance)
				return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
			}
			return ctrl.Result{}, err
		}

		// Auto-detach: if CI is being deleted, delete this PublicIPAttachment
		if !ci.DeletionTimestamp.IsZero() {
			log.Info("auto-detaching: ComputeInstance is being deleted",
				"computeInstance", ci.Name)
			if err := r.Delete(ctx, attachment); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{}, nil
		}

		if ci.Status.VirtualMachineReference == nil {
			log.Info("ComputeInstance has no VirtualMachineReference yet, requeueing",
				"computeInstance", ci.Name)
			return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
		}

		// Add CI detach finalizer to prevent CI deletion before detach completes
		if controllerutil.AddFinalizer(ci, osacPublicIPDetachFinalizer) {
			log.Info("adding publicip-detach finalizer to ComputeInstance",
				"computeInstance", ci.Name)
			if err := r.Update(ctx, ci); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Sync annotations
	needsUpdate := false
	if attachment.Annotations == nil {
		attachment.Annotations = make(map[string]string)
	}

	if attachment.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		attachment.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		needsUpdate = true
	}
	if attachment.Annotations[osacPublicIPPoolNameAnnotation] != pool.Name {
		attachment.Annotations[osacPublicIPPoolNameAnnotation] = pool.Name
		needsUpdate = true
	}
	if ci != nil && ci.Status.VirtualMachineReference != nil {
		targetNamespace := ci.Status.VirtualMachineReference.Namespace
		if attachment.Annotations[osacPublicIPTargetNamespaceAnnotation] != targetNamespace {
			attachment.Annotations[osacPublicIPTargetNamespaceAnnotation] = targetNamespace
			needsUpdate = true
		}
	}

	if needsUpdate {
		if err := r.Update(ctx, attachment); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(attachment), attachment); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Compute desired config version
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		Spec                   v1alpha1.PublicIPAttachmentSpec
		ImplementationStrategy string
	}{attachment.Spec, implementationStrategy})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	attachment.Status.DesiredConfigVersion = desiredVersion

	v1alpha1.SetPublicIPAttachmentStatusCondition(attachment, metav1.Condition{
		Type:               string(v1alpha1.PublicIPAttachmentConditionConfigurationApplied),
		Status:             metav1.ConditionTrue,
		Reason:             "ConfigurationApplied",
		Message:            "Controller has processed the current spec",
		LastTransitionTime: metav1.Now(),
	})

	if attachment.Status.Phase == "" || (attachment.Status.Phase == v1alpha1.PublicIPAttachmentPhaseReady &&
		!provisioning.IsConfigApplied(&attachment.Status.Jobs, attachment.Status.DesiredConfigVersion)) {
		attachment.Status.Phase = v1alpha1.PublicIPAttachmentPhaseProgressing
	}

	return r.handleProvisioning(ctx, attachment, publicIP, ci)
}

func (r *PublicIPAttachmentReconciler) handleProvisioning(
	ctx context.Context,
	attachment *v1alpha1.PublicIPAttachment,
	publicIP *v1alpha1.PublicIP,
	ci *v1alpha1.ComputeInstance,
) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, attachment,
		&provisioning.State{Jobs: &attachment.Status.Jobs, DesiredConfigVersion: attachment.Status.DesiredConfigVersion},
		r.MaxJobHistory, r.StatusPollInterval,
		&provisioning.PollCallbacks{
			OnFailed: func(_ string) { attachment.Status.Phase = v1alpha1.PublicIPAttachmentPhaseFailed },
			OnSuccess: func(_ provisioning.ProvisionStatus) {
				attachment.Status.Phase = v1alpha1.PublicIPAttachmentPhaseReady
				r.onProvisionSuccess(ctx, publicIP, ci)
			},
		},
		func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJob(
				ctx, r.APIReader, client.ObjectKeyFromObject(attachment), &v1alpha1.PublicIPAttachment{})
		},
		func() error {
			return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(attachment), attachment.Status)
		},
	)
}

// onProvisionSuccess updates the parent PublicIP and target ComputeInstance after
// a successful attach operation.
func (r *PublicIPAttachmentReconciler) onProvisionSuccess(ctx context.Context, publicIP *v1alpha1.PublicIP, ci *v1alpha1.ComputeInstance) {
	log := ctrllog.FromContext(ctx)

	// Set PublicIP.status.attached = true
	if !publicIP.Status.Attached {
		fresh := &v1alpha1.PublicIP{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(publicIP), fresh); err != nil {
			log.Error(err, "failed to fetch PublicIP for attached update")
			return
		}
		fresh.Status.Attached = true
		if err := r.Status().Update(ctx, fresh); err != nil {
			log.Error(err, "failed to set PublicIP status.attached=true")
		}
	}

	// Set ComputeInstance.status.publicIPAddress from the parent PublicIP's address
	if ci != nil && publicIP.Status.Address != "" {
		fresh := &v1alpha1.ComputeInstance{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(ci), fresh); err != nil {
			log.Error(err, "failed to fetch ComputeInstance for publicIPAddress update")
			return
		}
		if fresh.GetPublicIPAddress() != publicIP.Status.Address {
			fresh.SetPublicIPAddress(publicIP.Status.Address)
			if err := r.Status().Update(ctx, fresh); err != nil {
				log.Error(err, "failed to set ComputeInstance publicIPAddress")
			}
		}
	}
}

func (r *PublicIPAttachmentReconciler) handleDelete(ctx context.Context, attachment *v1alpha1.PublicIPAttachment) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting PublicIPAttachment")

	attachment.Status.Phase = v1alpha1.PublicIPAttachmentPhaseDeleting

	if !controllerutil.ContainsFinalizer(attachment, osacPublicIPAttachmentFinalizer) {
		return ctrl.Result{}, nil
	}

	result, err := r.handleDeprovisioning(ctx, attachment)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	// Deprovisioning complete: update parent resources and remove finalizers
	r.onDeprovisionSuccess(ctx, attachment)

	log.Info("removing finalizer after successful deprovisioning")
	controllerutil.RemoveFinalizer(attachment, osacPublicIPAttachmentFinalizer)
	if err := r.Update(ctx, attachment); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// onDeprovisionSuccess clears the attached state on the parent PublicIP, clears
// publicIPAddress on the ComputeInstance, and removes the CI detach finalizer when
// no other PublicIPAttachments reference the same CI.
func (r *PublicIPAttachmentReconciler) onDeprovisionSuccess(ctx context.Context, attachment *v1alpha1.PublicIPAttachment) {
	log := ctrllog.FromContext(ctx)

	// Clear PublicIP.status.attached
	publicIP := &v1alpha1.PublicIP{}
	if err := r.Get(ctx, types.NamespacedName{Name: attachment.Spec.PublicIP, Namespace: attachment.Namespace}, publicIP); err != nil {
		log.Error(err, "failed to fetch PublicIP for attached=false update")
	} else if publicIP.Status.Attached {
		publicIP.Status.Attached = false
		if err := r.Status().Update(ctx, publicIP); err != nil {
			log.Error(err, "failed to clear PublicIP status.attached")
		}
	}

	// Clear ComputeInstance.status.publicIPAddress and remove CI detach finalizer
	if attachment.Spec.ComputeInstance != nil {
		ciName := *attachment.Spec.ComputeInstance
		ci := &v1alpha1.ComputeInstance{}
		if err := r.Get(ctx, types.NamespacedName{Name: ciName, Namespace: r.ComputeInstanceNamespace}, ci); err != nil {
			if client.IgnoreNotFound(err) != nil {
				log.Error(err, "failed to fetch ComputeInstance for cleanup")
			}
		} else {
			if ci.GetPublicIPAddress() != "" {
				ci.SetPublicIPAddress("")
				if err := r.Status().Update(ctx, ci); err != nil {
					log.Error(err, "failed to clear ComputeInstance publicIPAddress")
				}
			}
		}

		if err := r.maybeRemoveCIDetachFinalizer(ctx, ciName, attachment.Name); err != nil {
			log.Error(err, "failed to remove CI detach finalizer")
		}
	}
}

// maybeRemoveCIDetachFinalizer removes the publicip-detach finalizer from the
// ComputeInstance if no other PublicIPAttachments (and no PublicIPs) still reference it.
func (r *PublicIPAttachmentReconciler) maybeRemoveCIDetachFinalizer(ctx context.Context, ciName string, excludeAttachment string) error {
	log := ctrllog.FromContext(ctx)

	ci := &v1alpha1.ComputeInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: ciName, Namespace: r.ComputeInstanceNamespace}, ci); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(ci, osacPublicIPDetachFinalizer) {
		return nil
	}

	// Check if other PublicIPAttachments still reference this CI
	attachments := &v1alpha1.PublicIPAttachmentList{}
	if err := r.List(ctx, attachments, client.InNamespace(r.NetworkingNamespace)); err != nil {
		return err
	}
	for i := range attachments.Items {
		if attachments.Items[i].Name == excludeAttachment {
			continue
		}
		if attachments.Items[i].Spec.ComputeInstance != nil && *attachments.Items[i].Spec.ComputeInstance == ciName {
			log.Info("other PublicIPAttachments still reference CI, keeping finalizer",
				"computeInstance", ciName,
				"attachment", attachments.Items[i].Name)
			return nil
		}
	}

	// Also check PublicIPs that reference this CI by UUID (shared finalizer)
	ciUUID, hasUUID := ci.Labels[osacComputeInstanceIDLabel]
	if hasUUID {
		publicIPs := &v1alpha1.PublicIPList{}
		if err := r.List(ctx, publicIPs, client.InNamespace(r.NetworkingNamespace)); err != nil {
			return err
		}
		for i := range publicIPs.Items {
			if publicIPs.Items[i].Spec.ComputeInstance == ciUUID {
				log.Info("PublicIP still references CI, keeping finalizer",
					"computeInstance", ciName,
					"publicIP", publicIPs.Items[i].Name)
				return nil
			}
		}
	}

	log.Info("no more references, removing CI detach finalizer", "computeInstance", ciName)
	if controllerutil.RemoveFinalizer(ci, osacPublicIPDetachFinalizer) {
		if err := r.Update(ctx, ci); err != nil {
			return err
		}
	}
	return nil
}

func (r *PublicIPAttachmentReconciler) handleDeprovisioning(ctx context.Context, attachment *v1alpha1.PublicIPAttachment) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	if r.ProvisioningProvider == nil {
		log.Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	latestDeprovisionJob := provisioning.FindLatestJobByType(attachment.Status.Jobs, v1alpha1.JobTypeDeprovision)

	if latestDeprovisionJob == nil || latestDeprovisionJob.JobID == "" {
		log.Info("triggering deprovisioning", "provider", r.ProvisioningProvider.Name())

		result, err := r.ProvisioningProvider.TriggerDeprovision(ctx, attachment)
		if err != nil {
			log.Error(err, "failed to trigger deprovisioning")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}

		switch result.Action {
		case provisioning.DeprovisionWaiting:
			log.Info("deprovisioning not ready, requeueing")
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil

		case provisioning.DeprovisionSkipped:
			log.Info("provider skipped deprovisioning")
			return ctrl.Result{}, nil

		case provisioning.DeprovisionTriggered:
			newJob := v1alpha1.JobStatus{
				JobID:                  result.JobID,
				Type:                   v1alpha1.JobTypeDeprovision,
				Timestamp:              metav1.NewTime(time.Now().UTC()),
				State:                  v1alpha1.JobStatePending,
				Message:                deprovisioningJobTriggeredMessage,
				BlockDeletionOnFailure: result.BlockDeletionOnFailure,
			}
			attachment.Status.Jobs = provisioning.AppendJob(attachment.Status.Jobs, newJob, r.MaxJobHistory)
			log.Info("deprovisioning job triggered", "jobID", result.JobID)
			return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
		}
	}

	status, err := r.ProvisioningProvider.GetDeprovisionStatus(ctx, attachment, latestDeprovisionJob.JobID)
	if err != nil {
		log.Error(err, "failed to get deprovision job status", "jobID", latestDeprovisionJob.JobID)
		updatedJob := *latestDeprovisionJob
		updatedJob.Message = fmt.Sprintf("Failed to get job status: %v", err)
		provisioning.UpdateJob(attachment.Status.Jobs, updatedJob)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	updatedJob := *latestDeprovisionJob
	updatedJob.State = status.State
	updatedJob.Message = status.MessageWithDetails()
	provisioning.UpdateJob(attachment.Status.Jobs, updatedJob)

	if !status.State.IsTerminal() {
		log.Info("deprovision job still running", "jobID", latestDeprovisionJob.JobID, "state", status.State)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	if status.State.IsSuccessful() {
		log.Info("deprovision job succeeded", "jobID", latestDeprovisionJob.JobID)
		return ctrl.Result{}, nil
	}

	if latestDeprovisionJob.BlockDeletionOnFailure {
		log.Info("deprovision job failed, blocking deletion to prevent orphaned resources",
			"jobID", latestDeprovisionJob.JobID,
			"state", status.State,
			"message", updatedJob.Message)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
	}

	log.Info("deprovision job did not succeed, allowing process to continue",
		"jobID", latestDeprovisionJob.JobID,
		"state", status.State,
		"message", updatedJob.Message)
	return ctrl.Result{}, nil
}

func (r *PublicIPAttachmentReconciler) updateStatusWithRetry(ctx context.Context, key client.ObjectKey, newStatus v1alpha1.PublicIPAttachmentStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.PublicIPAttachment{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		latest.Status = newStatus
		return r.Status().Update(ctx, latest)
	})
}

// mapComputeInstanceToPublicIPAttachments maps a ComputeInstance change to all
// PublicIPAttachments that reference it, so the controller can react to CI deletion
// or VirtualMachineReference changes.
func (r *PublicIPAttachmentReconciler) mapComputeInstanceToPublicIPAttachments(ctx context.Context, obj client.Object) []reconcile.Request {
	log := ctrllog.FromContext(ctx)

	ciName := obj.GetName()

	attachments := &v1alpha1.PublicIPAttachmentList{}
	if err := r.List(ctx, attachments, client.InNamespace(r.NetworkingNamespace)); err != nil {
		log.Error(err, "failed to list PublicIPAttachments for ComputeInstance watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range attachments.Items {
		if attachments.Items[i].Spec.ComputeInstance != nil && *attachments.Items[i].Spec.ComputeInstance == ciName {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&attachments.Items[i]),
			})
		}
	}

	if len(requests) > 0 {
		log.Info("mapped ComputeInstance change to PublicIPAttachments",
			"computeInstance", ciName,
			"attachmentCount", len(requests),
		)
	}

	return requests
}

// SetupWithManager registers this controller with the multicluster manager.
func (r *PublicIPAttachmentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.PublicIPAttachment{},
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(
			&v1alpha1.ComputeInstance{},
			mchandler.EnqueueRequestsFromMapFunc(r.mapComputeInstanceToPublicIPAttachments),
			mcbuilder.WithPredicates(ComputeInstanceNamespacePredicate(r.ComputeInstanceNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false),
		).
		Complete(r)
}
