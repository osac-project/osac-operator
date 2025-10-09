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
	"fmt"

	v1alpha1 "github.com/innabox/cloudkit-operator/api/v1alpha1"
)

const (
	defaultHostName      string = "host"
	defaultHostNamespace string = "cloudkit-host-orders"
	cloudkitHostPrefix   string = "cloudkit.openshift.io"
)

var (
	cloudkitHostNameLabel                 string = fmt.Sprintf("%s/host", cloudkitHostPrefix)
	cloudkitHostIDLabel                   string = fmt.Sprintf("%s/host-uuid", cloudkitHostPrefix)
	hostFinalizer                         string = fmt.Sprintf("%s/finalizer", cloudkitHostPrefix)
	cloudkitHostManagementStateAnnotation string = fmt.Sprintf("%s/management-state", cloudkitHostPrefix)
)

func generateHostNamespaceName(instance *v1alpha1.Host) string {
	return fmt.Sprintf("%s-%s", instance.GetNamespace(), instance.GetName())
}
