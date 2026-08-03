# AGENTS.md

## Project Overview

OSAC operator is a Kubernetes operator that reconciles infrastructure resources for the [OSAC](https://github.com/osac-project) project. It integrates with the [fulfillment service](https://github.com/osac-project/fulfillment-service/) and Ansible Automation Platform to provision OpenShift clusters and compute instances with networking. Includes a console proxy (aggregated API server) for KubeVirt VM console/VNC access via WebSocket.

### Resources Managed

- **ClusterOrder** (`cord`) — OpenShift clusters via Hosted Control Planes
- **ComputeInstance** (`ci`) — virtual machines via KubeVirt
- **Tenant** — namespace and OVN-Kubernetes UserDefinedNetwork for isolation
- **VirtualNetwork** (`vnet`) — cloud VPC with IPv4/IPv6 CIDR blocks
- **Subnet** (`subnet`) — subnet within a VirtualNetwork
- **SecurityGroup** (`sg`) — network security rules
- **ExternalIPPool** (`extippool`) — external IP pool
- **ExternalIP** — external IP allocated from ExternalIPPool
- **ExternalIPAttachment** — attachment of ExternalIP to ComputeInstance
- **NATGateway** — outbound SNAT for a VirtualNetwork

## Critical Rules

- **Always `make manifests generate`** after modifying CRD types in `api/v1alpha1/*_types.go`
- **Always `make helm-crds`** after regenerating CRDs (runs `make manifests` + `hack/sync-helm-crds.py`) to sync Helm charts (or run `make check-helm-crds` to verify sync)
- **Never edit** `config/crd/`, `zz_generated.deepcopy.go`, `internal/api/`, or `go.sum` — all generated (regenerate `go.sum` with `go mod tidy`)
- **Always `buf generate`** after updating the module version in `buf.gen.yaml` (check the file for the current pinned version)
- **Commit message format**: `OSAC-XXXXX: description of change` (Red Hat Jira key prefix)
- **AI attribution**: Use `Assisted-by: Claude Code <noreply@anthropic.com>` (not Co-Authored-By)
- **Sign-off required**: `git commit -s` for DCO compliance
- **Always `make lint`** after changing any Go code — fix all issues before proceeding
- Run `make lint test` before committing

## Development Commands

```bash
# Build and test (requires Go 1.26.3)
make build                    # Build manager + console-proxy (runs 'make test' first, then builds)
make test                     # Unit tests only (excludes e2e)
make test-integration         # Runs test + test-kustomize + test-smoke
make test-kustomize           # Validate all kustomization.yaml configs (catches missing files)
make test-smoke               # Create kind cluster 'osac-test', apply CRDs + samples, verify ComputeInstance
make test-integration-kind    # Go integration tests against a running kind cluster
make lint                     # golangci-lint v2.12.1 (strict — always run before commit)
make fmt                      # go fmt + goimports
make vet                      # go vet

# Code generation
make manifests                # Generate CRD manifests + RBAC
make generate                 # Generate DeepCopy
buf generate                  # Generate gRPC client from Buf Schema Registry

# Local development
make install                  # Install CRDs into cluster
make run                      # Run controller locally
make uninstall                # Remove CRDs

# Container builds (multi-arch via Containerfile, UBI10 base)
make image-build IMG=<registry>/osac-operator:tag
make image-push IMG=<registry>/osac-operator:tag
make deploy IMG=<registry>/osac-operator:tag
make undeploy

# Helm
make helm-crds                # Regenerate manifests + sync CRDs to charts/operator-crds/
make check-helm-crds          # Verify CRD sync (CI enforces)
```

## Architecture

### Dual-Controller Pattern

Each resource has a **resource controller** (provisions via AAP, manages finalizers) and a **feedback controller** (syncs state to fulfillment-service via gRPC). See `.claude/rules/controller-patterns.md` for reconciliation, finalizer, and AAP integration patterns.

`internal/controller/storage_controller.go` (`StorageReconciler`) is an exception to this pattern: it reconciles `Tenant` resources to provision/deprovision tenant storage (StorageClass discovery, CaaS backend and cluster storage lifecycle) rather than owning its own CRD or following the AAP-based provisioning flow.

### Provisioning

ClusterOrder and ComputeInstance controllers use direct AAP REST API integration via the `ProvisioningProvider` interface (`pkg/provisioning/provider.go` and `pkg/aap/client.go`). Networking controllers (VirtualNetwork, Subnet, SecurityGroup) and the PublicIP/ExternalIP family of controllers also use this pattern. `pkg/provisioning` and `pkg/aap` are public packages consumed outside this repo (e.g., `bare-metal-fulfillment-operator`) — changes to their interfaces can impact those consumers. Management-state annotation (`osac.openshift.io/management-state = Unmanaged`) is checked by every resource controller except `tenant_controller.go` to skip reconciliation.

### Multi-cluster

Built on controller-runtime and multicluster-runtime (see `go.mod` for current versions). Hub cluster runs the operator; remote cluster hosts Tenant/ComputeInstance resources (via `OSAC_REMOTE_CLUSTER_KUBECONFIG`).

### Console Proxy

Aggregated API server in `cmd/console-proxy/` provides KubeVirt VM console/VNC access via WebSocket. TLS configured via `OSAC_CONSOLE_PROXY_TLS_CERT_FILE` and `OSAC_CONSOLE_PROXY_TLS_KEY_FILE`. Server implementation in `internal/consoleproxy/` (auth, config resolver, discovery, server, subresource handlers). Tested in `test/integration/console_proxy_test.go`.

### gRPC Client

Consumes private fulfillment-service API. Generated in `internal/api/` from Buf Schema Registry (module version pinned in `buf.gen.yaml`). Update version there and run `buf generate` when API changes. Auth via `OSAC_FULFILLMENT_TOKEN_FILE`.

## File Organization

```text
api/v1alpha1/              # CRD type definitions
cmd/
  main.go                  # Operator entry point
  console-proxy/           # Console proxy aggregated API server
pkg/
  aap/                     # AAP REST API client (public package)
  provisioning/            # ProvisioningProvider abstraction
internal/
  api/                     # Generated gRPC client (DO NOT EDIT)
  controller/              # Reconciliation logic
    {resource}_controller.go           # Provisioning controller
    {resource}_feedback_controller.go  # Feedback controller
  consoleproxy/            # Console proxy server implementation (auth, config, handlers)
  migrations/              # Data migrations (e.g., migrate_subnetrefs.go)
helpers/                   # Utility functions (at project root, not under internal/)
config/
  crd/                     # Generated CRD manifests (DO NOT EDIT)
  rbac/                    # Generated RBAC rules
  samples/                 # Example CRs and config Secret
  testing/                 # Testing configurations
    default/               # Default testing kustomization
    console-proxy/         # Console proxy testing kustomization
charts/
  operator/                # Helm chart for operator
  operator-crds/           # Helm chart for CRDs
test/integration/          # Integration tests (console_proxy_test.go, integration_suite_test.go, networking_test.go)
hack/sync-helm-crds.py     # Script invoked by `make helm-crds` to sync CRDs to Helm charts
```

## Testing

- **Unit tests**: Ginkgo + Gomega with `envtest` (real etcd + kube-apiserver)
- **Integration**: `make test-kustomize` (manifest validation) + `make test-smoke` (kind cluster)
- **Go integration tests**: `test/integration/` (console_proxy_test.go, integration_suite_test.go, networking_test.go) — run against an already-running kind cluster via `make test-integration-kind` (`go test ./test/integration/ -v -ginkgo.v`)
- **E2E tests**: pytest-based, live in the separate `osac-test-infra` repo; triggered from this repo via `.github/workflows/e2e-vmaas-full-install.yml`
- Kind cluster defaults to `osac` (`KIND_CLUSTER_NAME` in Makefile line 81), but smoke tests create `osac-test`
- Clean up: `kind delete cluster --name osac-test`
- `test-kustomize` catches missing files in kustomization.yaml — always run before committing manifest changes

## Code Quality

- **golangci-lint** (see `Makefile` for pinned version) configured in `.golangci.yml`: dupl, errcheck, ginkgolinter, goconst, gocyclo, govet, ineffassign, lll, misspell, prealloc, revive, staticcheck, unconvert, unused
- Formatters: gofmt, goimports
- Pre-commit hooks (both files share trailing-whitespace, check-merge-conflict, end-of-file-fixer, yamllint --strict excluding `config/`, detect-private-key):
  - `.pre-commit-config.yaml` — local default (used by `pre-commit run --all-files`), also runs the `golangci-lint` hook
  - `.pre-commit-config-ci.yaml` — used in CI (`pre-commit/action` with `-c .pre-commit-config-ci.yaml`); golangci-lint runs as a separate CI job instead
- Run manually: `pre-commit run --all-files`
- **CRITICAL**: Always run `make lint` before committing — CI enforces strict linting

## Automation Hooks

Hooks are configured in `.claude/settings.json` and run automatically during agent sessions:

- **CRD type changes** (`PostToolUse`): When `*_types.go` is edited, `make manifests generate` runs automatically.
- **Go module changes** (`PostToolUse`): When `go.mod` is edited, `go mod tidy` runs automatically.
- **Pre-PR** (`PreToolUse`): `make fmt` (fails if files changed — commit fixes first), `make lint`, and `make test` run before `gh pr create`.

## Controller Development Patterns

### Dual-Controller Pattern

Each resource has two controllers:

```text
Resource Controller                    Feedback Controller
- Provisions via AAP                   - Syncs CR state → fulfillment-service
- Manages finalizers and deletion       - Converts K8s Phase → proto State
- Updates Phase, Conditions, etc.       - Sends Signal RPC on deletion
```

| File Pattern | Purpose |
|---|---|
| `{resource}_controller.go` | Provisioning, lifecycle |
| `{resource}_feedback_controller.go` | Sync to fulfillment-service |

**Why?** Resource controller handles infra lifecycle; feedback controller handles fulfillment-service integration. Operator works even if fulfillment-service is down.

### Reconciliation Pattern

```go
func (r *SubnetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    subnet := &v1alpha1.Subnet{}
    if err := r.Client.Get(ctx, req.NamespacedName, subnet); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Skip if unmanaged
    if subnet.Annotations[osacManagementStateAnnotation] == ManagementStateUnmanaged {
        return ctrl.Result{}, nil
    }

    // Save old status, compare after reconcile, update only if changed
    oldStatus := subnet.Status.DeepCopy()
    var res ctrl.Result
    var err error
    if subnet.DeletionTimestamp.IsZero() {
        res, err = r.handleUpdate(ctx, subnet)
    } else {
        res, err = r.handleDelete(ctx, subnet)
    }
    if !equality.Semantic.DeepEqual(subnet.Status, *oldStatus) {
        if err := r.Status().Update(ctx, subnet); err != nil {
            return res, err
        }
    }
    return res, err
}
```

Key rules:
- Always `client.IgnoreNotFound(err)` — resource may be deleted between list and get
- Save old status and compare before updating (avoids reconciliation loops)
- Separate `handleUpdate` and `handleDelete`

### Finalizer Management

- Each controller has its own finalizer: `osac.openshift.io/{resource}-finalizer`
- Feedback controllers: `osac.openshift.io/{resource}-feedback-finalizer`
- Remove finalizer only after cleanup fully completes
- Multiple finalizers coexist — each controller manages its own

### AAP Integration

Networking controllers use `provisioning.RunProvisioningLifecycle()` with callbacks:
- `OnBeforeProvision` — validate preconditions
- `OnSuccess` — extract outputs, set Phase to Ready
- `OnFailed` — set Phase to Failed

Template naming: `{prefix}-{action}-{kind}` (e.g., `osac-create-subnet`).
Prefix configurable via `OSAC_AAP_TEMPLATE_PREFIX`.

### Feedback Controller

Syncs K8s Phase → proto State:

| K8s Phase | Proto State |
|---|---|
| Progressing | PENDING |
| Ready | READY |
| Failed | FAILED |
| Deleting | DELETING |
| (deletion failed) | DELETE_FAILED |

Handle NotFound during deletion gracefully — fulfillment-service may archive before finalizer removed.

### CRD Type Definition

```go
type SubnetSpec struct {
    // +kubebuilder:validation:Required
    VirtualNetwork string `json:"virtualNetwork"`
    // +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
    IPv4CIDR string `json:"ipv4CIDR,omitempty"`
}
type SubnetStatus struct {
    Phase            SubnetPhase `json:"phase,omitempty"`
    BackendNetworkID string      `json:"backendNetworkID,omitempty"`
    Conditions       []Condition `json:"conditions,omitempty"`
    JobHistory       []JobRecord `json:"jobHistory,omitempty"`
}
// +kubebuilder:validation:Enum=Progressing;Ready;Failed;Deleting
type SubnetPhase string
```

Common kubebuilder markers: `+kubebuilder:validation:Required`, `Pattern`, `Enum`, `Minimum`, `+kubebuilder:printcolumn`.

## Common Pitfalls

1. **Forgetting to regenerate after CRD changes** — Always run `make manifests generate` after modifying `api/v1alpha1/*.go`. CRD YAML in `config/crd/` and DeepCopy in `zz_generated.deepcopy.go` are generated — never edit directly.
2. **Status update loops** — Save `oldStatus := subnet.Status.DeepCopy()` before reconciliation. Use `equality.Semantic.DeepEqual` to compare. Only call `r.Status().Update()` if changed.
3. **Finalizer removal timing** — Remove finalizer only after cleanup fully completes. For feedback controllers: after syncing deletion state. Handle errors during cleanup — retry on next reconcile, don't remove prematurely.
4. **AAP job polling inefficiency** — Use `StatusPollInterval` (default: 30s). Only poll active jobs. Return `ctrl.Result{RequeueAfter: interval}` for delayed reconciliation.
5. **NotFound errors during deletion** — Feedback controllers may see NotFound if fulfillment-service archives before finalizer removed. Handle gracefully: check `DeletionTimestamp` + `codes.NotFound`, then remove finalizer.
6. **Parent resource lookup failures** — If parent not found (e.g., Subnet → VirtualNetwork), requeue with delay. Don't set Phase to Failed — parent may be created soon. Use conditions for transient errors.
7. **Vendor directory confusion** — Operator uses Go modules, not vendoring. Delete `vendor/` if it exists. Use `go mod tidy`.
8. **Integration test failures** — Update `kustomization.yaml` after renaming/deleting manifests. Clean up test clusters: `kind delete cluster --name osac`. Run `make test-kustomize` before committing manifest changes.
9. **gRPC client version mismatches** — Update module version in `buf.gen.yaml`, never edit proto files directly. Run `buf generate` to regenerate `internal/api/`. Commit generated files.
10. **AAP template naming** — Template names: `{prefix}-{action}-{resource-kind}` (e.g., `osac-create-subnet`). Per-resource overrides (e.g., `OSAC_CLUSTER_AAP_PROVISION_TEMPLATE`) take precedence.

## Common Tasks

### Adding a New CRD

1. `kubebuilder create api --group osac.openshift.io --version v1alpha1 --kind MyResource`
2. Define types in `api/v1alpha1/myresource_types.go`
3. `make manifests generate`
4. Create controller in `internal/controller/myresource_controller.go`
5. Register controller in `cmd/main.go`

### Adding a Field to Existing CRD

1. Add field to `api/v1alpha1/{resource}_types.go`
2. `make manifests generate`
3. Update controller logic in `internal/controller/{resource}_controller.go`
4. Update feedback controller if field needs sync to fulfillment-service

### Cross-Repo Change Order

1. **fulfillment-service**: Update proto definitions, regenerate
2. **osac-operator**: Update CRD types, controller logic, `buf generate`
3. **osac-aap**: Update Ansible roles/playbooks
4. **osac-installer**: Update submodules, add RBAC if needed

### RBAC Changes

Add markers to controller, then `make manifests`:
```go
//+kubebuilder:rbac:groups=osac.openshift.io,resources=myresources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=osac.openshift.io,resources=myresources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=osac.openshift.io,resources=myresources/finalizers,verbs=update
```
Update osac-installer hub-access Role if fulfillment controller needs access.

### Debugging Controller Issues

```bash
kubectl describe subnet my-subnet -n osac-networking
kubectl logs -n osac-system deployment/osac-operator-controller-manager -f
# Enable debug: add --zap-log-level=debug to config/manager/manager.yaml args
```

## Configuration Reference

Config via environment variables from a Secret (see `config/samples/osac-config-secret.yaml`).

### AAP Provisioning

- `OSAC_AAP_URL` — AAP server URL (required)
- `OSAC_AAP_TOKEN` — authentication token (required)
- `OSAC_AAP_TEMPLATE_PREFIX` — template name prefix (default: `osac`)
- `OSAC_AAP_STATUS_POLL_INTERVAL` — job polling interval (default: 30s)
- `OSAC_AAP_INSECURE_SKIP_VERIFY` — skip TLS verification (default: false)

### Fulfillment Service gRPC

- `OSAC_FULFILLMENT_SERVER_ADDRESS` — gRPC server address
- `OSAC_FULFILLMENT_TOKEN_FILE` — path to auth token file

### Namespaces

- `OSAC_CLUSTER_ORDER_NAMESPACE`, `OSAC_COMPUTE_INSTANCE_NAMESPACE`
- `OSAC_TENANT_NAMESPACE`, `OSAC_NETWORKING_NAMESPACE`

### Controller Enable Flags

- `OSAC_ENABLE_CLUSTER_CONTROLLER` / `--enable-cluster-controller`
- `OSAC_ENABLE_COMPUTE_INSTANCE_CONTROLLER` / `--enable-compute-instance-controller`
- `OSAC_ENABLE_TENANT_CONTROLLER` / `--enable-tenant-controller`
- `OSAC_ENABLE_NETWORKING_CONTROLLER` / `--enable-networking-controller`

If none set, all controllers run. If any set, only flagged controllers run.

## PR Checklist

- [ ] `make manifests generate` if types changed
- [ ] `make helm-crds` run after CRD regeneration
- [ ] `make lint` passes (enforced by CI)
- [ ] `make test` passes
- [ ] `make test-kustomize` passes (catches missing files)
- [ ] CRD changes tested against a cluster
- [ ] Cross-repo dependencies documented in PR description
- [ ] PR title includes Jira key (e.g., "OSAC-12345: fix subnet race")
- [ ] Commits signed off (`git commit -s`) and include AI attribution if applicable

## CI Workflows

- **build-image.yaml**: Runs `make test`, `make test-kustomize`, `make test-smoke`, then builds and pushes container + manifest container
- **check-pull-request.yaml**: Validates `buf generate` output unchanged (ensures gRPC client is up-to-date)
- **helm-lint.yaml**: Checks CRD sync (`hack/sync-helm-crds.py`) and lints Helm charts
- **e2e-vmaas-full-install.yml**: E2E tests in VMaaS environment

## Security

- **Containers**: UBI10 base images (see `Containerfile` for pinned tags), non-root USER 1001
- **Secrets**: AAP credentials and fulfillment-service tokens via Secret (`config/samples/osac-config-secret.yaml`)
- **TLS**: Console proxy uses `OSAC_CONSOLE_PROXY_TLS_CERT_FILE` and `OSAC_CONSOLE_PROXY_TLS_KEY_FILE`
- **gRPC auth**: Token file via `OSAC_FULFILLMENT_TOKEN_FILE`
- **AAP TLS verification**: Configurable via `OSAC_AAP_INSECURE_SKIP_VERIFY` (defaults false)
- **Pre-commit**: `detect-private-key` hook prevents credential leaks

## Links

- [Kubebuilder Book](https://book.kubebuilder.io/)
- [controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [OSAC Project](https://github.com/osac-project)
