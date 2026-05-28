package controller

import (
	"fmt"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

const (
	subjectKindServiceAccount    string = "ServiceAccount"
	defaultServiceAccountName    string = "osac"
	defaultHostedClusterName     string = "cluster"
	defaultRoleBindingName       string = "osac"
	defaultClusterOrderNamespace string = "osac-orders"
	hubAccessServiceAccountName  string = "hub-access"
	hubAccessRoleBindingName     string = "hub-access"
	hubAccessClusterRoleName     string = "hub-access-hosted-clusters"
)

var (
	osacClusterOrderNameLabel     string = fmt.Sprintf("%s/clusterorder", osacPrefix)
	osacClusterOrderIDLabel       string = fmt.Sprintf("%s/clusterorder-uuid", osacPrefix)
	osacFinalizer                 string = fmt.Sprintf("%s/finalizer", osacPrefix)
	osacManagementStateAnnotation string = fmt.Sprintf("%s/management-state", osacPrefix)
)

func generateNamespaceName(instance *v1alpha1.ClusterOrder) string {
	return fmt.Sprintf("%s-%s", instance.GetNamespace(), instance.GetName())
}
