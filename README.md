# OSAC operator

Reconciles `ClusterOrder`, `HostPool`, `ComputeInstance`, and `Tenant` custom
resources created by the [fulfillment
service](https://github.com/osac-project/fulfillment-service/) or elsewhere.
`ClusterOrder` deploys OpenShift clusters via [Hosted Control
Planes](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/hosted_control_planes/hosted-control-planes-overview).
`ComputeInstance` provisions virtual machines via
[KubeVirt](https://kubevirt.io/). `HostPool` creates a namespace and notifies an
external system via webhooks. `Tenant` provisions a namespace and an
[OVN-Kubernetes
UserDefinedNetwork](https://github.com/ovn-org/ovn-kubernetes/blob/master/go-controller/pkg/crd/userdefinednetwork/v1/network.go)
for tenant networking.

## Description

OSAC operator is part of the [Open Sovereign AI Cloud
(OSAC)](https://github.com/osac-project) project. All four resource types are
created by the fulfillment service (or elsewhere); the operator reconciles them.
For `ClusterOrder` it drives cluster deployment using Hosted Control Planes. For
`ComputeInstance` it provisions VMs using KubeVirt. For `HostPool` it creates a
dedicated namespace and calls create/delete webhooks so an external system can
provision the hosts. For `Tenant` it creates a namespace and an OVN-Kubernetes
UserDefinedNetwork (layer-2, persistent IPAM) for tenant isolation.

## Configuration

Configuration is supplied via environment variables (e.g. from a Secret mounted
into the manager deployment). The following are supported:

### Cluster provisioning

- `OSAC_CLUSTER_CREATE_WEBHOOK` — URL to POST the JSON-serialized ClusterOrder
  after creating the target namespace, service account, and rolebinding.
- `OSAC_CLUSTER_DELETE_WEBHOOK` — URL to POST the JSON-serialized ClusterOrder
  before deleting the target namespace.
- `OSAC_CLUSTER_ORDER_NAMESPACE` — namespace to watch for ClusterOrder resources
  (optional).

### HostPool provisioning

- `OSAC_HOSTPOOL_CREATE_WEBHOOK` — URL to POST when a HostPool’s namespace is
  created.
- `OSAC_HOSTPOOL_DELETE_WEBHOOK` — URL to POST before deleting the HostPool
  namespace.
- `OSAC_HOSTPOOL_ORDER_NAMESPACE` — namespace to watch for HostPool resources
  (optional).

### ComputeInstance provisioning

The operator supports two providers: **EDA** (webhooks) and **AAP** (Ansible
Automation Platform).

**Provider selection:**

- `OSAC_PROVISIONING_PROVIDER` — `"eda"` (default) or `"aap"`.

**EDA provider (default):**

- `OSAC_COMPUTE_INSTANCE_PROVISION_WEBHOOK` — webhook URL for provisioning.
- `OSAC_COMPUTE_INSTANCE_DEPROVISION_WEBHOOK` — webhook URL for deprovisioning.

**AAP provider:**

- `OSAC_AAP_URL` — AAP server URL (required when using AAP).
- `OSAC_AAP_TOKEN` — AAP authentication token (required when using AAP).
- `OSAC_AAP_PROVISION_TEMPLATE` — template name for provisioning (optional).
- `OSAC_AAP_DEPROVISION_TEMPLATE` — template name for deprovisioning (optional).
- `OSAC_AAP_STATUS_POLL_INTERVAL` — job status polling interval (optional,
  default: 30s). Duration strings e.g. `30s`, `1m`. -
- `OSAC_AAP_INSECURE_SKIP_VERIFY` — skip TLS verification for AAP (optional,
  default: false). Set to a truthy value to enable.

**Namespaces and remote cluster:**

- `OSAC_COMPUTE_INSTANCE_NAMESPACE` — namespace for ComputeInstance resources
  (optional).
- `OSAC_TENANT_NAMESPACE` — namespace for Tenant resources (optional).
- `OSAC_REMOTE_CLUSTER_KUBECONFIG` — path to kubeconfig for the remote cluster
  used by tenant and compute-instance controllers (optional). Not supported when
  cluster or host-pool controllers are enabled.

### Networking (VirtualNetwork, Subnet, SecurityGroup)

Networking controllers use AAP only (no EDA webhook support).

- `OSAC_NETWORKING_NAMESPACE` — namespace for networking resources (optional).
- AAP-related variables above (`OSAC_AAP_URL`, `OSAC_AAP_TOKEN`, etc.) apply
  when using networking controllers.

### Job history

- `OSAC_MAX_JOB_HISTORY` — maximum number of job history entries to retain
  (optional).

### Fulfillment service (gRPC)

- `OSAC_FULFILLMENT_SERVER_ADDRESS` — fulfillment service gRPC address
  (e.g. `fulfillment-service:50051`).
- `OSAC_FULFILLMENT_TOKEN_FILE` — path to file containing the gRPC auth token.
- `OSAC_MINIMUM_REQUEST_INTERVAL` — minimum duration between calls to the same
  webhook URL (optional). Duration string, default: 0.

### Controller enable flags

Each controller can be enabled or disabled. If none of these are set, all
controllers are enabled.

- `OSAC_ENABLE_TENANT_CONTROLLER` — enable Tenant controller (truthy/falsy).
- `OSAC_ENABLE_HOST_POOL_CONTROLLER` — enable HostPool controller
  (truthy/falsy).
- `OSAC_ENABLE_COMPUTE_INSTANCE_CONTROLLER` — enable ComputeInstance controller
  (truthy/falsy).
- `OSAC_ENABLE_CLUSTER_CONTROLLER` — enable ClusterOrder controller
  (truthy/falsy).
- `OSAC_ENABLE_NETWORKING_CONTROLLER` — enable networking controllers
  (truthy/falsy).

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
above commands don’t work.

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

NOTE: The makefile target mentioned above generates an ‘install.yaml’ file in
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

Licensed under the Apache License, Version 2.0 (the “License”); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an “AS IS” BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
