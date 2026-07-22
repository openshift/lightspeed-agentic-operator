# System Overview

The lightspeed-agentic-operator is a Kubernetes operator that watches `AgenticRun` custom resources and drives them through a multi-phase AI-assisted workflow. Each phase (analysis, execution, verification) invokes an LLM-backed agent running in an ephemeral sandbox pod, with human approval gates between phases. The operator manages the full lifecycle: CRD reconciliation, sandbox provisioning, RBAC materialization, result recording, and garbage collection.

## Behavioral Rules

### System Role

1. The operator MUST watch `AgenticRun` resources across all namespaces and reconcile them through the run lifecycle defined in `run-lifecycle.md`.
2. The operator MUST run as a single-replica controller-runtime manager in a designated operator namespace, not in workload namespaces.
3. The operator MUST register health (`/healthz`) and readiness (`/readyz`) probes.
4. The operator MUST accept its namespace via `--namespace` flag or `POD_NAMESPACE` environment variable; it MUST exit if neither is provided.

### Component Inventory

5. The system comprises three functional components:
   - **AgenticRun controller** — reconciles `AgenticRun` CRs through the workflow state machine.
   - **CLI plugin** (`oc-agentic`) — provides `oc agentic run` commands for run CRUD, approval, watch, and log streaming.
   - **API types** (`api/v1alpha1`) — CRD type definitions published as a separate Go module for downstream consumers.
5a. [PLANNED: OLS-3236] The **console plugin** deployment is migrated to the lightspeed-operator for full reconciliation lifecycle management. The agentic-operator no longer deploys any console plugins. The **alerts adapter** deployment is also managed by the lightspeed-operator.
6. The run controller runs in the operator binary via `controller.Setup()`. [PLANNED: OLS-3236] The console plugin `RunnableFunc` and `controller/console/` package are removed.
7. The CLI is a separate binary (`cmd/oc-agentic`) that communicates directly with the Kubernetes API server.

### External Dependencies

8. The operator MUST interact with the Kubernetes API server for all CR CRUD, status updates, and RBAC management.
9. When `--sandbox-mode=sandbox-claim`, the operator MUST interact with the Sandbox API (`extensions.agents.x-k8s.io/v1alpha1` `SandboxClaim`, `agents.x-k8s.io/v1alpha1` `Sandbox`) to provision ephemeral agent workloads. In the default `bare-pod` mode, the operator creates Pods directly and does not depend on Sandbox API CRDs.
10. The operator MUST resolve `Agent` CRs and their referenced `LLMProvider` CRs to determine model configuration and credentials for each workflow step.
11. The operator MUST call the sandbox agent's `POST /v1/agent/run` HTTP endpoint for each workflow step (analysis, execution, verification, escalation).
12. [PLANNED: OLS-3236] Console plugin deployment is migrated to the lightspeed-operator. The agentic-operator no longer interacts with OpenShift Console APIs for plugin deployment.

### Dual-Module Structure

13. The `api/` directory MUST be a separate Go module (`github.com/openshift/lightspeed-agentic-operator/api`) so downstream projects can depend on CRD types without importing the full operator.
14. The root `go.mod` MUST use a `replace` directive pointing `api` to `./api` for local development.

### Multi-Tenancy

15. Runs are namespace-scoped; the operator reconciles runs across all namespaces.
16. Cluster-scoped resources (`Agent`, `LLMProvider`, `ApprovalPolicy`) are shared across all tenants.
17. Sandbox pods and claims run in the operator namespace, not in tenant namespaces.

## Configuration Surface

| Field/Flag | Type | Default | Description |
|---|---|---|---|
| `--namespace` / `POD_NAMESPACE` | string | (required) | Operator install namespace |
| `--metrics-bind-address` | string | `:8080` | Metrics endpoint bind address |
| `--health-probe-bind-address` | string | `:8081` | Health probe endpoint bind address |
| `--agentic-sandbox-image` | string | `""` | Sandbox container image (default provided by lightspeed-operator constants, overridable) |
| `--sandbox-mode` | string | `bare-pod` | Sandbox mode: `bare-pod` (direct Pod management) or `sandbox-claim` (Agent Sandbox API) |

## Constraints

- The operator assumes it is the sole controller for `agentic.openshift.io/v1alpha1` resources; running multiple replicas without leader election would cause conflicts.
- Sandbox provisioning via `SandboxClaim` depends on the Sandbox API CRDs being installed in the cluster; this dependency only applies when `--sandbox-mode=sandbox-claim`. The default `bare-pod` mode has no external CRD dependency.
- The operator requires OpenShift APIs for console plugin deployment; running on vanilla Kubernetes skips console integration.

## Planned Changes

| Ticket | Summary |
|---|---|
| OLS-2957 | Sandbox template management UX and CRD ergonomics may change operator/template coupling |
| OLS-2940 | Autonomous workflow CRD migrations may rename or reshape `v1alpha1` fields |
| OLS-3236 | Remove `controller/console/` package and `--agentic-console-image` flag. Console plugin and alerts adapter deployment moves to lightspeed-operator. |
| OLS-3572 | Inter-operator config handoff: read base PodSpec from `lightspeed-sandbox-config` ConfigMap (produced by lightspeed-operator). Deprecate `--sandbox-mode` flag in favor of ConfigMap-provided mode. Fail hard if ConfigMap missing. |
