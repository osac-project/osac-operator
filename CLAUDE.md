# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

OSAC operator is a Kubernetes operator that reconciles infrastructure resources for the [Open Sovereign AI Cloud (OSAC)](https://github.com/osac-project) project. It integrates with the [fulfillment service](https://github.com/osac-project/fulfillment-service/) and Ansible Automation Platform to provision OpenShift clusters and compute instances with networking capabilities.

### Resources Managed

- **ClusterOrder** (`cord`) — OpenShift clusters via [Hosted Control Planes](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/hosted_control_planes/hosted-control-planes-overview)
- **ComputeInstance** (`ci`) — virtual machines via [KubeVirt](https://kubevirt.io/)
- **Tenant** — namespace and [OVN-Kubernetes UserDefinedNetwork](https://github.com/ovn-org/ovn-kubernetes/blob/master/go-controller/pkg/crd/userdefinednetwork/v1/network.go) for isolation
- **VirtualNetwork** (`vnet`) — cloud virtual network / VPC with IPv4/IPv6 CIDR blocks
- **Subnet** (`subnet`) — subnet within a VirtualNetwork
- **SecurityGroup** (`sg`) — network security (firewall) rules
- **PublicIPPool** (`pippool`) — pool of public IP addresses for MetalLB

## Development Commands

### Building and Testing

```bash
# Build the operator binary (runs all tests first)
make build

# Run unit tests only (excludes integration tests)
make test

# Run all tests including integration tests
make test-integration

# Run only kustomize validation tests
make test-kustomize

# Run smoke tests in kind cluster
make test-smoke

# Run golangci-lint
make lint

# Run golangci-lint with automatic fixes
make lint-fix

# Run go fmt
make fmt

# Run go vet
make vet
```

### Code Generation

```bash
# Generate CRD manifests, RBAC, and DeepCopy methods
make manifests

# Generate DeepCopy implementations
make generate

# Generate proto-based gRPC client code (from buf.gen.yaml)
buf generate
```

**IMPORTANT:** After modifying CRD types in `api/v1alpha1/*_types.go`, always run `make manifests generate` to update generated code and CRD manifests.

### Local Development

```bash
# Install CRDs into the cluster
make install

# Run controller locally against configured cluster
make run

# Uninstall CRDs from the cluster
make uninstall
```

### Container Images

```bash
# Build container image (default: ghcr.io/osac-project/osac-operator:latest)
make image-build IMG=<registry>/osac-operator:tag

# Push container image
make image-push IMG=<registry>/osac-operator:tag

# Run container locally
make image-run IMG=<registry>/osac-operator:tag

# Build and push for multiple platforms
make docker-buildx IMG=<registry>/osac-operator:tag
```

### Deployment

```bash
# Deploy operator to cluster
make deploy IMG=<registry>/osac-operator:tag

# Apply sample resources
kubectl apply -k config/samples/

# Undeploy operator
make undeploy

# Build installer YAML (generates dist/install.yaml)
make build-installer IMG=<registry>/osac-operator:tag
```

## Architecture

### Dual-Controller Pattern

Each resource has two controllers with distinct responsibilities:

```
┌─────────────────────────────────────────────────────────────┐
│  Resource Controller                                        │
│  - Provisions infrastructure via AAP/EDA                    │
│  - Manages finalizers and deletion                          │
│  - Updates CR status (Phase, Conditions, BackendNetworkID)  │
│  - Triggers AAP job templates                               │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
                   ┌──────────────┐
                   │ K8s CR State │
                   └──────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│  Feedback Controller                                        │
│  - Syncs CR state → fulfillment-service                     │
│  - Converts K8s Phase → proto State enum                    │
│  - Propagates BackendNetworkID, deletion status             │
│  - Sends Signal RPC to trigger reconciliation               │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
                 ┌──────────────────┐
                 │ fulfillment-     │
                 │ service (gRPC)   │
                 └──────────────────┘
```

**Why dual controllers?** Separation of concerns: the resource controller handles infrastructure lifecycle (create/update/delete), while the feedback controller handles integration with the fulfillment service. This allows the operator to function even if the fulfillment service is unavailable.

**Controller naming convention:**

| File Pattern | Purpose |
|--------------|---------|
| `{resource}_controller.go` | Resource controller (provisioning, lifecycle) |
| `{resource}_feedback_controller.go` | Feedback controller (sync to fulfillment-service) |

Resources with feedback controllers:
- ClusterOrder → `clusterorder_controller.go` + `feedback_controller.go`
- ComputeInstance → `computeinstance_controller.go` + `computeinstance_feedback_controller.go`
- VirtualNetwork → `virtualnetwork_controller.go` + `virtualnetwork_feedback_controller.go`
- Subnet → `subnet_controller.go` + `subnet_feedback_controller.go`
- SecurityGroup → `securitygroup_controller.go` + `securitygroup_feedback_controller.go`

### Provisioning Provider Abstraction

The operator supports two provisioning backends via the `ProvisioningProvider` interface (`internal/provisioning/provider.go`):

- **AAP provider** (`internal/aap/client.go`) — integrates directly with Ansible Automation Platform REST API, launches job templates, polls for status
- **EDA provider** (`internal/provisioning/eda_provider.go`) — triggers external automation via webhooks, uses synthetic job IDs (`eda-webhook-N`), cannot poll for status

The provider is selected per-deployment via `OSAC_PROVISIONING_PROVIDER` environment variable (defaults to `aap`). Networking controllers always use AAP regardless of this setting.

AAP template name resolution follows the convention: `{prefix}-{action}-{kind}` (e.g., `osac-create-subnet`, `osac-delete-security-group`). The prefix is configurable via `OSAC_AAP_TEMPLATE_PREFIX` (default: `osac`).

### Multi-cluster Support

The operator uses `multicluster-runtime` to support managing resources across clusters:

- **Hub cluster** — where the operator runs and CRDs are created
- **Remote cluster** — where Tenant and ComputeInstance resources are deployed (configured via `OSAC_REMOTE_CLUSTER_KUBECONFIG`)

ClusterOrder resources deploy HostedControlPlanes to the hub cluster.

### Controller Enable Flags

Each controller can be enabled/disabled independently via environment variables or command-line flags:
- `OSAC_ENABLE_CLUSTER_CONTROLLER` / `--enable-cluster-controller`
- `OSAC_ENABLE_COMPUTE_INSTANCE_CONTROLLER` / `--enable-compute-instance-controller`
- `OSAC_ENABLE_TENANT_CONTROLLER` / `--enable-tenant-controller`
- `OSAC_ENABLE_NETWORKING_CONTROLLER` / `--enable-networking-controller`

If none are set, all controllers are enabled. If any flag is set, only flagged controllers run.

### gRPC Client Generation

The operator consumes the private fulfillment service API via generated gRPC clients. Proto files are pulled from the `private-api` Buf Schema Registry module.

Generated code lives in `internal/api/osac/private/v1/` and is created by `buf generate` using the configuration in `buf.gen.yaml`. The module version is pinned (currently v0.0.52).

When the fulfillment service API changes, update the module version in `buf.gen.yaml` and run `buf generate`.

## File Organization

```
osac-operator/
├── api/v1alpha1/              # CRD type definitions
│   ├── {resource}_types.go    # Spec, Status, Phase enums
│   ├── {resource}_conditions.go  # Condition types and helpers
│   └── groupversion_info.go   # API group metadata
├── cmd/
│   └── main.go                # Operator entry point
├── internal/
│   ├── aap/                   # AAP REST API client implementation
│   ├── api/                   # Generated gRPC client code (from buf generate)
│   ├── controller/            # Reconciliation logic
│   │   ├── {resource}_controller.go          # Provisioning controller
│   │   ├── {resource}_feedback_controller.go # Feedback controller
│   │   ├── {resource}_names.go               # Resource naming constants
│   │   └── suite_test.go      # Test suite setup (envtest)
│   ├── helpers/               # Utility functions
│   ├── provisioning/          # ProvisioningProvider abstraction
│   │   ├── provider.go        # Interface definition
│   │   └── job_helpers.go     # Job status tracking
│   └── webhook/               # Webhook types
├── config/                    # Kustomize manifests
│   ├── crd/                   # Generated CRD manifests (DO NOT EDIT)
│   ├── rbac/                  # RBAC rules (generated from markers)
│   ├── manager/               # Deployment manifest
│   ├── default/               # Default kustomize overlay
│   └── samples/               # Example CRs and configuration Secret
└── test/
    ├── e2e/                   # End-to-end tests
    └── utils/                 # Test utilities
```

## CRD Development Patterns

### Type Definition Structure

All CRD types in `api/v1alpha1/{resource}_types.go` follow this pattern:

```go
// {Resource}Spec defines the desired state
type SubnetSpec struct {
    // +kubebuilder:validation:Required
    VirtualNetwork string `json:"virtualNetwork"`

    // +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
    IPv4CIDR string `json:"ipv4CIDR,omitempty"`
}

// {Resource}Status defines observed state
type SubnetStatus struct {
    Phase              SubnetPhase  `json:"phase,omitempty"`
    BackendNetworkID   string       `json:"backendNetworkID,omitempty"`
    Conditions         []Condition  `json:"conditions,omitempty"`
    JobHistory         []JobRecord  `json:"jobHistory,omitempty"`
}

// Phase enum for user-facing status
// +kubebuilder:validation:Enum=Progressing;Ready;Failed;Deleting
type SubnetPhase string

const (
    SubnetPhaseProgressing SubnetPhase = "Progressing"
    SubnetPhaseReady       SubnetPhase = "Ready"
    SubnetPhaseFailed      SubnetPhase = "Failed"
    SubnetPhaseDeleting    SubnetPhase = "Deleting"
)
```

Common kubebuilder markers:
- `+kubebuilder:validation:Required` — field is mandatory
- `+kubebuilder:validation:Pattern` — regex validation
- `+kubebuilder:validation:Enum` — restrict to specific values
- `+kubebuilder:validation:Minimum` — numeric minimum
- `+kubebuilder:printcolumn` — add column to `kubectl get` output

**IMPORTANT:** After modifying types, always run `make manifests generate` to update CRD YAML and DeepCopy methods.

### Controller Reconciliation Pattern

Controllers follow this standard structure:

```go
func (r *SubnetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := ctrllog.FromContext(ctx)

    // Step 1: Fetch the resource
    subnet := &v1alpha1.Subnet{}
    err := r.Client.Get(ctx, req.NamespacedName, subnet)
    if err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Step 2: Check management state annotation
    if subnet.Annotations[osacManagementStateAnnotation] == ManagementStateUnmanaged {
        log.Info("ignoring resource due to management-state annotation")
        return ctrl.Result{}, nil
    }

    // Step 3: Save old status for comparison
    oldStatus := subnet.Status.DeepCopy()

    // Step 4: Handle create/update vs deletion
    var res ctrl.Result
    if subnet.DeletionTimestamp.IsZero() {
        res, err = r.handleUpdate(ctx, subnet)
    } else {
        res, err = r.handleDelete(ctx, subnet)
    }

    // Step 5: Update status only if changed
    if !equality.Semantic.DeepEqual(subnet.Status, *oldStatus) {
        if err := r.Status().Update(ctx, subnet); err != nil {
            return res, err
        }
    }

    return res, err
}
```

Key patterns:
- Always use `client.IgnoreNotFound(err)` — resource may be deleted between list and get
- Check `management-state` annotation to skip reconciliation if `unmanaged`
- Save old status and compare before updating (avoids unnecessary writes and reconciliation loops)
- Separate `handleUpdate` and `handleDelete` logic for clarity

### Finalizer Management

```go
const osacSubnetFinalizer = "osac.openshift.io/subnet-finalizer"

func (r *SubnetReconciler) handleUpdate(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
    // Add finalizer on first reconcile
    if controllerutil.AddFinalizer(subnet, osacSubnetFinalizer) {
        if err := r.Update(ctx, subnet); err != nil {
            return ctrl.Result{}, err
        }
    }
    // ... provisioning logic
}

func (r *SubnetReconciler) handleDelete(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
    // Trigger deprovision if provisioned
    if subnet.Status.BackendNetworkID != "" {
        // ... trigger AAP deprovision job
    }

    // Remove finalizer when cleanup complete
    if controllerutil.RemoveFinalizer(subnet, osacSubnetFinalizer) {
        return ctrl.Result{}, r.Update(ctx, subnet)
    }

    return ctrl.Result{}, nil
}
```

Finalizer rules:
- Each controller has its own finalizer (e.g., `osac.openshift.io/{resource}-finalizer`)
- Feedback controllers add their own finalizer: `osac.openshift.io/{resource}-feedback-finalizer`
- Remove finalizer only after cleanup completes successfully
- If finalizer removal fails, it will be retried on next reconcile
- Multiple finalizers can coexist — each controller manages its own

### AAP Integration Pattern

Networking controllers use the `RunProvisioningLifecycle` helper:

```go
import "github.com/osac-project/osac-operator/internal/provisioning"

res, err := provisioning.RunProvisioningLifecycle(ctx, provisioning.ProvisioningLifecycleArgs{
    Provider:           r.ProvisioningProvider,
    Client:             r.Client,
    Object:             subnet,
    JobHistory:         &subnet.Status.JobHistory,
    MaxJobHistory:      r.MaxJobHistory,
    StatusPollInterval: r.StatusPollInterval,

    // Resource-specific callbacks
    OnBeforeProvision: func() error {
        // Validate preconditions, read parent resources
        return nil
    },
    OnSuccess: func(jobID string) error {
        // Extract outputs from AAP job, update status
        subnet.Status.Phase = v1alpha1.SubnetPhaseReady
        subnet.Status.BackendNetworkID = extractNetworkID(jobID)
        return nil
    },
    OnFailed: func(jobID string, jobErr error) error {
        subnet.Status.Phase = v1alpha1.SubnetPhaseFailed
        return nil
    },
})
```

### Feedback Controller Pattern

Feedback controllers sync K8s CR state to fulfillment-service:

```go
func (r *SubnetFeedbackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // Step 1: Fetch CR
    object := &v1alpha1.Subnet{}
    err := r.hubClient.Get(ctx, req.NamespacedName, object)
    if err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Step 2: Get subnet ID from label (set by fulfillment controller)
    subnetID, ok := object.Labels[osacSubnetIDLabel]
    if !ok {
        // Not created by fulfillment-service, ignore
        return ctrl.Result{}, nil
    }

    // Step 3: Fetch current state from fulfillment-service
    subnet, err := r.fetchSubnet(ctx, subnetID)
    if err != nil {
        // Special case: NotFound during deletion is OK
        if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
            // Record already archived, remove finalizer
            if controllerutil.RemoveFinalizer(object, osacSubnetFeedbackFinalizer) {
                return ctrl.Result{}, r.hubClient.Update(ctx, object)
            }
        }
        return ctrl.Result{}, err
    }

    // Step 4: Clone subnet for modification (compare before/after)
    updated := clone(subnet)

    // Step 5: Sync CR state to proto state
    if object.DeletionTimestamp.IsZero() {
        syncPhase(object.Status.Phase, updated)
        syncBackendNetworkID(object.Status.BackendNetworkID, updated)
    } else {
        updated.Status.State = privatev1.SubnetState_SUBNET_STATE_DELETING
    }

    // Step 6: Save if changed
    if !equal(subnet, updated) {
        _, err = r.subnetsClient.Update(ctx, &privatev1.SubnetsUpdateRequest{
            Object: updated,
        })
    }

    // Step 7: Signal fulfillment-service if last finalizer removed
    if shouldSignal(object) {
        r.subnetsClient.Signal(ctx, &privatev1.SubnetsSignalRequest{Id: subnetID})
    }

    return ctrl.Result{}, err
}
```

Phase to State mapping:

| K8s Phase | Proto State |
|-----------|-------------|
| Progressing | PENDING |
| Ready | READY |
| Failed | FAILED |
| Deleting | DELETING |
| (deletion failed) | DELETE_FAILED |

## Testing Patterns

### Unit Tests

Unit tests use Ginkgo and Gomega. Test files follow the `*_test.go` naming convention.

Controller tests use controller-runtime's `envtest` which spins up a real etcd and kube-apiserver.

```go
// Common test setup pattern (see internal/controller/suite_test.go)
var _ = BeforeSuite(func() {
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
    }
    cfg, err := testEnv.Start()
    // ... create client, manager, start controllers
})
```

### Integration Tests

Integration tests (`make test-integration`) include:
- Kustomize validation tests (`make test-kustomize`) — ensures all resources referenced in `kustomization.yaml` files exist
- Smoke tests (`make test-smoke`) — creates a kind cluster, deploys the operator, verifies basic functionality

The kind cluster is named `osac` (configurable via `KIND_CLUSTER_NAME`).

### Testing Against a Cluster

```bash
# Install CRDs
make install

# Run operator locally
make run

# In another terminal, apply sample CR
kubectl apply -f config/samples/osac_v1alpha1_subnet.yaml

# Watch reconciliation
kubectl get subnet -w

# Check operator logs (appear in terminal where `make run` is running)

# Debug specific resource
kubectl describe subnet my-subnet -n osac-networking
```

## Common Configuration

Configuration is supplied via environment variables from a Secret (see `config/samples/osac-config-secret.yaml`). Key variables:

### AAP Provider

- `OSAC_AAP_URL` — AAP server URL (required)
- `OSAC_AAP_TOKEN` — authentication token (required)
- `OSAC_AAP_TEMPLATE_PREFIX` — prefix for template name resolution (default: `osac`)
- `OSAC_AAP_STATUS_POLL_INTERVAL` — job status polling interval (default: 30s)
- `OSAC_AAP_INSECURE_SKIP_VERIFY` — skip TLS verification (default: false)

### EDA Provider

- `OSAC_CLUSTER_CREATE_WEBHOOK` / `OSAC_CLUSTER_DELETE_WEBHOOK`
- `OSAC_COMPUTE_INSTANCE_PROVISION_WEBHOOK` / `OSAC_COMPUTE_INSTANCE_DEPROVISION_WEBHOOK`

### Fulfillment Service gRPC

- `OSAC_FULFILLMENT_SERVER_ADDRESS` — gRPC server address (e.g., `fulfillment-service:50051`)
- `OSAC_FULFILLMENT_TOKEN_FILE` — path to auth token file

### Namespaces

- `OSAC_CLUSTER_ORDER_NAMESPACE` — namespace for ClusterOrder resources
- `OSAC_COMPUTE_INSTANCE_NAMESPACE` — namespace for ComputeInstance resources
- `OSAC_TENANT_NAMESPACE` — namespace for Tenant resources
- `OSAC_NETWORKING_NAMESPACE` — namespace for networking resources

## Code Quality

Pre-commit hooks are configured in `.pre-commit-config.yaml`:
- trailing whitespace, merge conflicts, large files
- yamllint (strict mode, excludes `config/` directory)
- golangci-lint with automatic fixes

Run manually: `pre-commit run --all-files`

Linter configuration is in `.golangci.yml`. Notable exclusions:
- Line length (lll) disabled for `api/*` and `internal/*`
- Duplication checks (dupl) disabled for `internal/*` and specific generated files

## Common Pitfalls

### 1. Forgetting to regenerate after CRD changes

**Problem:** Changes to `api/v1alpha1/*.go` don't take effect.

**Solution:** Always run `make manifests generate` after modifying CRD types.
- CRD YAML in `config/crd/` is generated — never edit directly
- DeepCopy methods in `api/v1alpha1/zz_generated.deepcopy.go` are also generated

### 2. Status update loops

**Problem:** Controller triggers infinite reconciliation because status is updated on every reconcile.

**Solution:**
- Save `oldStatus := subnet.Status.DeepCopy()` before reconciliation
- Use `equality.Semantic.DeepEqual(subnet.Status, *oldStatus)` to compare
- Only call `r.Status().Update()` if status actually changed

### 3. Finalizer removal timing

**Problem:** Resource stuck in Terminating state because finalizer removed too early or never removed.

**Solution:**
- Remove finalizer only after cleanup fully completes
- For feedback controllers: remove finalizer after syncing deletion state to fulfillment-service
- If multiple finalizers exist, each controller manages its own independently
- Handle errors during cleanup — retry on next reconcile, don't remove finalizer prematurely

### 4. AAP job polling inefficiency

**Problem:** Controller polls AAP on every reconcile, causing API rate limiting.

**Solution:**
- Use `StatusPollInterval` from config (default: 30s)
- Only poll when job is active (check `subnet.Status.JobHistory`)
- Return `ctrl.Result{RequeueAfter: interval}` for delayed reconciliation
- Don't poll completed jobs

### 5. NotFound errors during deletion

**Problem:** Feedback controller crashes when fulfillment-service returns NotFound during CR deletion.

**Solution:**
- Feedback controllers may see NotFound if fulfillment-service archives records before finalizer removed
- Handle gracefully:
```go
if !object.DeletionTimestamp.IsZero() && status.Code(err) == codes.NotFound {
    // Record already archived, remove finalizer
    controllerutil.RemoveFinalizer(object, osacSubnetFeedbackFinalizer)
    return ctrl.Result{}, r.hubClient.Update(ctx, object)
}
```

### 6. Parent resource lookup failures

**Problem:** Subnet controller fails when VirtualNetwork doesn't exist yet.

**Solution:**
- Networking resources often depend on parent resources (Subnet → VirtualNetwork)
- If parent not found, requeue with delay:
```go
return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
```
- Don't set Phase to Failed — parent may be created soon
- Use conditions to communicate transient errors to users

### 7. Vendor directory confusion

**Problem:** Changes to dependencies don't take effect.

**Solution:** The operator uses Go modules, not vendoring. If a `vendor/` directory exists, delete it. Run `go mod tidy` and `go mod download` instead.

### 8. Integration test failures

**Common causes:**
- Kustomize tests fail if files referenced in `kustomization.yaml` don't exist (happens after file renames/deletions)
- Smoke tests require Docker/Podman and kind installed
- Stale kind clusters from previous runs

**Solution:**
- After renaming/deleting manifests, update `kustomization.yaml`
- Clean up test clusters: `kind delete cluster --name osac`
- Check `make test-kustomize` before committing manifest changes

### 9. gRPC client version mismatches

**Problem:** Operator expects fields that don't exist in fulfillment-service API.

**Solution:**
- Always update the module version in `buf.gen.yaml`, never edit proto files directly
- Run `buf generate` to regenerate client code in `internal/api/`
- Generated files should be committed to the repository
- Coordinate with fulfillment-service team when making breaking API changes

### 10. AAP template naming conventions

**Problem:** AAP jobs fail with "template not found" error.

**Solution:**
- Template names follow strict conventions: `{prefix}-{action}-{resource-kind}`
- Kind is derived from CRD name in lowercase with hyphens (e.g., SecurityGroup → security-group)
- Default prefix is `osac` (configurable via `OSAC_AAP_TEMPLATE_PREFIX`)
- Examples: `osac-create-subnet`, `osac-delete-virtual-network`
- Per-resource template overrides (env vars like `OSAC_CLUSTER_AAP_PROVISION_TEMPLATE`) take precedence

## Common Tasks

### Adding a New CRD

1. Generate scaffold:
```bash
kubebuilder create api --group osac.openshift.io --version v1alpha1 --kind MyResource
```
2. Define types in `api/v1alpha1/myresource_types.go`
3. Generate manifests and code:
```bash
make manifests generate
```
4. Create controller in `internal/controller/myresource_controller.go`
5. Register controller in `cmd/main.go`:
```go
if err = (&controller.MyResourceReconciler{
    Client: mgr.GetClient(),
    Scheme: mgr.GetScheme(),
}).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "MyResource")
    os.Exit(1)
}
```

### Adding a Field to Existing CRD

1. Add field to `api/v1alpha1/{resource}_types.go`:
```go
type SubnetSpec struct {
    // ... existing fields

    // +kubebuilder:validation:Optional
    // +kubebuilder:validation:Minimum=1
    MTU int32 `json:"mtu,omitempty"`
}
```
2. Run `make manifests generate`
3. Update controller logic to handle new field in `internal/controller/{resource}_controller.go`
4. Update feedback controller if field needs sync to fulfillment-service
5. Verify CRD updated:
```bash
kubectl get crd subnets.osac.openshift.io -o yaml | grep mtu
```

### Debugging Controller Issues

```bash
# Check CR status
kubectl describe subnet my-subnet -n osac-networking

# View operator logs
kubectl logs -n osac-system deployment/osac-operator-controller-manager -f

# Check AAP job status (if AAP provider)
# Use AAP UI or CLI to view job outputs and errors

# Enable verbose logging
# Edit config/manager/manager.yaml, add to args:
#   - --zap-log-level=debug
```

## Cross-Repo Coordination

When making changes that span multiple repos:

### Networking Resource Changes

**Typical change order:**
1. **fulfillment-service**: Update proto definitions, add fields, regenerate
2. **osac-operator**: Update CRD types, controller logic
3. **osac-aap**: Update Ansible roles/playbooks to handle new fields
4. **osac-installer**: Update submodules, add RBAC if needed

**Example:** Adding MTU field to Subnet
1. Add `mtu` field to `subnet_type.proto` in fulfillment-service
2. Run `buf generate` in fulfillment-service
3. Update `buf.gen.yaml` in osac-operator to reference new private-api version
4. Run `buf generate` in osac-operator
5. Add `mtu` field to `SubnetSpec` in `api/v1alpha1/subnet_types.go`
6. Run `make manifests generate` in osac-operator
7. Update osac-aap playbook to provision the MTU value
8. Update osac-installer submodule refs

### RBAC Changes

If adding a new CRD or subresource:
1. Add RBAC markers to controller:
```go
//+kubebuilder:rbac:groups=osac.openshift.io,resources=myresources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=osac.openshift.io,resources=myresources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=osac.openshift.io,resources=myresources/finalizers,verbs=update
```
2. Run `make manifests` to regenerate `config/rbac/role.yaml`
3. Update osac-installer hub-access Role if fulfillment controller needs access

## Git Workflow

When committing changes:

```bash
# Run linters and tests before committing
make lint test

# Generate manifests if CRD types changed
make manifests generate

# Commit message format (include Jira ticket)
git commit -m "MGMT-XXXXX: description of change"
```

**Pull request checklist:**
- [ ] `make manifests generate` run if types changed
- [ ] `make lint` passes
- [ ] `make test` passes
- [ ] CRD changes tested against a cluster
- [ ] Cross-repo dependencies documented in PR description
- [ ] RBAC changes reflected in osac-installer if needed

## Key Technologies

- **Kubebuilder** — operator framework and scaffolding
- **controller-runtime** — Kubernetes controller library
- **multicluster-runtime** — multi-cluster resource management
- **Ginkgo/Gomega** — BDD testing framework
- **Buf** — Protocol Buffer toolchain (lint, generate)
- **Kustomize** — Kubernetes manifest templating
- **HyperShift** — OpenShift Hosted Control Planes API
- **OVN-Kubernetes** — UserDefinedNetwork CRD
- **KubeVirt** — VM management API

## Links

- [Kubebuilder Book](https://book.kubebuilder.io/) — Controller patterns and best practices
- [controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime) — Core reconciliation framework
- [fulfillment-service](../fulfillment-service/CLAUDE.md) — Backend API integration
- [OSAC Project](https://github.com/osac-project) — Parent organization
