# Project Structure

## Module Map

| File/Directory | Key Symbols | Responsibility |
|---|---|---|
| `go.mod` | module `github.com/openshift/lightspeed-agentic-operator` | Root module: controller, CLI, build targets |
| `api/go.mod` | module `github.com/openshift/lightspeed-agentic-operator/api` | Standalone API module for downstream consumers |
| `api/v1alpha1/` | `AgenticRun`, `Agent`, `LLMProvider`, `ApprovalPolicy`, `AgenticRunApproval`, result types, `DerivePhase` | CRD type definitions, phase derivation, CEL markers, deepcopy |
| `cmd/main.go` | `main`, `scheme` | Operator binary entry point |
| `cmd/oc-agentic/main.go` | `main` | CLI binary entry point |
| `controller/agenticrun/` | `AgenticRunReconciler`, `SandboxAgentCaller`, `SandboxManager`, `SandboxLifecycle`, `PodSpecBuilder`, `PodEventHandler`, `TimeoutHandler` | AgenticRun reconciler, unified sandbox management (SA, RBAC, ConfigMap, pod), unified pod event handler (pod_handler.go handles both bare-pod labels and sandbox-claim ownerRef chain), mode-dispatching timeout loop (timeout_handler.go), results |
| `controller/console/` | `EnsureAgenticConsole`, `AgenticConsoleConfig` | Console plugin deployment (Deployment, Service, ConfigMap, ConsolePlugin CR) |
| `controller/sandbox/` | Legacy bootstrap helpers | SA creation inlined into `cmd/main.go` |
| `pkg/configuration/` | `Config`, `Cache`, `OnConfigMapChange` | ConfigMap-driven config cache (sandbox mode, PodSpec, OTEL, MCP) |
| `pkg/configwatch/` | `Watcher`, `TryLoad` | Generic ConfigMap watcher utility |
| `cli/` | `NewRootCmd` | CLI root command |
| `cli/run/` | `CreateOptions`, `ListOptions`, `GetOptions`, `ApproveOptions`, `DenyOptions`, `WatchOptions`, `LogsOptions`, `DeleteOptions` | CLI subcommands for run lifecycle operations |
| `config/crd/bases/` | Generated YAML | CRD manifests (regenerate with `make manifests`) |
| `config/rbac/` | Generated YAML | RBAC manifests: ServiceAccount, ClusterRole, bindings |
| `config/manager/` | Kustomize patches | Operator Deployment kustomize overlays |
| `config/default/` | Kustomize base | Default kustomize composition for full deployment |
| `examples/setup/` | YAML fixtures | Day-0 resources: Agent, LLMProvider, ApprovalPolicy, sample AgenticRuns |
| `test/agent/` | Mock HTTP server | `POST /v1/agent/run` mock for integration testing |
| `test/agent/sandboxtemplate/` | Kustomize base | Base `SandboxTemplate` for in-cluster mock agent |
| `test/e2e/` | Build tag `e2e` | Black-box tests against live cluster + running operator |
| `docs/` | Markdown | Design documents and meeting notes |

## Key Entry Points

**Operator binary** (`cmd/main.go`):
- Parses flags (`--namespace`, `--metrics-bind-address`, `--health-probe-bind-address`, `--agentic-console-image`)
- Creates `configuration.Cache` and registers ConfigMap watcher for `lightspeed-agentic-configuration`
- Wires `SandboxManager` → `SandboxAgentCaller` → `AgenticRunReconciler` directly (no `controller/setup.go`)
- Ensures `lightspeed-agent` ServiceAccount unconditionally (discovery seed for reader CRBs)
- Registers console plugin, health/readiness probes, and webhook
- Starts manager with signal handler

**CLI binary** (`cmd/oc-agentic/main.go`):
- Builds `genericclioptions.IOStreams` from stdin/stdout/stderr
- Executes `cli.NewRootCmd(streams)` (Cobra)

**Controller setup** (inlined in `cmd/main.go`):
- Creates `SandboxManager(client, cfgCache, namespace)` and `SandboxAgentCaller` with dependency injection
- Registers `AgenticRunReconciler` via `SetupWithManager`
- Registers `EnsureAgenticConsole` as a `RunnableFunc`
- Registers `lightspeed-agent` SA creation as a `RunnableFunc`

## Naming Conventions

- **CRD type files**: `<kind>_types.go` (e.g., `agenticrun_types.go`, `agent_types.go`)
- **Controller files**: functional decomposition by concern (`reconciler.go`, `handlers.go`, `approval.go`, `rbac.go`, `sandbox.go`, etc.)
- **Test files**: `<concern>_test.go` adjacent to source
- **Embedded templates**: `controller/agenticrun/templates/*.tmpl` — Go text templates for agent query payloads
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
