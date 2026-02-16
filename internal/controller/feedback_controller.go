/*
Copyright (c) 2025 Red Hat Inc.

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

	"github.com/go-logr/logr"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"

	ckv1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac/osac-operator/internal/api/private/v1"
	sharedv1 "github.com/osac/osac-operator/internal/api/shared/v1"
)

// FeedbackReconciler sends updates to the fulfillment service.
type FeedbackReconciler struct {
	logger                logr.Logger
	hubClient             clnt.Client
	clustersClient        privatev1.ClustersClient
	clusterOrderNamespace string
}

// feedbackReconcilerTask contains data that is used for the reconciliation of a specific cluster order, so there is less
// need to pass around as function parameters that and other related objects.
type feedbackReconcilerTask struct {
	r             *FeedbackReconciler
	object        *ckv1alpha1.ClusterOrder
	cluster       *privatev1.Cluster
	hostedCluster *hypershiftv1beta1.HostedCluster
}

// NewFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about cluster orders.
func NewFeedbackReconciler(logger logr.Logger, hubClient clnt.Client, grpcConn *grpc.ClientConn, clusterOrderNamespace string) *FeedbackReconciler {
	return &FeedbackReconciler{
		logger:                logger,
		hubClient:             hubClient,
		clustersClient:        privatev1.NewClustersClient(grpcConn),
		clusterOrderNamespace: clusterOrderNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *FeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("clusterorder-feedback").
		For(&ckv1alpha1.ClusterOrder{}, builder.WithPredicates(NamespacePredicate(r.clusterOrderNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *FeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	// Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &ckv1alpha1.ClusterOrder{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Get the identifier of the cluster from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	clusterID, ok := object.Labels[cloudkitClusterOrderIDLabel]
	if !ok {
		r.logger.Info(
			"There is no label containing the cluster identifier, will ignore it",
			"label", cloudkitClusterOrderIDLabel,
		)
		return
	}

	// Fetch the cluster:
	cluster, err := r.fetchCluster(ctx, clusterID)
	if err != nil {
		return
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &feedbackReconcilerTask{
		r:       r,
		object:  object,
		cluster: clone(cluster),
	}
	if object.ObjectMeta.DeletionTimestamp.IsZero() {
		result, err = t.handleUpdate(ctx)
	} else {
		result, err = t.handleDelete(ctx)
	}
	if err != nil {
		return
	}

	// Save the objects that have changed:
	err = r.saveCluster(ctx, cluster, t.cluster)
	if err != nil {
		return
	}
	return
}

func (r *FeedbackReconciler) fetchCluster(ctx context.Context, id string) (cluster *privatev1.Cluster, err error) {
	response, err := r.clustersClient.Get(ctx, privatev1.ClustersGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	cluster = response.GetObject()
	if !cluster.HasSpec() {
		cluster.SetSpec(&privatev1.ClusterSpec{})
	}
	if !cluster.HasStatus() {
		cluster.SetStatus(&privatev1.ClusterStatus{})
	}
	return
}

func (r *FeedbackReconciler) saveCluster(ctx context.Context, before, after *privatev1.Cluster) error {
	if !equal(after, before) {
		_, err := r.clustersClient.Update(ctx, privatev1.ClustersUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *feedbackReconcilerTask) handleUpdate(ctx context.Context) (result ctrl.Result, err error) {
	err = t.syncConditions()
	if err != nil {
		return
	}
	err = t.syncPhase(ctx)
	if err != nil {
		return
	}
	err = t.syncNodeRequests()
	if err != nil {
		return
	}
	return
}

func (t *feedbackReconcilerTask) syncConditions() error {
	for _, condition := range t.object.Status.Conditions {
		err := t.syncCondition(condition)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *feedbackReconcilerTask) syncCondition(condition metav1.Condition) error {
	switch ckv1alpha1.ClusterOrderConditionType(condition.Type) {
	case ckv1alpha1.ClusterOrderConditionAccepted:
		return t.syncConditionAccepted(condition)
	case ckv1alpha1.ClusterOrderConditionProgressing:
		return t.syncConditionProgressing(condition)
	case ckv1alpha1.ClusterOrderConditionControlPlaneAvailable:
		return t.syncConditionControlPlaneAvailable(condition)
	case ckv1alpha1.ClusterOrderConditionAvailable:
		return t.syncConditionAvailable(condition)
	default:
		t.r.logger.Info(
			"Unknown condition, will ignore it",
			"condition", condition.Type,
		)
	}
	return nil
}

func (t *feedbackReconcilerTask) syncConditionAccepted(condition metav1.Condition) error {
	orderCondition := t.findClusterCondition(privatev1.ClusterConditionType_CLUSTER_CONDITION_TYPE_PROGRESSING)
	oldStatus := orderCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	orderCondition.SetStatus(newStatus)
	orderCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		orderCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *feedbackReconcilerTask) syncConditionProgressing(condition metav1.Condition) error {
	orderCondition := t.findClusterCondition(privatev1.ClusterConditionType_CLUSTER_CONDITION_TYPE_PROGRESSING)
	oldStatus := orderCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	orderCondition.SetStatus(newStatus)
	orderCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		orderCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *feedbackReconcilerTask) syncConditionControlPlaneAvailable(condition metav1.Condition) error {
	orderCondition := t.findClusterCondition(privatev1.ClusterConditionType_CLUSTER_CONDITION_TYPE_PROGRESSING)
	oldStatus := orderCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	orderCondition.SetStatus(newStatus)
	orderCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		orderCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *feedbackReconcilerTask) syncConditionAvailable(condition metav1.Condition) error {
	clusterCondition := t.findClusterCondition(privatev1.ClusterConditionType_CLUSTER_CONDITION_TYPE_PROGRESSING)
	oldStatus := clusterCondition.GetStatus()
	newStatus := t.mapConditionStatus(condition.Status)
	clusterCondition.SetStatus(newStatus)
	clusterCondition.SetMessage(condition.Message)
	if newStatus != oldStatus {
		clusterCondition.SetLastTransitionTime(timestamppb.Now())
	}
	return nil
}

func (t *feedbackReconcilerTask) mapConditionStatus(status metav1.ConditionStatus) sharedv1.ConditionStatus {
	switch status {
	case metav1.ConditionFalse:
		return sharedv1.ConditionStatus_CONDITION_STATUS_FALSE
	case metav1.ConditionTrue:
		return sharedv1.ConditionStatus_CONDITION_STATUS_TRUE
	default:
		return sharedv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

func (t *feedbackReconcilerTask) syncPhase(ctx context.Context) error {
	switch t.object.Status.Phase {
	case ckv1alpha1.ClusterOrderPhaseProgressing:
		return t.syncPhaseProgressing()
	case ckv1alpha1.ClusterOrderPhaseFailed:
		return t.syncPhaseFailed()
	case ckv1alpha1.ClusterOrderPhaseReady:
		return t.syncPhaseReady(ctx)
	case ckv1alpha1.ClusterOrderPhaseDeleting:
		// TODO: There is no equivalent phase.
		// return t.syncPhaseDeleting(ctx)
	default:
		t.r.logger.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
		return nil
	}
	return nil
}

func (t *feedbackReconcilerTask) syncPhaseProgressing() error {
	t.cluster.GetStatus().SetState(privatev1.ClusterState_CLUSTER_STATE_PROGRESSING)
	return nil
}

func (t *feedbackReconcilerTask) syncPhaseFailed() error {
	t.cluster.GetStatus().SetState(privatev1.ClusterState_CLUSTER_STATE_FAILED)
	return nil
}

func (t *feedbackReconcilerTask) syncPhaseReady(ctx context.Context) error {
	// Set the status of the cluster:
	clusterStatus := t.cluster.GetStatus()
	clusterStatus.SetState(privatev1.ClusterState_CLUSTER_STATE_READY)

	// In order to get the API and console URL we need to fetch the hosted cluster:
	err := t.fetchHostedCluster(ctx)
	if err != nil {
		return err
	}
	apiURL := t.calculateAPIURL()
	if apiURL != "" {
		clusterStatus.SetApiUrl(apiURL)
	}
	consoleURL := t.calculateConsoleURL()
	if consoleURL != "" {
		clusterStatus.SetConsoleUrl(consoleURL)
	}

	return nil
}

func (t *feedbackReconcilerTask) syncNodeRequests() error {
	for i := range len(t.object.Status.NodeRequests) {
		err := t.syncNodeRequest(&t.object.Status.NodeRequests[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *feedbackReconcilerTask) syncNodeRequest(nodeRequest *ckv1alpha1.NodeRequest) error {
	// Find a matching node set in the spec of the cluster:
	var nodeSetID string
	for candidateNodeSetID, candidateNodeSet := range t.cluster.GetSpec().GetNodeSets() {
		if candidateNodeSet.GetHostClass() == nodeRequest.ResourceClass {
			nodeSetID = candidateNodeSetID
			break
		}
	}
	if nodeSetID == "" {
		t.r.logger.Error(
			nil,
			"Failed to find a matching node set",
			"resource_class", nodeRequest.ResourceClass,
		)
		return nil
	}

	// Find or create the matching node set in the status of the cluster:
	nodeSets := t.cluster.GetStatus().GetNodeSets()
	if nodeSets == nil {
		nodeSets = map[string]*privatev1.ClusterNodeSet{}
		t.cluster.GetStatus().SetNodeSets(nodeSets)
	}
	nodeSet := nodeSets[nodeSetID]
	if nodeSet == nil {
		nodeSet = privatev1.ClusterNodeSet_builder{
			HostClass: nodeRequest.ResourceClass,
		}.Build()
		nodeSets[nodeSetID] = nodeSet
	}

	// Copy the number of nodes:
	oldValue := nodeSet.GetSize()
	newValue := int32(nodeRequest.NumberOfNodes)
	if newValue != oldValue {
		t.r.logger.Info(
			"Updating node set size",
			"resource_class", nodeRequest.ResourceClass,
			"old_value", oldValue,
			"new_value", newValue,
		)
		nodeSet.SetSize(newValue)
	}

	return nil
}

func (t *feedbackReconcilerTask) fetchHostedCluster(ctx context.Context) error {
	hostedClusterRef := t.object.Status.ClusterReference
	if hostedClusterRef == nil || hostedClusterRef.Namespace == "" || hostedClusterRef.HostedClusterName == "" {
		return nil
	}
	hostedClusterKey := clnt.ObjectKey{
		Namespace: hostedClusterRef.Namespace,
		Name:      hostedClusterRef.HostedClusterName,
	}
	hostedCluster := &hypershiftv1beta1.HostedCluster{}
	err := t.r.hubClient.Get(ctx, hostedClusterKey, hostedCluster)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	t.hostedCluster = hostedCluster
	return nil
}

func (t *feedbackReconcilerTask) calculateAPIURL() string {
	if t.hostedCluster == nil {
		return ""
	}
	apiEndpoint := t.hostedCluster.Status.ControlPlaneEndpoint
	if apiEndpoint.Host == "" || apiEndpoint.Port == 0 {
		return ""
	}
	return fmt.Sprintf("https://%s:%d", apiEndpoint.Host, apiEndpoint.Port)
}

func (t *feedbackReconcilerTask) calculateConsoleURL() string {
	if t.hostedCluster == nil {
		return ""
	}
	return fmt.Sprintf(
		"https://console-openshift-console.apps.%s.%s",
		t.hostedCluster.Name, t.hostedCluster.Spec.DNS.BaseDomain,
	)
}

func (t *feedbackReconcilerTask) handleDelete(ctx context.Context) (result ctrl.Result, err error) {
	// TODO.
	return
}

func (t *feedbackReconcilerTask) findClusterCondition(kind privatev1.ClusterConditionType) *privatev1.ClusterCondition {
	var condition *privatev1.ClusterCondition
	for _, current := range t.cluster.Status.Conditions {
		if current.Type == kind {
			condition = current
			break
		}
	}
	if condition == nil {
		condition = &privatev1.ClusterCondition{
			Type:   kind,
			Status: sharedv1.ConditionStatus_CONDITION_STATUS_FALSE,
		}
		t.cluster.Status.Conditions = append(t.cluster.Status.Conditions, condition)
	}
	return condition
}

func clone[M proto.Message](message M) M {
	return proto.Clone(message).(M)
}

func equal[M proto.Message](x, y M) bool {
	return proto.Equal(x, y)
}
