# Agentic Operator

Kubernetes controller for the agentic proposal workflow. CRDs: Proposal, ProposalApproval, Agent, LLMProvider, ApprovalPolicy under `agentic.openshift.io/v1alpha1`.

## Module Structure

Two Go modules in this repo:
- `go.mod` — main module (`github.com/openshift/lightspeed-agentic-operator`), contains controller code
- `api/go.mod` — separate API module (`github.com/openshift/lightspeed-agentic-operator/api`), contains CRD types only so downstream consumers can depend on just the types

The main `go.mod` uses `replace github.com/openshift/lightspeed-agentic-operator/api => ./api` for local development.

## CRD Generation — IMPORTANT

CRDs are generated and stored in **lightspeed-operator**, not here. After changing any types or kubebuilder markers in `api/v1alpha1/`:

```bash
cd ../lightspeed-operator
make manifests-agentic
```

This runs `controller-gen` with `paths=github.com/openshift/lightspeed-agentic-operator/api/...` and outputs to `lightspeed-operator/config/crd/bases/`. Do NOT edit CRD YAML files by hand — always regenerate.

CRD files live at `../lightspeed-operator/config/crd/bases/agentic.openshift.io_*.yaml`. There are also local copies at `config/crd/bases/` (Proposal, Agent, LLMProvider only — ProposalApproval and ApprovalPolicy are operator-side only).

## Testing

Standard Go tests (not Ginkgo). Run from this repo:

```bash
go test ./... -count=1                                    # all tests
go test ./controller/proposal/... -v                      # proposal controller tests
go test ./api/... -v                                      # API/DerivePhase tests
go test ./cli/... -v                                      # CLI tests
```

## API Linting

Kube API Linter checks API types against Kubernetes conventions. Build the custom linter binary once, then run against the API module:

```bash
golangci-lint custom                                       # builds bin/golangci-lint-kube-api-linter
cd api && GOWORK=off ../bin/golangci-lint-kube-api-linter run --config ../.golangci-kal.yml ./...
```

Config: `.golangci-kal.yml`. Custom binary spec: `.custom-gcl.yml`. Must run from the `api/` directory with `GOWORK=off` due to the separate module layout.

## Key Directories

- `api/v1alpha1/` — CRD type definitions, DerivePhase, constants
- `controller/proposal/` — Proposal reconciler, handlers, approval logic, RBAC, sandbox templates
- `controller/console/` — Agentic console plugin deployment
- `cli/` — `oc-agentic` CLI plugin (list, get, watch, approve, logs)
- `config/crd/bases/` — Local CRD copies (subset; canonical source is lightspeed-operator)
- `examples/setup/` — Day 0 setup YAMLs (agents, approval policy, proposals)

## CEL Validation

kubebuilder `+kubebuilder:validation:XValidation` markers generate CEL rules in CRDs.

**Gotcha**: `omitempty` fields don't exist in the stored object when unset. CEL expressions must use `has(field)` before accessing `omitempty` fields, otherwise the rule fails with "no such key". Example: `has(old.denied) && old.denied` instead of `old.denied`.

**Gotcha**: Struct-level XValidation causes double-quoting in generated YAML. Put XValidation on the **field** (in the parent struct), not on the type itself.

## Proposal Lifecycle Phases

Phases are derived from conditions via `DerivePhase()` — never stored directly:

```
Pending → Analyzing → Proposed → Executing → Verifying → Completed
                                                       → Failed
                                                       → Denied
                                                       → Escalated
```

- `Proposed` = analysis done, awaiting execution approval (Analyzed=True, no Executed condition)
- `Executing` = execution in progress (Executed=Unknown) or retry (Verified=False/RetryingExecution)

## Code Conventions

- Create-only idempotency: `Create` + handle `AlreadyExists` (not Get-then-Create)
- Owner references on child resources must set `Controller: true` and `BlockOwnerDeletion: true` for `Owns()` watches to work
- Error constants: `const ErrFoo = "failed to ..."`, wrap with `fmt.Errorf("%s: %w", ...)`
- Status patches use `client.MergeFrom(base)` pattern
