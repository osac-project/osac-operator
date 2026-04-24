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
)

const (
	// defaultStorageClassSentinel is the label value that marks a shared StorageClass
	// available to all tenants. No Tenant CR can be named "Default" because uppercase
	// is forbidden in Kubernetes resource names.
	defaultStorageClassSentinel = "Default"

	// tenantControllerName is the name used when creating the event recorder
	tenantControllerName = "tenant-controller"

	// eventReasonDuplicateStorageClass is the event reason emitted when multiple
	// StorageClasses match a tenant
	eventReasonDuplicateStorageClass = "DuplicateStorageClass"

	// eventActionDetectDuplicate is the event action for duplicate StorageClass detection
	eventActionDetectDuplicate = "DetectDuplicate"
)

var (
	// osacTenantRefLabel the label used to reference the tenant object
	osacTenantRefLabel string = fmt.Sprintf("%s/tenant-ref", osacPrefix)

	// osacProjectRefLabel is the label used to reference the project in which the tenant obehct lives
	osacProjectRefLabel string = fmt.Sprintf("%s/project", osacPrefix)

	// osacTenantAnnotation is the annotation used to reference the tenant name
	osacTenantAnnotation string = fmt.Sprintf("%s/tenant", osacPrefix)
)
