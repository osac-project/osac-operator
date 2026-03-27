/*
Copyright 2026.

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
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

// BareMetalPoolReconciler reconciles a BareMetalPool object
type BareMetalPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const BareMetalPoolFinalizer = "osac.openshift.io/bare-metal-pool"

// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hosts,verbs=get;list;watch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the pool closer to the desired state.
func (r *BareMetalPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	bareMetalPool := &v1alpha1.BareMetalPool{}
	err := r.Get(ctx, req.NamespacedName, bareMetalPool)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info(
		"Starting reconcile",
		"namespacedName",
		req.NamespacedName,
		"name",
		bareMetalPool.Name,
	)

	oldstatus := bareMetalPool.Status.DeepCopy()

	var result ctrl.Result
	if !bareMetalPool.DeletionTimestamp.IsZero() {
		err = r.handleDeletion(ctx, bareMetalPool)
		result = ctrl.Result{}
	} else {
		result, err = r.handleUpdate(ctx, bareMetalPool)
	}

	if !equality.Semantic.DeepEqual(bareMetalPool.Status, *oldstatus) {
		statusErr := r.Status().Update(ctx, bareMetalPool)
		if statusErr != nil {
			return result, statusErr
		}
	}

	return result, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *BareMetalPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.BareMetalPool{}).
		Named("baremetalpool").
		Complete(r)
}

// handleUpdate processes BareMetalPool creation or specification updates.
func (r *BareMetalPoolReconciler) handleUpdate(ctx context.Context, bareMetalPool *v1alpha1.BareMetalPool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Handling update", "name", bareMetalPool.Name)

	bareMetalPool.InitializeStatusConditions()

	if controllerutil.AddFinalizer(bareMetalPool, BareMetalPoolFinalizer) {
		if err := r.Update(ctx, bareMetalPool); err != nil {
			log.Error(err, "Failed to add finalizer", "name", bareMetalPool.Name)
			bareMetalPool.SetStatusCondition(
				v1alpha1.BareMetalPoolConditionTypeReady,
				metav1.ConditionFalse,
				"Failed to add finalizer",
				v1alpha1.BareMetalPoolReasonFailed,
			)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if bareMetalPool.Status.HostSets == nil {
		bareMetalPool.Status.HostSets = []v1alpha1.BareMetalHostSet{}
	}

	// List all Host CRs owned by this BareMetalPool
	hostList := &v1alpha1.HostList{}
	err := r.List(ctx, hostList,
		client.InNamespace(bareMetalPool.Namespace),
		client.MatchingLabels{"osac.openshift.io/pool-id": string(bareMetalPool.UID)},
	)
	if err != nil {
		log.Error(err, "Failed to list Host CRs")
		bareMetalPool.SetStatusCondition(
			v1alpha1.BareMetalPoolConditionTypeReady,
			metav1.ConditionFalse,
			"Failed to list Host CRs",
			v1alpha1.BareMetalPoolReasonFailed,
		)
		return ctrl.Result{}, err
	}

	// Group current hosts per hostClass, sorted by name for deterministic scale-down
	currentHosts := map[string][]*v1alpha1.Host{}
	for i := range hostList.Items {
		hostClass := hostList.Items[i].Spec.HostClass
		currentHosts[hostClass] = append(currentHosts[hostClass], &hostList.Items[i])
	}
	for hostClass := range currentHosts {
		sort.Slice(currentHosts[hostClass], func(i, j int) bool {
			return currentHosts[hostClass][i].Name < currentHosts[hostClass][j].Name
		})
	}

	// Build a map of desired replicas for easier lookup
	desiredReplicas := map[string]int32{}
	for _, hostSet := range bareMetalPool.Spec.HostSets {
		desiredReplicas[hostSet.HostClass] = hostSet.Replicas
	}

	// Scale up or down for each desired hostClass
	defer r.updateStatusHostSets(bareMetalPool, currentHosts)
	for hostClass, replicas := range desiredReplicas {
		delta := replicas - int32(len(currentHosts[hostClass]))
		if delta > 0 {
			for range delta {
				if err := r.createHostCR(ctx, bareMetalPool, hostClass); err != nil {
					log.Error(err, "Failed to create Host CR")
					bareMetalPool.SetStatusCondition(
						v1alpha1.BareMetalPoolConditionTypeReady,
						metav1.ConditionFalse,
						"Failed to create Host CR",
						v1alpha1.BareMetalPoolReasonFailed,
					)
					return ctrl.Result{}, err
				}
				currentHosts[hostClass] = append(currentHosts[hostClass], nil)
			}
		} else if delta < 0 {
			for int32(len(currentHosts[hostClass])) > replicas {
				lastIdx := len(currentHosts[hostClass]) - 1
				hostToDelete := currentHosts[hostClass][lastIdx]
				if err := r.Delete(ctx, hostToDelete); client.IgnoreNotFound(err) != nil {
					log.Error(err, "Failed to delete Host CR", "host", hostToDelete.Name)
					bareMetalPool.SetStatusCondition(
						v1alpha1.BareMetalPoolConditionTypeReady,
						metav1.ConditionFalse,
						"Failed to delete Host CR",
						v1alpha1.BareMetalPoolReasonFailed,
					)
					return ctrl.Result{}, err
				}
				currentHosts[hostClass] = currentHosts[hostClass][:lastIdx]
			}
		}
	}

	// Delete hosts for hostClasses no longer in spec
	for hostClass := range currentHosts {
		if _, ok := desiredReplicas[hostClass]; ok {
			continue
		}
		for len(currentHosts[hostClass]) > 0 {
			lastIdx := len(currentHosts[hostClass]) - 1
			hostToDelete := currentHosts[hostClass][lastIdx]
			if err := r.Delete(ctx, hostToDelete); client.IgnoreNotFound(err) != nil {
				log.Error(err, "Failed to delete Host CR", "host", hostToDelete.Name)
				bareMetalPool.SetStatusCondition(
					v1alpha1.BareMetalPoolConditionTypeReady,
					metav1.ConditionFalse,
					"Failed to delete Host CR",
					v1alpha1.BareMetalPoolReasonFailed,
				)
				return ctrl.Result{}, err
			}
			currentHosts[hostClass] = currentHosts[hostClass][:lastIdx]
		}
		delete(currentHosts, hostClass)
	}

	// TODO: add profile (setup) logic

	bareMetalPool.SetStatusCondition(
		v1alpha1.BareMetalPoolConditionTypeReady,
		metav1.ConditionTrue,
		"Successfully reconciled hosts",
		v1alpha1.BareMetalPoolReasonReady,
	)

	log.Info("Successfully reconciled")
	return ctrl.Result{}, nil
}

// handleDeletion handles the cleanup when a BareMetalPool is being deleted
func (r *BareMetalPoolReconciler) handleDeletion(ctx context.Context, bareMetalPool *v1alpha1.BareMetalPool) error {
	log := logf.FromContext(ctx)
	log.Info("Handling delete", "name", bareMetalPool.Name)

	bareMetalPool.SetStatusCondition(
		v1alpha1.BareMetalPoolConditionTypeReady,
		metav1.ConditionFalse,
		"BareMetalPool is being torn down",
		v1alpha1.BareMetalPoolReasonDeleting,
	)

	// TODO: add profile (teardown) logic

	// Owner references will cascade-delete the Host CRs.
	// The host-inventory-operator's finalizer on each Host CR ensures
	// inventory hosts are freed before the Host CRs are fully removed.

	if controllerutil.RemoveFinalizer(bareMetalPool, BareMetalPoolFinalizer) {
		err := r.Update(ctx, bareMetalPool)
		return err
	}

	log.Info("Successfully deleted pool")
	return nil
}

// createHostCR creates a new Host CR owned by this BareMetalPool
func (r *BareMetalPoolReconciler) createHostCR(
	ctx context.Context,
	bareMetalPool *v1alpha1.BareMetalPool,
	hostClass string,
) error {
	log := logf.FromContext(ctx)

	hostName := fmt.Sprintf("%s-host-%s-%s", bareMetalPool.Name, hostClass, rand.String(5))
	namespace := bareMetalPool.Namespace
	labels := map[string]string{
		"osac.openshift.io/pool-id":    string(bareMetalPool.UID),
		"osac.openshift.io/host-class": hostClass,
	}

	selector := v1alpha1.HostSelectorSpec{
		HostSelector: map[string]string{ // TODO: add selectors from profile
			"managedBy":      "baremetal",
			"provisionState": "available",
		},
	}
	templateID := "default"
	templateParameters := ""
	if bareMetalPool.Spec.Profile != nil {
		if bareMetalPool.Spec.Profile.Name != "" {
			templateID = "other_tempate" // TODO: get template id from profile
		}
		templateParameters = bareMetalPool.Spec.Profile.TemplateParameters
	}

	hostCR := &v1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1alpha1.HostSpec{
			HostClass:          hostClass,
			ID:                 "",
			Name:               "",
			Selector:           selector,
			TemplateID:         templateID,
			TemplateParameters: templateParameters,
			Online:             false,
		},
	}
	if err := controllerutil.SetControllerReference(bareMetalPool, hostCR, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference", "host", hostName)
		return err
	}
	if err := r.Create(ctx, hostCR); client.IgnoreAlreadyExists(err) != nil {
		log.Error(err, "Failed to create Host CR", "host", hostName)
		return err
	}

	log.Info("Created Host CR", "host", hostName)
	return nil
}

// updateStatusHostSets updates status.HostSets from the current hosts map.
func (r *BareMetalPoolReconciler) updateStatusHostSets(bareMetalPool *v1alpha1.BareMetalPool, currentHosts map[string][]*v1alpha1.Host) {
	updatedHostSets := []v1alpha1.BareMetalHostSet{}
	for hostClass, hosts := range currentHosts {
		if len(hosts) > 0 {
			updatedHostSets = append(updatedHostSets, v1alpha1.BareMetalHostSet{
				HostClass: hostClass,
				Replicas:  int32(len(hosts)),
			})
		}
	}
	bareMetalPool.Status.HostSets = updatedHostSets
}
