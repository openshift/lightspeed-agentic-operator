# System Overview

The lightspeed-agentic-operator is a Kubernetes operator that watches `AgenticRun` custom resources and drives them through a multi-phase AI-assisted workflow. Each phase (analysis, execution, verification) invokes an LLM-backed agent running in an ephemeral sandbox pod, with human approval gates between phases. The operator manages the full lifecycle: CRD reconciliation, sandbox provisioning, RBAC materialization, result recording, and garbage collection.

## Behavioral Rules

### System Role

1. The operator MUST watch `AgenticRun` resources across all namespaces and reconcile them through the run lifecycle defined in `run-lifecycle.md`.
2. The operator MUST run as a single-replica controller-runtime manager in a designated operator namespace, not in workload namespaces.
3. The operator MUST register health (`/healthz`) and readiness (`/readyz`) probes.
3a. Deployed managers MUST enable Lease-based leader election in the operator namespace, using a lock distinct from the classic operator. Only the leader reconciles resources; local development MAY disable election.
3b. Deployed metrics MUST use HTTPS on port 8443 with a service-ca serving certificate. A metrics Service and ServiceMonitor MUST enable cluster monitoring with CA and Service DNS verification. Metrics ingress MUST be restricted to cluster Prometheus pods in the `openshift-monitoring` namespace. The endpoint MUST require client mTLS trusted by the CA certificate in the `client-ca.crt` data key of the `metrics-client-ca` ConfigMap in the `openshift-monitoring` namespace; the ServiceMonitor MUST present the certificate and private key from Prometheus's `metrics-client-certs` Secret. Metrics MUST consume the effective TLS policy from the `tls-profile`, `tls-min-version`, and `tls-cipher-suites` data keys of the `lightspeed-agentic-configuration` ConfigMap, not resolve OLSConfig or APIServer policy themselves. Missing, deleted, or invalid policy MUST reject new metrics TLS handshakes without blocking manager startup or health probes. Valid policy updates MUST apply safely to new connections on all replicas, including standbys, independently of sandbox/telemetry parsing failures. Each handshake MUST see a complete policy, never partially updated settings. Existing connections are not forcibly closed. Missing/invalid monitoring client trust MUST fail secure startup; client-CA updates require a restart.
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
9. When `sandbox-mode=sandbox-claim` (from `lightspeed-agentic-configuration` ConfigMap), the operator MUST interact with the Sandbox API (`extensions.agents.x-k8s.io/v1alpha1` `SandboxClaim`, `agents.x-k8s.io/v1alpha1` `Sandbox`) to provision ephemeral agent workloads. In the default `bare-pod` mode, the operator creates Pods directly and does not depend on Sandbox API CRDs.
10. The operator MUST resolve `Agent` CRs and their referenced `LLMProvider` CRs to determine model configuration and credentials for each workflow step.
11. For each workflow step (analysis, execution, verification, escalation), the operator MUST materialize input in a ConfigMap and create a batch Pod or SandboxClaim. The sandbox MUST publish a Result CR whose `spec` is immutable; its `status` MAY subsequently be patched. The operator MUST observe and process the Result CR only after `status.conditions` includes `Completed=True`. The production workflow does not call a sandbox HTTP endpoint.
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
| `--leader-elect` | bool | `false` | Enable leader election (enabled in Deployment) |
| `--secure-metrics-server` | bool | `false` | Enable HTTPS metrics (enabled in Deployment) |
| `--cert-dir` | string | `/etc/tls/private` | Metrics serving certificate directory (`tls.crt`, `tls.key`) |
| `--health-probe-bind-address` | string | `:8081` | Health probe endpoint bind address |
| `lightspeed-agentic-configuration` ConfigMap | — | — | Sandbox mode, base PodSpec, OTEL endpoints, MCP endpoint (produced by lightspeed-operator) |

## Constraints

- All deployed replicas MUST share the same namespaced election Lease. Running multiple replicas with election disabled is unsupported.
- The default installation requires OpenShift service-ca and the Prometheus Operator ServiceMonitor CRD. Local `make run` retains HTTP metrics and does not require those monitoring resources.
- Sandbox provisioning via `SandboxClaim` depends on the Sandbox API CRDs being installed in the cluster; this dependency only applies when `sandbox-mode=sandbox-claim` in the ConfigMap. The default `bare-pod` mode has no external CRD dependency.
- The `lightspeed-agentic-configuration` ConfigMap must be created by lightspeed-operator before any AgenticRun can execute. The operator does not block startup on this ConfigMap — it starts normally and individual runs fail gracefully when the ConfigMap is missing.
- The operator requires OpenShift APIs for console plugin deployment; running on vanilla Kubernetes skips console integration.

## Planned Changes

| Ticket | Summary |
|---|---|
| OLS-2957 | Sandbox template management UX and CRD ergonomics may change operator/template coupling |
| OLS-2940 | Autonomous workflow CRD migrations may rename or reshape `v1alpha1` fields |
| OLS-3236 | Remove `controller/console/` package and `--agentic-console-image` flag. Console plugin and alerts adapter deployment moves to lightspeed-operator. |
| OLS-3685/3686 | [DONE] Inter-operator config handoff: unified `SandboxManager` reads base PodSpec from `lightspeed-agentic-configuration` ConfigMap. Sandbox mode, image, and PodSpec all sourced from ConfigMap. CLI flags removed. No-blocking startup; graceful degradation. |
