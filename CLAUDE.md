# Agentic Operator — project guide

## Specs

All specifications live in `.ai/spec/`. Start with `.ai/spec/README.md` for project overview, reading order, and structure guide.

**What lives where:** **`agent.md`** (how agents should work), **`README.md`** (tests, **`make manifests`**, Makefile, cluster workflow, **`make api-lint`**, CEL / **`XValidation`** notes). This file is **architecture and conventions** for humans and agents editing the tree.

## Module layout

- **`go.mod`** — main module (`github.com/openshift/lightspeed-agentic-operator`): controller, CLI, etc.
- **`api/go.mod`** — API-only module so downstreams can depend on types without the operator. Root **`go.mod`** uses **`replace … => ./api`** for local dev.

## Key directories

| Path | Role |
|------|------|
| `api/v1alpha1/` | CRD types, `DerivePhase`, constants |
| `controller/proposal/` | Proposal reconciler, approval, sandbox wiring |
| `controller/console/` | Agentic console plugin deployment |
| `cli/` | `oc-agentic` plugin |
| `config/crd/bases/` | Generated CRD YAML (regen: **`README.md`** → **`make manifests`**) |
| `config/rbac/` | SA, bindings, generated `role.yaml` |
| `config/manager/`, `config/default/` | In-cluster Deployment kustomize |
| `examples/setup/` | Day-0 YAML (agents, policies, proposals) |
| `test/agent/` | Mock agent HTTP server (`POST /v1/agent/run`), image Makefile, `cmd/schemadump` |
| `test/agent/sandboxtemplate/` | Kustomize base `SandboxTemplate` for in-cluster mock |
| `test/e2e/` | Build tag **`e2e`**: black-box tests against live cluster + running operator (`make test-e2e`) |

## Proposal lifecycle phases

Derived from conditions via **`DerivePhase()`** — never stored on the spec:

```
Pending → Analyzing → Proposed → Executing → Verifying → Completed
                                                       → Failed
                                                       → Denied
                                                       → Escalated
```

- **Proposed** — analysis done, awaiting execution approval (Analyzed=True, no Executed condition).
- **Executing** — in flight (Executed=Unknown) or retry (Verified=False / RetryingExecution).

## Code conventions

- Create-only idempotency: **`Create`** + handle **`AlreadyExists`** (not Get-then-Create).
- Owner refs on children: **`Controller: true`**, **`BlockOwnerDeletion: true`** for **`Owns()`** watches.
- Errors: **`const ErrFoo = "…"`**, wrap with **`fmt.Errorf("%s: %w", …)`**.
- Status: **`client.MergeFrom(base)`** patch pattern.
