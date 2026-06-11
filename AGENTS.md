# AGENTS.md

## Project Overview

OSAC operator is a Kubernetes operator that reconciles infrastructure resources for the [OSAC](https://github.com/osac-project) project. It integrates with the [fulfillment service](https://github.com/osac-project/fulfillment-service/) and Ansible Automation Platform to provision OpenShift clusters and compute instances with networking.

### Resources Managed

- **ClusterOrder** (`cord`) — OpenShift clusters via Hosted Control Planes
- **ComputeInstance** (`ci`) — virtual machines via KubeVirt
- **Tenant** — namespace and OVN-Kubernetes UserDefinedNetwork for isolation
- **VirtualNetwork** (`vnet`) — cloud VPC with IPv4/IPv6 CIDR blocks
- **Subnet** (`subnet`) — subnet within a VirtualNetwork
- **SecurityGroup** (`sg`) — network security rules
- **PublicIPPool** (`pippool`) — public IP pool for MetalLB

## Critical Rules

- **Always `make manifests generate`** after modifying CRD types in `api/v1alpha1/*_types.go`
- **Always `make helm-crds`** after regenerating CRDs (or run `make check-helm-crds` to verify sync)
- **Never edit** `config/crd/`, `zz_generated.deepcopy.go`, or `internal/api/` — all generated
- **Always `buf generate`** after updating the module version in `buf.gen.yaml`
- **Commit message format**: `MGMT-XXXXX: description of change`
- **Always `make lint`** after changing any Go code — fix all issues before proceeding
- Run `make lint test` before committing

## Development Commands

```bash
make build                    # Build (runs tests first)
make test                     # Unit tests only
make test-integration         # All tests including integration
make test-kustomize           # Kustomize validation
make test-smoke               # Smoke tests in kind cluster
make lint                     # golangci-lint
make fmt                      # go fmt
make vet                      # go vet

make manifests                # Generate CRD manifests + RBAC
make generate                 # Generate DeepCopy
buf generate                  # Generate gRPC client code

make install                  # Install CRDs into cluster
make run                      # Run controller locally
make uninstall                # Remove CRDs

make image-build IMG=<r>/osac-operator:tag
make image-push IMG=<r>/osac-operator:tag
make deploy IMG=<r>/osac-operator:tag
make undeploy
```

## Architecture

### Dual-Controller Pattern

Each resource has a **resource controller** (provisions via AAP, manages finalizers) and a **feedback controller** (syncs state to fulfillment-service via gRPC). See `.claude/rules/controller-patterns.md` for reconciliation, finalizer, and AAP integration patterns.

### Provisioning

All controllers use direct AAP REST API integration via the `ProvisioningProvider` interface (`pkg/provisioning/provider.go` and `pkg/aap/client.go`).

### Multi-cluster

Hub cluster runs the operator; remote cluster hosts Tenant/ComputeInstance resources (via `OSAC_REMOTE_CLUSTER_KUBECONFIG`).

### gRPC Client

Consumes private fulfillment-service API. Generated from Buf Schema Registry module pinned in `buf.gen.yaml`. Update version there and run `buf generate` when API changes.

## File Organization

```text
api/v1alpha1/              # CRD type definitions
cmd/main.go                # Operator entry point
pkg/
  aap/                     # AAP REST API client (public package)
  provisioning/            # ProvisioningProvider abstraction
internal/
  api/                     # Generated gRPC client (DO NOT EDIT)
  controller/              # Reconciliation logic
    {resource}_controller.go           # Provisioning controller
    {resource}_feedback_controller.go  # Feedback controller
  helpers/                 # Utility functions
config/
  crd/                     # Generated CRD manifests (DO NOT EDIT)
  rbac/                    # Generated RBAC rules
  samples/                 # Example CRs and config Secret
test/e2e/                  # End-to-end tests
```

## Testing

- **Unit tests**: Ginkgo + Gomega with `envtest` (real etcd + kube-apiserver)
- **Integration**: `make test-kustomize` (manifest validation) + `make test-smoke` (kind cluster)
- Kind cluster named `osac` (configurable via `KIND_CLUSTER_NAME`)
- Clean up: `kind delete cluster --name osac`

## Code Quality

- Pre-commit hooks in `.pre-commit-config.yaml`: whitespace, yamllint, golangci-lint
- Pre-push hooks: kustomize-build, check-generated (manifest and code freshness)
- Linter config in `.golangci.yml`
- Run manually: `pre-commit run --all-files`

### Hook Installation

Two hook types must be installed separately:

```bash
pre-commit install                        # commit-stage hooks (lint, whitespace, etc.)
pre-commit install --hook-type pre-push   # push-stage hooks (kustomize-build, check-generated)
```

Or use `make install-hooks` to install both at once.

## Automation Hooks

Hooks are configured in `.claude/settings.json` and run automatically during agent sessions:

- **CRD type changes** (`PostToolUse`): When `*_types.go` is edited, `make manifests generate` runs automatically.
- **Go module changes** (`PostToolUse`): When `go.mod` is edited, `go mod tidy` runs automatically.
- **Pre-PR** (`PreToolUse`): `make fmt` (fails if files changed — commit fixes first), `make lint`, and `make test` run before `gh pr create`.

## Detailed Rules (auto-loaded from `.claude/rules/`)

- **`controller-patterns.md`** — Dual-controller, reconciliation, finalizer, AAP, feedback, CRD type patterns
- **`common-pitfalls.md`** — 10 common issues: regen, status loops, finalizers, AAP polling, NotFound, etc.
- **`common-tasks.md`** — Adding CRDs/fields, cross-repo change order, RBAC, debugging
- **`configuration.md`** — Environment variables for AAP, gRPC, namespaces, controller flags

## PR Checklist

- [ ] `make manifests generate` if types changed
- [ ] `make lint` passes
- [ ] `make test` passes
- [ ] CRD changes tested against a cluster
- [ ] Cross-repo dependencies documented in PR description

## Links

- [Kubebuilder Book](https://book.kubebuilder.io/)
- [controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [OSAC Project](https://github.com/osac-project)
