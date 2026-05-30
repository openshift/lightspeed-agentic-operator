# Project Structure

## Module Map

| File/Directory | Key Symbols | Responsibility |
|---|---|---|
| `go.mod` | module `github.com/openshift/lightspeed-agentic-operator` | Root module: controller, CLI, build targets |
| `api/go.mod` | module `github.com/openshift/lightspeed-agentic-operator/api` | Standalone API module for downstream consumers |
| `api/v1alpha1/` | `Proposal`, `Agent`, `LLMProvider`, `ApprovalPolicy`, `ProposalApproval`, result types, `DerivePhase` | CRD type definitions, phase derivation, CEL markers, deepcopy |
| `cmd/main.go` | `main`, `scheme` | Operator binary entry point |
| `cmd/oc-agentic/main.go` | `main` | CLI binary entry point |
| `controller/setup.go` | `Setup`, `Options` | Wires proposal controller + console plugin into manager |
| `controller/proposal/` | `ProposalReconciler`, `SandboxAgentCaller`, `SandboxManager` | Proposal reconciler, sandbox management, agent HTTP client, RBAC, results, templates |
| `controller/console/` | `EnsureAgenticConsole`, `AgenticConsoleConfig` | Console plugin deployment (Deployment, Service, ConfigMap, ConsolePlugin CR) |
| `cli/` | `NewRootCmd` | CLI root command |
| `cli/proposal/` | `CreateOptions`, `ListOptions`, `GetOptions`, `ApproveOptions`, `DenyOptions`, `WatchOptions`, `LogsOptions`, `DeleteOptions` | CLI subcommands for proposal lifecycle operations |
| `config/crd/bases/` | Generated YAML | CRD manifests (regenerate with `make manifests`) |
| `config/rbac/` | Generated YAML | RBAC manifests: ServiceAccount, ClusterRole, bindings |
| `config/manager/` | Kustomize patches | Operator Deployment kustomize overlays |
| `config/default/` | Kustomize base | Default kustomize composition for full deployment |
| `examples/setup/` | YAML fixtures | Day-0 resources: Agent, LLMProvider, ApprovalPolicy, sample Proposals |
| `test/agent/` | Mock HTTP server | `POST /v1/agent/run` mock for integration testing |
| `test/agent/sandboxtemplate/` | Kustomize base | Base `SandboxTemplate` for in-cluster mock agent |
| `test/e2e/` | Build tag `e2e` | Black-box tests against live cluster + running operator |
| `docs/` | Markdown | Design documents and meeting notes |

## Key Entry Points

**Operator binary** (`cmd/main.go`):
- Parses flags (`--namespace`, `--metrics-bind-address`, `--health-probe-bind-address`, `--agentic-console-image`, `--agentic-sandbox-image`)
- Builds controller-runtime `Manager` with scheme (core + `agenticv1alpha1` + OpenShift console/operator)
- Calls `controller.Setup(mgr, opts)` which registers proposal controller and console runnable
- Starts manager with signal handler

**CLI binary** (`cmd/oc-agentic/main.go`):
- Builds `genericclioptions.IOStreams` from stdin/stdout/stderr
- Executes `cli.NewRootCmd(streams)` (Cobra)

**Controller setup** (`controller/setup.go`):
- Creates `SandboxManager` and `SandboxAgentCaller` with dependency injection
- Registers `ProposalReconciler` via `SetupWithManager`
- Registers `EnsureAgenticConsole` as a `RunnableFunc`

## Naming Conventions

- **CRD type files**: `<kind>_types.go` (e.g., `proposal_types.go`, `agent_types.go`)
- **Controller files**: functional decomposition by concern (`reconciler.go`, `handlers.go`, `approval.go`, `rbac.go`, `sandbox.go`, etc.)
- **Test files**: `<concern>_test.go` adjacent to source
- **Embedded templates**: `controller/proposal/templates/*.tmpl` — Go text templates for agent query payloads
- **API package**: `api/v1alpha1/` — follows kubebuilder conventions for API group/version layout
- **Config manifests**: `config/<concern>/` — kustomize directory structure per kubebuilder scaffold
- **Generated code**: `zz_generated.deepcopy.go` (controller-gen), `config/crd/bases/` (CRD YAML)
- **Build artifacts**: `bin/` (gitignored)

## Build System

The project uses a `Makefile` with standard kubebuilder targets:

| Target | Purpose |
|---|---|
| `make manifests` | Regenerate CRD YAML and RBAC from Go markers |
| `make generate` | Run controller-gen for deepcopy |
| `make api-lint` | Lint API types with golangci-lint + custom linters |
| `make test` | Unit tests (excludes `e2e` build tag) |
| `make test-e2e` | E2E tests against live cluster |
| `make build` | Build operator binary |
| `make docker-build` | Build container image |
