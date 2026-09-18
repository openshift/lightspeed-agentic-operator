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
| `controller/agenticrun/` | AgenticRun reconciler, approval, sandbox wiring |
| `controller/agent/` | Agent Ready status (LLMProvider + Secret) |
| `controller/agenticolsconfig/` | AgenticOLSConfig reconciler |
| `controller/sandbox/` | Sandbox pod management |
| `cli/` | `oc-agentic` plugin |
| `config/crd/bases/` | Generated CRD YAML (regen: **`README.md`** → **`make manifests`**) |
| `config/rbac/` | SA, bindings, generated `role.yaml` |
| `config/manager/`, `config/default/` | In-cluster Deployment kustomize |
| `examples/setup/` | Day-0 YAML (agents, policies, runs) |
| `test/agent/` | Mock agent HTTP server (`POST /v1/agent/run`), image Makefile, `cmd/schemadump` |
| `test/agent/sandboxtemplate/` | Kustomize base `SandboxTemplate` for in-cluster mock |
| `test/e2e/` | Build tag **`e2e`**: black-box tests against live cluster + running operator (`make test-e2e`) |

## Run lifecycle phases

Derived from conditions via **`DerivePhase()`** — never stored on the spec:

```
Pending → Analyzing → Proposed → Executing → Verifying → Completed
                                                       → Failed
                                                       → Denied
                                                       → Escalated
```

- **Proposed** — analysis done, awaiting execution approval (Analyzed=True, no Executed condition).
- **Executing** — execution in flight (Executed=Unknown). Verification failure escalates; it never returns to Executing.

## Commands

```bash
make build         # Build the operator binary (bin/manager)
make test          # Unit tests (always use make, not go test directly)
make test-e2e      # E2E tests (requires cluster + running operator)
make manifests     # Regenerate CRD YAML and RBAC ClusterRole
make api-lint      # Kube API linter on api/ types (golangci-lint)
make fmt            # go fmt
make vet            # go vet
```

## Code conventions

- Create-only idempotency: **`Create`** + handle **`AlreadyExists`** (not Get-then-Create).
- Owner refs on children: **`Controller: true`**, **`BlockOwnerDeletion: true`** for **`Owns()`** watches.
- Errors: **`const ErrFoo = "…"`**, wrap with **`fmt.Errorf("%s: %w", …)`**.
- Status: **`client.MergeFrom(base)`** patch pattern.

## Git and PR Workflow

### Commit Messages
- Start with the Jira ticket reference: `OLS-XXXX description`
- Keep the first line under 72 characters
- Use imperative mood

### Pull Requests
This repo uses a **fork-based workflow**:

1. **Push to your fork**, not to `origin` (openshift/lightspeed-agentic-operator)
2. **Create the PR** against `origin/main` using your fork's branch:
   ```bash
   git push <your-fork-remote> <branch>
   gh pr create --repo openshift/lightspeed-agentic-operator --head <your-github-user>:<branch> --base main
   ```
3. **PR title** must start with the Jira reference: `OLS-XXXX description`
4. **Squash commits** before pushing
