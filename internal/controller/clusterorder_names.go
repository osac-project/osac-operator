package controller

import (
	"fmt"

	v1alpha1 "github.com/osac/osac-operator/api/v1alpha1"
)

const (
	defaultServiceAccountName    string = "cloudkit"
	defaultHostedClusterName     string = "cluster"
	defaultRoleBindingName       string = "cloudkit"
	defaultClusterOrderNamespace string = "cloudkit-orders"
)

var (
	cloudkitClusterOrderNameLabel     string = fmt.Sprintf("%s/clusterorder", cloudkitPrefix)
	cloudkitClusterOrderIDLabel       string = fmt.Sprintf("%s/clusterorder-uuid", cloudkitPrefix)
	cloudkitFinalizer                 string = fmt.Sprintf("%s/finalizer", cloudkitPrefix)
	cloudkitManagementStateAnnotation string = fmt.Sprintf("%s/management-state", cloudkitPrefix)
)

func generateNamespaceName(instance *v1alpha1.ClusterOrder) string {
	return fmt.Sprintf("%s-%s", instance.GetNamespace(), instance.GetName())
}
