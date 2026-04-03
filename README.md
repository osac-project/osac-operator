# OSAC operator

Reconciles custom resources created by the [fulfillment
service](https://github.com/osac-project/fulfillment-service/) or elsewhere.

## Description

OSAC operator is part of the [Open Sovereign AI Cloud
(OSAC)](https://github.com/osac-project) project. It watches the following
custom resources and reconciles them to their desired state:

- **ClusterOrder** (`cord`) — deploys OpenShift clusters via [Hosted Control
  Planes](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/hosted_control_planes/hosted-control-planes-overview).
- **ComputeInstance** (`ci`) — provisions virtual machines via
  [KubeVirt](https://kubevirt.io/).
- **Tenant** — creates a namespace and an [OVN-Kubernetes
  UserDefinedNetwork](https://github.com/ovn-org/ovn-kubernetes/blob/master/go-controller/pkg/crd/userdefinednetwork/v1/network.go)
  (layer-2, persistent IPAM) for tenant isolation.
- **VirtualNetwork** (`vnet`) — represents a cloud virtual network / VPC with
  IPv4/IPv6 CIDR blocks and a NetworkClass.
- **Subnet** (`subnet`) — represents a subnet within a VirtualNetwork.
- **SecurityGroup** (`sg`) — defines network security (firewall) rules with
  ingress/egress rules, protocols, port ranges, and CIDR blocks.

## Configuration

Configuration is supplied via environment variables (e.g. from a Secret mounted
into the manager deployment). The following are supported:

### Provisioning providers

The operator supports two provisioning providers. The provider is selected
**per-deployment** (not per-resource-type) and applies to all controllers that
perform provisioning (ClusterOrder, ComputeInstance). Networking controllers
(VirtualNetwork, Subnet, SecurityGroup) always use AAP.

- `OSAC_PROVISIONING_PROVIDER` — `"eda"` or `"aap"` (default: `"aap"`).
  Ignored by networking controllers, which always use AAP.

**EDA provider** — triggers external automation via webhooks. Job IDs are
synthetic (`eda-webhook-N`). The EDA provider cannot poll for job status;
completion is tracked via resource phase changes and finalizers.

- `OSAC_CLUSTER_CREATE_WEBHOOK` — webhook URL for cluster provisioning.
- `OSAC_CLUSTER_DELETE_WEBHOOK` — webhook URL for cluster deprovisioning.
- `OSAC_COMPUTE_INSTANCE_PROVISION_WEBHOOK` — webhook URL for compute instance
  provisioning.
- `OSAC_COMPUTE_INSTANCE_DEPROVISION_WEBHOOK` — webhook URL for compute instance
  deprovisioning.

**AAP provider** — integrates directly with the Ansible Automation Platform REST
API. Launches job/workflow templates and polls AAP for job status.

- `OSAC_AAP_URL` — AAP server URL (required).
- `OSAC_AAP_TOKEN` — AAP authentication token (required).
- `OSAC_AAP_PROVISION_TEMPLATE` — default AAP template name for provisioning
  (optional).
- `OSAC_AAP_DEPROVISION_TEMPLATE` — default AAP template name for
  deprovisioning (optional).
- `OSAC_AAP_TEMPLATE_PREFIX` — prefix for convention-based template name
  resolution (default: `"osac"`). Used by networking controllers to derive
  template names (e.g. `osac-create-virtual-network`).
- `OSAC_AAP_STATUS_POLL_INTERVAL` — job status polling interval (default:
  `30s`). Duration string, e.g. `30s`, `1m`.
- `OSAC_AAP_INSECURE_SKIP_VERIFY` — skip TLS verification for AAP (default:
  `false`).

**Per-resource AAP template overrides** (optional, override the shared
defaults):

- `OSAC_CLUSTER_AAP_PROVISION_TEMPLATE` /
  `OSAC_CLUSTER_AAP_DEPROVISION_TEMPLATE` — override for ClusterOrder.

Networking controllers derive template names from the prefix:
`{prefix}-{action}-{kind}` (e.g. `osac-create-subnet`,
`osac-delete-security-group`).

### Namespaces

- `OSAC_CLUSTER_ORDER_NAMESPACE` — namespace for ClusterOrder resources
  (optional).
- `OSAC_COMPUTE_INSTANCE_NAMESPACE` — namespace for ComputeInstance resources
  (optional).
- `OSAC_TENANT_NAMESPACE` — namespace for Tenant resources (optional).
- `OSAC_NETWORKING_NAMESPACE` — namespace for networking resources
  (VirtualNetwork, Subnet, SecurityGroup) (optional).

### Remote cluster

- `OSAC_REMOTE_CLUSTER_KUBECONFIG` — path to kubeconfig for the remote cluster
  used by Tenant and ComputeInstance controllers (optional). Not supported when
  ClusterOrder controller is enabled.

### Job history

- `OSAC_MAX_JOB_HISTORY` — maximum number of job history entries to retain per
  resource (default: 10).

### Fulfillment service (gRPC)

- `OSAC_FULFILLMENT_SERVER_ADDRESS` — fulfillment service gRPC address
  (e.g. `fulfillment-service:50051`).
- `OSAC_FULFILLMENT_TOKEN_FILE` — path to file containing the gRPC auth token.
- `OSAC_MINIMUM_REQUEST_INTERVAL` — minimum duration between calls to the same
  webhook URL (optional). Duration string, default: `0`.

### Controller enable flags

Each controller can be enabled or disabled. If none of these are set, all
controllers are enabled. If any flag is set, only the flagged controllers are
enabled.

- `OSAC_ENABLE_CLUSTER_CONTROLLER` — enable ClusterOrder controller
  (truthy/falsy).
- `OSAC_ENABLE_COMPUTE_INSTANCE_CONTROLLER` — enable ComputeInstance controller
  (truthy/falsy).
- `OSAC_ENABLE_TENANT_CONTROLLER` — enable Tenant controller (truthy/falsy).
- `OSAC_ENABLE_NETWORKING_CONTROLLER` — enable networking controllers
  (VirtualNetwork, Subnet, SecurityGroup) (truthy/falsy).

See `config/samples/osac-config-secret.yaml` for a complete configuration
example.

## Getting Started

### Prerequisites

- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster

**Build and push your image to the location specified by `IMG`:**

``` sh
make image-build image-push IMG=<some-registry>/osac-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you
specified. And it is required to have access to pull the image from the working
environment. Make sure you have the proper permission to the registry if the
above commands don't work.

**Install the CRDs into the cluster:**

``` sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

``` sh
make deploy IMG=<some-registry>/osac-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself
> cluster-admin privileges or be logged in as admin.

**Create instances of your solution**

You can apply the samples (examples) from the config/sample:

``` sh
kubectl apply -k config/samples/
```

> **NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall

**Delete the instances (CRs) from the cluster:**

``` sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

``` sh
make uninstall
```

**UnDeploy the controller from the cluster:**

``` sh
make undeploy
```

## Project Distribution

Following are the steps to build the installer and distribute this project to
users.

1.  Build the installer for the image built and published in the registry:

``` sh
make build-installer IMG=<some-registry>/osac-operator:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml' file in
the dist directory. This file contains all the resources built with Kustomize,
which are necessary to install this project without its dependencies.

2.  Using the installer

Users can just run kubectl apply -f <URL for YAML BUNDLE> to install the
project, i.e.:

``` sh
kubectl apply -f https://raw.githubusercontent.com/<org>/osac-operator/<tag or branch>/dist/install.yaml
```

## Contributing

// TODO(user): Add detailed information on how you would like others to
contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder
Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
