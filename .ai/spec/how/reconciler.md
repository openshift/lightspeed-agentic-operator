# AgenticRun controller — architecture (how)

Audience: AI agents. Behavioral rules and phase semantics live in **what/** specs (e.g. `what/run-lifecycle.md`, `what/crd-api.md`, `what/approval.md`, `what/sandbox-execution.md`). This document maps **structure, call graph, and implementation mechanics** only.

---

## Entry point: `cmd/main.go`

- Parses flags: `metrics-bind-address`, `health-probe-bind-address`, `namespace` (falls back to `POD_NAMESPACE`).
- Builds controller-runtime `Manager` with core + `agenticv1alpha1` scheme.
- Creates `configuration.Cache` (starts nil). Eagerly attempts `configwatch.TryLoad` for the `lightspeed-agentic-configuration` ConfigMap. Registers `configwatch.Watcher` for runtime changes. [PLANNED: OLS-3928] The cache also holds the effective tool-result inspection value.
- Wires **dependency injection** directly (no `controller/setup.go`):
  - `sandboxMgr := agenticrun.NewSandboxManager(mgr.GetClient(), mgr.GetAPIReader(), cfgCache, namespace, auditLogger)` → shared concrete `SandboxManager`.
  - `&agenticrun.SandboxAgentCaller{Sandbox: sandboxMgr, APIReader: mgr.GetAPIReader(), K8sClient, Namespace, Audit}` → satisfies `agenticrun.AgentCaller`.
  - `agenticrun.AgenticRunReconciler{Client, APIReader: mgr.GetAPIReader(), Agent, Sandbox: sandboxMgr, Config, Namespace, Audit, TempLog}` → `SetupWithManager(mgr)`.
  - `agenticolsconfig.Reconciler{Client, APIReader: mgr.GetAPIReader(), Sandbox: sandboxMgr}` → `SetupWithManager(mgr)`; `Now` defaults to `time.Now` and is injectable in tests.
- Ensures `lightspeed-agent` ServiceAccount unconditionally (idempotent create).
- Registers health/readiness probes and webhook.

---

## Module map: `controller/agenticrun/`

| File | Types / primary responsibilities | Key functions / methods |
| ------ | ---------------------------------- | ------------------------- |
| `reconciler.go` | `AgenticRunReconciler` (embeds `client.Client`, `Agent AgentCaller`, `Log`) | `Reconcile`, `SetupWithManager`, termination guard ordering |
| `handlers.go` | (methods on `AgenticRunReconciler`) | `handleAnalysis`, `handleRevision`, `handleExecution`, `handleVerification`, `handleEscalation`, `handleFailed`, `handleSuspensionAt`, `denyAgenticRun`, `conditionTime`, `hasMutationSuccess`, `isObservationAction`, `analysisFailureMessage`, `executionFailureMessage` |
| `helpers.go` | `revisionData`, `analysisQuery`, `executionQuery`, `verificationQuery`, `escalationData`; embedded templates via `//go:embed templates/*.tmpl` | `renderTemplate`, `failStep`, `statusPatch`, `hasSandboxClaims`, `isTerminal`, `setVerificationSkipped`, `getLatestAnalysisResult`, `selectedOption`, `trimNonSelectedOptions`, `resetExecutionAndVerification`, `buildEscalationRequest`, `needsRevision`, `buildRevisionContext`, `buildAnalysisQuery`, `buildExecutionQuery`, `buildVerificationQuery`, `prettyJSON` |
| `approval.go` | — | `getApprovalPolicy`, `getAgenticRunApproval`, `ensureAgenticRunApproval`, `isStageApproved`, `isStageDenied`, `getStageOverrideAgent`, `getStageOption` |
| `resolve.go` | `resolvedStep`, `resolvedWorkflow` | `resolveAgenticRun`, `stepAgentName`; [DONE: OLS-4060] resolves one run-level `ToolsSpec` from `AgenticRun.spec.tools` for all steps |
| `agent.go` | `AgentCaller`, `StubAgentCaller`; `AnalysisOutput`, `ExecutionOutput`, `VerificationOutput`, `EscalationOutput` | Interface methods on `StubAgentCaller` |
| `sandbox_manager.go` | `SandboxManager` | `NewSandboxManager`, `Create`, `Release`, `createBarePod`, `createSandboxClaim`, `releaseBarePod`, `releaseSandboxClaim`, `releaseSandboxClaimPod`, `sandboxPodNameAnnotation`, `ensureSA`, `setSAOwner`, `buildInputConfigMap`, `createInputConfigMap`, `podSpecToUnstructured` |
| `sandbox_agent.go` | `SandboxLifecycle` interface; `SandboxAgentCaller` | `Analyze`, `Execute`, `Verify`, `Escalate`, `ReleaseSandboxes`, `launchSandbox`, `patchSandboxInfo`, `buildAgentContext`, `collectFailedResults`, `stepString` |
| `pod_handler.go` | Unified pod watch handler for both bare-pod and sandbox-claim modes; shared step helpers | `handlePodEvent`, `resolveBarePodMetadata`, `resolveSandboxPodMetadata`, `completeStep`, `patchStepCondition`, `patchStepResult`, `releaseSandbox`, `stepConditionType`, `validateResultCR`, `podFailMessage`, `podTerminatedInfo` |
| `timeout_handler.go` | Mode-dispatching timeout background goroutine; shared timeout helpers | `runTimeoutLoop`, `handleTimeEvent`, `listBarePods`, `listSandboxPods`, `isSandboxClaimMode`, `startTimedOut`, `overallTimedOut` |
| `podspec_builder.go` | `PodSpecBuilder`; label constants (`LabelManaged`, `LabelRun`, etc.); MCP env DTOs (`mcpServerEnvEntry`, `mcpHeaderEnvEntry`) | `Build`, `buildSkills`, `buildMCPServers`, `buildRequiredSecrets`, `addProviderSpecificEnv`, `credentialsSecretName`, `providerURL`, `providerTypeString` |
| `schemas.go` | Package vars: default/minimal analysis schemas, execution/verification/escalation schemas; `defaultOutputSchemas`, `builtInPropertyJSON` | `init` (precompute property JSON), `injectBuiltInProperty`, `outputSchemaForStep` |
| `rbac.go` | `readerBindings atomic.Value` (cached CRB names) | `ensureExecutionRBAC`, `cleanupExecutionRBAC`, `resolveReaderBindings`, `addReaderSubject`, `removeReaderSubject`, `addSubjectToBinding`, `removeSubjectFromBinding`, `annotatedRBACNamespaces`, `deleteIfExists`, `rbacTargetNamespaces`, `truncateK8sName`, `sandboxSAName`, `executionRoleName`, `clusterRoleName`, `rbacLabels`, `rbacRulesToPolicyRules`, `normalizeCoreAPIGroup` |
| `results.go` | `statusHolder` interface (defined; no references elsewhere in this package) | `resultCRName`, `agenticRunOwnerRef`, `resultLabels`, `resultConditions`, `createAnalysisResult`, `createExecutionResult`, `createVerificationResult`, `createEscalationResult`, `createIdempotent` |
| `templates/*.tmpl` | Text templates | Names: `analysis_query.tmpl`, `execution_query.tmpl`, `verification_query.tmpl`, `revision_context.tmpl`, `escalation_request.tmpl` |
| `reconciler_test.go` | `testAgentCaller`, fixtures | `testScheme`, `testDefaultAgent`, `testAgenticRun`, `reconcileOnce`, `getAgenticRun`, … |
| `state_machine_test.go` | Policy/combo tests | Helpers: `testManualPolicy`, `newManualReconciler`, `approveStage`, `denyStage`, `assertPhase`, … |
| `approval_test.go` | Tests for approval helpers | — |
| `pod_handler_test.go` | Pod event handler and timeout tests | — |
| `handlers_test.go` | Handler-focused tests | — |
| `helpers_test.go` | Helper tests | — |
| `results_test.go` | Result CR tests | — |
| `resolve_test.go` | Resolution tests | — |
| `revision_test.go` | Revision flow tests | — |
| `rbac_test.go` | RBAC ensure/cleanup tests | — |
| `sandbox_manager_test.go` | SandboxManager Create/Release, name prefix routing, truncation | — |
| `sandbox_agent_test.go` | Agent caller tests | — |
| `schemas_test.go` | Output schema assembly tests | — |

---

## Module map: `controller/agenticolsconfig/`

| File | Types | Key functions |
| ------ | ------- | ---------------- |
| `reconciler.go` | `Reconciler` (`client.Client`, `APIReader`, `Sandbox`, `EventRecorder`, optional `Now`) | `Reconcile`, `SetupWithManager`, `ensureActivationStatus`, `reconcileSuspension`, `persistRunStop`, `patchActivationOutcome`, `handleDeactivation`, `activationStart`, `setConditionAt` |
| `reconciler_test.go` | — | Actual-Reconcile activation, deadline/restart, generation, cleanup/safety, marker-conflict, remote-evidence, and deactivation cases |

**Integration note:** Registered in `cmd/main.go`. Reads the singleton config and AgenticRun inventory through the uncached API reader, watches AgenticRun changes, and requeues every five seconds while suspension remains active to discover late resources.

---

## Module map: `controller/console/`

| File | Types | Key functions |
| ------ | ------- | ---------------- |
| `reconciler.go` | `AgenticConsoleConfig` (Image, Namespace); constants for plugin name, cert, nginx config string | `EnsureAgenticConsole` (orchestrates ordered ensures), `labels`, `ensureConfigMap`, `ensureServiceAccount`, `ensureService`, `ensureDeployment`, `ensureConsolePlugin`, `ensureConsoleActivation` |
| `reconciler_test.go` | — | Tests for idempotency, image updates, skip when no image |

**Integration note:** `EnsureAgenticConsole` is registered in `cmd/main.go` as a `manager.RunnableFunc` — it runs once at manager start, not as a reconcile loop. It mutates OpenShift `Console` cluster CR `spec.plugins` via retry-on-conflict.

---

## Data flow: reconcile loop

1. **Watch / enqueue:** controller-runtime delivers `ctrl.Request` for a `AgenticRun` namespaced name. `SetupWithManager` `Owns` child CRs (`AgenticRunApproval`, `AnalysisResult`, `ExecutionResult`, `VerificationResult`, `EscalationResult`), `Owns` Pods and ConfigMaps [OLS-3066], and **Watches** cluster `ApprovalPolicy` and `AgenticOLSConfig` to enqueue all non-terminal runs when either changes. [OLS-3066] Pod watches serve **failure detection** (pod `Failed`, `ImagePullBackOff`); Result CR watches serve **completion detection** (CR created with `Completed` condition).
2. **`Reconcile` load:** `Get` `AgenticRun`; ignore not-found.
3. **Deletion and stop checks:** A deleting run first reads authoritative suspension and its persisted stop marker. Accepted stop intent or a suspended config routes through stop handling before ordinary release; outside stop mode, the RBAC finalizer uses `Agent.ReleaseSandboxes(ctx, &run, false)` and the templog finalizer is processed as usual. For non-deleting runs, missing cleanup finalizers are added and re-fetched before the same stop check.
4. **Stop handling:** Before configuration-cache readiness, approval, revision, or terminal shortcuts, current suspension and persisted stop acceptance route to `handleSuspensionAt`. A current activation without current-generation status blocks progression until the config controller establishes it. The run controller persists `Stopping=True/SystemSuspended` before cleanup, uses the copied, second-precision global activation time, and writes confirmed (`Stopping=False/TeardownComplete` plus `EmergencyStopped=True`) or failed (`Stopping=False/TeardownFailed`) outcomes with optimistic locking. For a terminal run with `TerminalTime` at/after activation or absent, `Stopping` is auxiliary intent: cleanup may finalize that marker but must not alter workflow conditions or TTL; a definitively pre-activation terminal run remains unchanged. Stop cleanup continues to discover late resources and does not invent another ten-second window.
5. **Configuration guard:** Runs not in stop mode wait for the `lightspeed-agentic-configuration` cache. If it is unavailable, return an error.
6. **Phase:** `agenticv1alpha1.DerivePhase(run.Status.Conditions)` — see **what/** for semantics. `EmergencyStopped` is the highest-precedence terminal phase.
7. **Terminal / failed shortcuts:** Completed/Denied/Escalated/EmergencyStopped use ordinary terminal cleanup; `AgenticRunPhaseFailed` reaches `handleFailed`. These shortcuts occur only after the stop guard.
8. **Shared prelude:** `getApprovalPolicy` (cluster singleton name `cluster`), `ensureAgenticRunApproval`, `resolveAgenticRun`. Resolution failure → set `AgenticRunConditionAnalyzed=False` with `reasonWorkflowFailed`, status patch, return (no requeue).
9. **Phase switch:** Routes to `handleRevision` (if `needsRevision`) before analysis/execution/escalation arms; otherwise `handleAnalysis`, `handleExecution`, `handleVerification`, `handleEscalation`, or no-op.
10. **Handlers** set step conditions (`Unknown` → check Result CR / pod status → `True`/`False`), process Result CRs created by sandbox, append `Status.Steps.*.Results`, `statusPatch` proposal.
11. **[OLS-3066] Agent path (batch model):** Handlers use the async re-entry pattern defined in `what/sandbox-execution.md` rules 43–43e. On first entry: create input ConfigMap (query, output-schema, context, result-template) → create Pod/SandboxClaim with ConfigMap mounted at `/input/` → patch sandbox info → set step condition `Unknown` → return `ctrl.Result{}, nil`. Re-entry is driven by pod watch events (`handlePodEvent`) and Result CR watches, plus a background 1-minute timeout ticker (`runTimeoutLoop` / `handleTimeEvent`) as a safety net. [PLANNED: OLS-3743] The timeout loop uses the fixed startup deadline and the effective-agent-budget-based running deadline, and schedules checks no later than the applicable deadline. On re-entry: check Result CR (with `Completed` condition) → process result → update run conditions → cleanup pod + ConfigMap. No synchronous polling, no HTTP calls, no `WaitReady` within a single Reconcile. See `what/sandbox-execution.md` for the full re-entry decision tree, timeout handling, and race condition mitigations.

---

## Handler dispatch pattern

- **Single `Reconcile`** dispatches on **derived phase** and **revision predicate** (`needsRevision`: non-empty `Spec.RevisionFeedback` and `Generation > ObservedGeneration` on `AgenticRunConditionAnalyzed`).
- **Revision** clears downstream conditions and step sandboxes for execution/verification, resets analyzed condition to `Unknown`, appends revision context to request text, re-runs analysis path logic.
- **[OLS-3066] In-progress idempotency:** Each handler checks (a) existing run-level condition status — `True` or `False` means step is done, skip; (b) whether a Result CR exists with `Completed` condition — if so, process it; (c) whether a pod exists for this step — if not, create ConfigMap + pod. This replaces the former "check `Unknown` to avoid duplicate agent invocations" pattern. See `what/sandbox-execution.md` rules 43–43e for the full decision tree.
- **Approval gates:** Handlers call `isStageDenied` / `isStageApproved` before progressing; waiting states return `(Result{}, nil)` without error.

---

## `SandboxManager`

Unified sandbox lifecycle manager. Fully encapsulates SA, RBAC, ConfigMap, and pod creation/cleanup for every step.

- **`Create(ctx, run, step, agent, llm, tools, deadline, query, agentCtx)`:** Orchestrates: (1) creates a per-step ServiceAccount via `ensureSA` (`sandboxSAName(run, step, namespace)` → `ls-{step}-{namespace}-{runUID}`); (2) adds the per-step SA to all reader ClusterRoleBindings via `addReaderSubject`; (3) for execution step with non-empty RBAC: calls `ensureExecutionRBAC` and persists the RBAC namespaces annotation on the `AgenticRun`; (4) builds and creates the input ConfigMap (`buildInputConfigMap` + `createInputConfigMap`) with owner reference; (5) reads base PodSpec from config cache, overlays agent config via `PodSpecBuilder.Build`, creates either a bare Pod or SandboxClaim+SandboxTemplate; (6) sets SA owner reference to pod/claim via `setSAOwner`. Both resource types set OwnerReferences to the AgenticRun. Idempotent via `AlreadyExists`.
- **`Release(ctx, run, step, spoke, zeroGrace)`:** Ordinary per-step, terminal, and finalizer calls pass `false` and retain their existing graceful cleanup behavior. The suspension-specific low-level path may pass `true` for applicable spoke requests. Global and per-run hub cleanup is driven by `CleanupSuspension`, which performs managed inventory and authoritative confirmation rather than relying only on recorded sandbox references. `Release` still cleans per-step reader subjects and execution RBAC.
- **`CleanupSuspension(ctx, runUID)`:** Performs one uncached inventory, deletion, and confirmation pass. An empty UID sweeps managed hub-local inventory; a non-empty UID scopes it to one run. Confirmation verifies active workload removal and temporary-access cleanup; completed Pods need not be deleted, and a successful DELETE alone is insufficient. It preserves input ConfigMaps, Result CRs, shared reader bindings, and reusable templates, and does not discover or confirm spoke resources.

### `PodSpecBuilder` (internal to `SandboxManager`)

- **Build:** Takes base `*corev1.PodSpec` (from config cache) and overlays agent-specific configuration: LLM env vars, credential mounts, skills volumes, MCP config, required secrets, input ConfigMap volume mount [OLS-3066], SA. [OLS-3066] HTTP readiness/liveness probes are no longer set. [PLANNED: OLS-3743] It always injects operator-resolved `LIGHTSPEED_AGENT_TIMEOUT_SECONDS` and `LIGHTSPEED_AGENT_MAX_TURNS` for the selected step Agent. [PLANNED: OLS-3928] It also injects `LIGHTSPEED_TOOL_OUTPUT_INSPECTION_ENABLED` from the configuration cache.
- Also defines label constants (`LabelManaged`, `LabelRun`, etc.) and shared helpers (`credentialsSecretName`, `providerURL`, `providerTypeString`).

**No log streaming in controller:** logs are cluster-side (`kubectl` / CLI); [OLS-3066] manager watches for Result CR creation, not endpoint readiness.

---

## `SandboxAgentCaller` [OLS-3066: batch model]

- **Constructor:** Struct literal with `Sandbox SandboxLifecycle`, `APIReader client.Reader`, `K8sClient`, `Namespace`, `Audit`.
- **[OLS-3066] Batch flow:** Each `Analyze`/`Execute`/`Verify`/`Escalate` method calls `launchSandbox` which: (a) calls `Sandbox.Create` (which fully encapsulates SA, RBAC, ConfigMap, and pod creation — see `SandboxManager.Create` above), (b) patches sandbox info on the run, (c) returns nil (handler returns `ctrl.Result{}, nil`; re-entry is watch-driven via pod events and the background timeout ticker). No `WaitReady`, no HTTP call, no `serviceAccount` parameter.
- **`buildAgentContext`:** Adds `ApprovedOption` / `ExecutionResult` per step and `PreviousAttempts` from failed `StepResultRef` outcomes; target namespaces are carried by approved namespace-scoped RBAC rules, not as a separate context field.
- **`ReleaseSandboxes(ctx, run, zeroGrace)`:** Retains the ordinary reference-driven release helper and existing spoke cleanup requests. Global suspension does not rely on run references: the config controller calls `CleanupSuspension(ctx, "")`, while run stop handling uses the run UID and continues late-resource cleanup. Ordinary terminal/finalizer paths preserve their non-zero-grace behavior.

## `AgentHTTPClient` [OLS-3066: removed]

The `AgentHTTPClient`, `AgentHTTPClientInterface`, `agentRunRequest`, `agentRunResponse`, `ClientFactory`, and `client.go` are removed under OLS-3066. The operator no longer makes HTTP calls to sandbox pods. All I/O is via ConfigMap (input) and Result CR (output).

## Event handlers and timeout loop [OLS-3794, OLS-4070]

Two handlers — a unified pod watcher and a timeout loop.

- **`handlePodEvent` (pod_handler.go):** Registered via `Watches(&Pod{}, handler.EnqueueRequestsFromMapFunc)`. Handles both bare-pod and sandbox-claim modes. In bare-pod mode, `resolveBarePodMetadata` reads `LabelRun`/`LabelStep` labels directly from the pod. In sandbox-claim mode, `resolveSandboxPodMetadata` follows the pod's ownerRef chain (Pod → Sandbox → SandboxClaim) to find the SandboxClaim which carries operator labels and `AnnotationRunName`. Both paths feed into `completeStep` when the pod terminates (Succeeded/Failed): (a) reads the Result CR to determine agent success/failure, (b) patches the step condition on the `AgenticRun`, (c) calls `releaseSandbox` for cleanup. For in-progress pods, patches step reason (`Running` / `WaitingForSandbox`).
- **`runTimeoutLoop` (timeout_handler.go):** Background goroutine started via `mgr.Add`. Calls `handleTimeEvent` on each tick, which dispatches to `listBarePods` (labels) or `listSandboxPods` (ownerRef chain resolution) based on `isSandboxClaimMode()`. Both paths check per-step timeouts via `startTimedOut` / `overallTimedOut` and retry completion for terminal pods whose step condition patch failed earlier.

---

## Template system

- **Embed:** `helpers.go` embeds `templates/*.tmpl` into `templateFS`; `template.Must(ParseFS(...))`.
- **Query builders:** `buildAnalysisQuery` (`analysis_query.tmpl` + `analysisQuery`), `buildExecutionQuery` (`execution_query.tmpl` + pretty-printed option JSON), `buildVerificationQuery` (`verification_query.tmpl` + option + execution JSON via `executionOutputToAgentResult`).
- **Revision:** `buildRevisionContext` → `revision_context.tmpl`.
- **Escalation:** `buildEscalationRequest` → `escalation_request.tmpl` with run identity, request, and slices of `StepResultRef` from status (`Name`, `Outcome` per API — verify template field names match; `StepResultRef` has no `Success` field).

---

## Result CR creation [OLS-3066: moved to sandbox]

- **[OLS-3066]** Result CRs are created by the **sandbox** via `oc create` + `oc patch --subresource=status`, not by the operator. The operator pre-computes the Result CR template (metadata, labels, ownerRefs, spec) and includes it in the input ConfigMap — see `what/sandbox-execution.md` rules 7a and 8.
- **Naming:** `resultCRName(agenticRunName, step, len(existingResults)+1)` with K8s name truncation — same function, now used to build the template.
- **Owner:** Controller ref to `AgenticRun`; labels `LabelRun`, `LabelStep` — set in the template by the operator.
- **`createIdempotent`:** Retained for backward compatibility but primary path is sandbox-driven creation. The operator only reads Result CRs, not creates them.

---

## RBAC resource lifecycle

- **Per-step SA naming:** `sandboxSAName(run, step, namespace)` generates `ls-{step}-{namespace}-{runUID}`. Uses the run UID to guarantee uniqueness without truncation collisions. Every step gets its own unique SA.
- **SA + reader RBAC creation:** `SandboxManager.Create` calls `ensureSA` to create the per-step SA, then `addReaderSubject` to add it to all reader ClusterRoleBindings. This happens for **all** steps, not just execution.
- **Execution RBAC creation:** When the approved option has non-empty `RBAC` rules, `SandboxManager.Create` calls `ensureExecutionRBAC`. Creates namespaced `Role`/`RoleBinding` per unique, non-empty namespace declared by namespace-scoped rules, persists those namespaces in annotation `agentic.openshift.io/rbac-namespaces`, and creates cluster `ClusterRole`/`ClusterRoleBinding` when cluster rules are present.
- **Cleanup:** `SandboxManager.Release` handles all cleanup: (a) deletes pod/claim — GC cascades to SA, ConfigMap, result RBAC via owner references; (b) removes per-step SA from reader CRBs via `removeReaderSubject`; (c) for execution step: `cleanupExecutionRBAC` reads the annotation to delete cross-namespace Roles/RoleBindings and cluster RBAC. No standalone `handleRBACCleanup` — `ReleaseSandboxes` handles recorded-step cleanup from the deletion finalizer (`false`), terminal cleanup (`false`), and suspension (`true`).
- **`normalizeCoreAPIGroup`:** Maps LLM-facing `"core"` to `""` in K8s `PolicyRule.APIGroups`.
- **Read RBAC (multi-binding):** `resolveReaderBindings` discovers **all** `ClusterRoleBinding`s where `lightspeed-agent` is a subject (e.g. `cluster-reader`, `cluster-monitoring-view`), caches the list in `readerBindings` (`atomic.Value` storing `[]string`), and returns it. `addReaderSubject`/`removeReaderSubject` iterate all discovered bindings. Individual binding updates are in `addSubjectToBinding`/`removeSubjectFromBinding` with conflict retry loops. [OLS-3712]
- **Execution outcome override:** In `handleExecution`, when the agent reports `success=false`, the controller calls `hasMutationSuccess(actions)` to check whether all mutating actions actually succeeded. If yes, it overrides `execResult.Success = true` and proceeds to verification (or trust-mode completion). `isObservationAction(type)` classifies non-mutating action types (`pre-check`, `post-check`, `verification`, `check`, `wait`); everything else is treated as a mutation. [OLS-3558]

---

## Key abstractions

- **`AgentCaller`:** Boundary between reconciler and runtime (stub vs sandbox+batch). Methods mirror workflow steps (`Analyze`, `Execute`, `Verify`, `Escalate`) plus `ReleaseSandbox(ctx, run, step)` (always `zeroGrace=false`) and `ReleaseSandboxes(ctx, run, zeroGrace)` (suspension `true`, terminal/finalizer `false`). No `serviceAccount` parameter — SA management is fully encapsulated in `SandboxManager`. [OLS-3066] Production implementation no longer makes HTTP calls — it launches sandboxes via `SandboxManager.Create`, then returns. Result processing happens on re-entry when the Result CR appears.
- **`SandboxLifecycle`:** Interface (`Create(ctx, run, step, agent, llm, tools, deadline, query, agentCtx)` / `Release(ctx, run, step, spoke, zeroGrace)`) for swappable sandbox management (tests can fake). Production implementation: `SandboxManager`. `Create` fully encapsulates SA, RBAC, ConfigMap, and pod lifecycle. All resources use the `ls-` name prefix; `Release` dispatches by reading `cfg.Sandbox.Mode` from the config cache. [OLS-3066] `WaitReady` is removed — the operator watches for pod completion and Result CR creation instead of polling.
- **`PodSpecBuilder`:** Internal to `SandboxManager`. Takes base `*corev1.PodSpec` from config cache and overlays agent config. Produces typed `corev1.PodSpec`; the mode then determines delivery (bare Pod or SandboxTemplate conversion).
- **Provider-egress TLS [PLANNED: OLS-3041]:** The configuration cache reads the resolved TLS values and CA object-name references from `lightspeed-agentic-configuration`. `PodSpecBuilder` adds read-only source mounts below `/var/run/secrets/lightspeed/tls/` and passes `LIGHTSPEED_TLS_PROFILE`, `LIGHTSPEED_TLS_MIN_VERSION`, and `LIGHTSPEED_TLS_CIPHER_SUITES` unchanged. The agentic operator watches only the handoff ConfigMap; it does not inspect, aggregate, deduplicate, or watch referenced CA objects.
- **`resolveAgenticRun`:** Produces `resolvedWorkflow` with cached `Agent` + `LLMProvider` per name; applies per-stage agent overrides from `AgenticRunApproval` via `getStageOverrideAgent`; `Execution`/`Verification` steps nil when corresponding spec sections are zero. [DONE: OLS-4060] Tool resolution is run-level only: every resolved step receives `AgenticRun.spec.tools`; step records do not carry or override tools.

---

## Integration points (who calls whom)

```text
cmd/main.go
  ├─ configuration.NewCache → cfgCache (starts nil)
  ├─ configwatch.Watcher → populates cfgCache on ConfigMap event
  ├─ `sandboxMgr := NewSandboxManager(client, APIReader, cfgCache, namespace, auditLogger)` → shared manager
  ├─ `SandboxAgentCaller{Sandbox: sandboxMgr, APIReader, K8sClient, Namespace, Audit}`
  ├─ `AgenticRunReconciler{Client, APIReader, Agent, Sandbox: sandboxMgr, Config, Namespace}.SetupWithManager`
  ├─ `agenticolsconfig.Reconciler{Client, APIReader, Sandbox: sandboxMgr}.SetupWithManager`
  └─ inline lightspeed-agent SA creation (manager.RunnableFunc)

AgenticRunReconciler.Reconcile
  ├─ uncached AgenticRun load and persisted-stop/current-suspension guard
  ├─ accepted stop → per-run suspension cleanup/outcome before config-cache, approval, revision, or terminal dispatch
  ├─ otherwise: config-cache guard → terminal handling → approval.go / resolve.go → handlers.go
  ├─ Agent (SandboxAgentCaller) [OLS-3066: batch model]
  │     ├─ First entry: launchSandbox → Sandbox.Create → patchSandboxInfo → return (watch-driven re-entry)
  │     ├─ Re-entry: read Result CR / Pod event → update run workflow status
  │     └─ handlePodEvent / runTimeoutLoop remain guarded against stale workflow writes
  └─ ordinary Agent.Release(..., false) retains graceful cleanup

AgenticOLSConfigReconciler.Reconcile
  ├─ uncached config read; persist current-generation Suspended=Stopping and SafeToResume=False on activation
  ├─ uncached run inventory; persist UID-bound run stop acceptance before the global sweep
  ├─ SandboxManager.CleanupSuspension(ctx, "") with the original remaining ten-second window
  ├─ aggregate cleanup report, run outcomes, unsaved markers, and known unverified remote evidence
  ├─ patch Suspended and SafeToResume through the live reconcile context
  └─ requeue while suspended, including clean/error completion, to discover late resources

---

## Implementation notes (gotchas)

- **`cmd/main.go` scheme:** Registers core + `agenticv1alpha1` + `consolev1` + `openshiftv1`. No separate `controller/setup.go` — all wiring is inline in `main.go`. Watching or applying arbitrary CRDs from tests may need extended schemes (see `reconciler_test.go`).
- **Max concurrent reconciles:** `SetupWithManager` reads cluster `ApprovalPolicy` via API reader for `MaxConcurrentRuns`, else `DefaultMaxConcurrentRuns` from API package.
- **Policy watch:** Enqueues **all** non-terminal runs on any `ApprovalPolicy` event — can be chatty.
- **AgenticOLSConfig watch:** Run events enqueue the singleton config reconcile as an accelerator; the controller also requeues every five seconds while `spec.suspended` remains true. It persists current-generation `Suspended=True/Stopping` and `SafeToResume=False` before cleanup, normalizes a fresh activation time to second precision, and uses the persisted timestamp plus generation as activation identity. Nonterminal runs and terminal runs with absent or at/after-activation `TerminalTime` receive UID-bound auxiliary stop acceptance; definitively pre-activation terminal runs keep their history untouched. Run-marker reads/patches and cleanup use a pass context bounded by the original remaining ten seconds, then a separate five-second background timeout; final global outcome/safety patches use the live reconcile context. Suspension reconciliation requires both the uncached `APIReader` and concrete `SandboxManager`; missing dependencies are errors, not success fallbacks. The global sweep uses uncached `CleanupSuspension(ctx, "")`. Clean `AdminActivated` requires confirmed cleanup and affected outcomes. At the original deadline, unresolved cleanup, outcomes, inventory, or markers produce terminal `AdminActivatedWithErrors`. `SafeToResume` independently requires confirmed cleanup, durable run acceptance, and no known unverified remote workload/access; it may become true during later recovery without changing the terminal error reason. Both conditions observe the current suspended generation. A target cluster alone is not remote-resource evidence. Activation Events are best-effort, and terminal/clean/error states requeue to discover late resources.
- **Workflow resolution errors:** Patched onto `AgenticRunConditionAnalyzed` false — see API for exact condition ordering vs `DerivePhase`.
- **`selectedOption` vs trim:** Verification uses latest analysis result’s **first** option (`Options[0]`) when resolving; execution path uses `trimNonSelectedOptions` which respects `AgenticRunApproval` execution option index when multiple options exist.
- **No execution retries:** Execution runs exactly once per analysis iteration; verification failure escalates directly via the `Escalating` phase — there is no `maxAttempts`/retry loop (see **what/run-lifecycle.md** and **what/approval.md**).
- **[OLS-3066] No sandbox FQDN or endpoint:** With the batch model, the operator does not construct agent URLs or connect to sandbox pods over HTTP. The former `Sandbox FQDN` note is obsolete.
- **Logs CLI vs status:** CLI `logs` uses `SandboxInfo.ClaimName` as **pod name** in `GetLogs`; ensure cluster layout matches (if claim name ≠ pod name, logs command would need revision — operational detail for agents touching `logs.go`). [OLS-3066] Log tailing is unchanged — sandbox pods still write progress to stdout during execution.
- **Tests:** `state_machine_test.go` is the primary lifecycle matrix; `testAgentCaller` implements `AgentCaller` with injectable errors/results; fake client uses `WithStatusSubresource` for run and result types.
- **Tool-result inspection failure [PLANNED: OLS-3928]:** This path conforms to `openshift/ols/.ai/spec/what/tool-result-inspection.md`. The sandbox exits nonzero, publishes no Result CR, and writes `ToolResultSafetyInspectionFailed` as the termination message. `pod_handler.go` matches this message before generic `SandboxFailed` handling and uses it as the step condition reason. The condition uses the contract-defined controlled message.
- **[PLANNED: OLS-3743] Limit resolution:** Resolve timeout and max-turn defaults in one operator helper from the selected `resolvedStep.Agent`, including approval agent overrides. The resolved timeout drives both pod env injection and hard-deadline calculation; these paths MUST NOT resolve independently.
