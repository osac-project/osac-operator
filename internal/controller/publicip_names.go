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
	"fmt"
)

const (
	publicipControllerName = "publicip-controller"

	// Must match the AAP playbook Service naming convention in osac-aap.
	publicIPServiceNamePrefix = "osac-pip-"
	defaultMetalLBNamespace   = "metallb-system"
)

var (
	osacPublicIPIDLabel                   string = fmt.Sprintf("%s/publicip-uuid", osacPrefix)
	osacPublicIPFeedbackFinalizer         string = fmt.Sprintf("%s/publicip-feedback", osacPrefix)
	osacPublicIPTargetNamespaceAnnotation string = fmt.Sprintf("%s/publicip-target-namespace", osacPrefix)
	osacPublicIPDetachFinalizer           string = fmt.Sprintf("%s/publicip-detach", osacPrefix)
)
