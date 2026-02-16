# osac-operator

Deploy OpenShift clusters in response to `ClusterOrders` using [Hosted Control Planes].

[hosted control planes]: https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/hosted_control_planes/hosted-control-planes-overview

## Description

osac-operator is part of the [OSAC][osac] (Open Source AI Cloud) project. It accepts `ClusterOrder` requests from the [fulfillment service] (or from elsewhere), and drives the process of deploying a new cluster.

[osac]: https://github.com/osac
[fulfillment service]: https://github.com/osac/fulfillment-service/

## Configuration

osac-operator makes use of the following environment variables:

### Cluster Provisioning
- `CLOUDKIT_CLUSTER_CREATE_WEBHOOK` -- the operator will post the JSON-serialized ClusterOrder to this URL after creating the target namespace, service account, and rolebinding.
- `CLOUDKIT_CLUSTER_DELETE_WEBHOOK` -- the operator will post the JSON-serialized ClusterOrder to this URL before deleting the target namespace.

### ComputeInstance Provisioning

The operator supports two provisioning providers for ComputeInstance resources:

**Provider Selection:**
- `CLOUDKIT_PROVISIONING_PROVIDER` -- selects the provider: `"eda"` (default) or `"aap"`

**EDA Provider (default):**
- `CLOUDKIT_COMPUTE_INSTANCE_PROVISION_WEBHOOK` -- webhook URL for provisioning
- `CLOUDKIT_COMPUTE_INSTANCE_DEPROVISION_WEBHOOK` -- webhook URL for deprovisioning

**AAP Provider:**
- `CLOUDKIT_AAP_URL` -- AAP server URL (required)
- `CLOUDKIT_AAP_TOKEN` -- AAP authentication token (required)
- `CLOUDKIT_AAP_PROVISION_TEMPLATE` -- template name for provisioning (optional)
- `CLOUDKIT_AAP_DEPROVISION_TEMPLATE` -- template name for deprovisioning (optional)
- `CLOUDKIT_AAP_STATUS_POLL_INTERVAL` -- job status polling interval (optional, default: 30s)

See `config/samples/cloudkit-config-secret.yaml` for a complete configuration example.

## Getting Started

### Prerequisites
- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make image-build image-push IMG=<some-registry>/osac-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/osac-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/osac-operator:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without
its dependencies.

2. Using the installer

Users can just run kubectl apply -f <URL for YAML BUNDLE> to install the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/osac-operator/<tag or branch>/dist/install.yaml
```

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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
