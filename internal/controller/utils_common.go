package controller

import (
	"context"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func getClusterClient(ctx context.Context, mgr mcmanager.Manager, clusterName string) (client.Client, error) {
	if clusterName == localClusterName {
		return mgr.GetLocalManager().GetClient(), nil
	}

	cluster, err := mgr.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	return cluster.GetClient(), nil
}
