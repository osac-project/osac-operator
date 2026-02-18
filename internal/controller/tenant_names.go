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
	"encoding/base32"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

var (
	// osacTenantRefLabel the label used to reference the tenant object
	osacTenantRefLabel string = fmt.Sprintf("%s/tenant-ref", osacPrefix)

	// osacProjectRefLabel is the label used to reference the project in which the tenant obehct lives
	osacProjectRefLabel string = fmt.Sprintf("%s/project", osacPrefix)

	// tenantFinalizer is the finalizer used to clean up the tenant
	tenantFinalizer string = fmt.Sprintf("%s/tenant", osacPrefix)

	// osacTenantAnnotation is the annotation used to reference the tenant name
	osacTenantAnnotation string = fmt.Sprintf("%s/tenant", osacPrefix)

	// udnName is the default name of the user defined network created inside tenant's namespace
	udnName = "udn"
)

// generateTenantNamespaceName generates a namespace name for a tenant by hashing the tenant name and adding it to the
// project (or namespace) name
func generateTenantNamespaceName(instance *v1alpha1.Tenant) string {
	return fmt.Sprintf("%s-%s", instance.GetNamespace(), encodeTenantName(instance.Spec.Name))
}

// encodeTenantName hashes the tenant name into a unique string in order to make it compliant with Kubernetes
// naming conventions
func encodeTenantName(s string) string {
	hashedName := fnv.New64a()
	hashedName.Write([]byte(s))

	b32Encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	return strings.ToLower(b32Encoding.EncodeToString(hashedName.Sum(nil)))
}
