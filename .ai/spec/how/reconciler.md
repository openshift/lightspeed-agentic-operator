# AgenticRun controller — architecture (how)

Audience: AI agents. Behavioral rules and phase semantics live in **what/** specs (e.g. `what/run-lifecycle.md`, `what/crd-api.md`, `what/approval.md`, `what/sandbox-execution.md`). This document maps **structure, call graph, and implementation mechanics** only.

---

## Entry point: `cmd/main.go`

- Parses flags: `metrics-bind-address`, `health-probe-bind-address`, `namespace` (falls back to `POD_NAMESPACE`).
- Builds controller-runtime `Manager` with core + `agenticv1alpha1` scheme.
- Creates `configuration.Cache` (starts nil). Eagerly attempts `configwatch.TryLoad` for the `lightspeed-agentic-configuration` ConfigMap. Registers `configwatch.Watcher` for runtime changes.
- Wires **dependency injection** directly (no `controller/setup.go`):
  - `agenticrun.NewSandboxManager(mgr.GetClient(), cfgCache, namespace, auditLogger)` → `SandboxLifecycle`.
  - `&agenticrun.SandboxAgentCaller{Sandbox, K8sClient, ClientFactory, Namespace, Audit}` → satisfies `agenticrun.AgentCaller`.
  - `agenticrun.AgenticRunReconciler{Client, Agent, Config, Namespace, Audit, TempLog}` → `SetupWithManager(mgr)`.
  - `agenticolsconfig.Reconciler` → `SetupWithManager(mgr)` — maintains `AgenticOLSConfig` `Suspended` condition.
- Ensures `lightspeed-agent` ServiceAccount unconditionally (idempotent create).
- Registers health/readiness probes and webhook.

---

## Module map: `controller/agenticrun/`

| File | Types / primary responsibilities | Key functions / methods |
|------|----------------------------------|-------------------------|
| `reconciler.go` | `AgenticRunReconciler` (embeds `client.Client`, `Agent AgentCaller`, `Log`) | `Reconcile`, `SetupWithManager` |
| `handlers.go` | (methods on `AgenticRunReconciler`) | `handleAnalysis`, `handleRevision`, `handleExecution`, `handleVerification`, `handleEscalation`, `handleFailed`, `denyAgenticRun`, `conditionTime`, `hasMutationSuccess`, `isObservationAction`, `analysisFailureMessage`, `executionFailureMessage` |
| `helpers.go` | `revisionData`, `analysisQuery`, `executionQuery`, `verificationQuery`, `escalationData`; embedded templates via `//go:embed templates/*.tmpl` | `renderTemplate`, `failStep`, `statusPatch`, `hasSandboxClaims`, `isTerminal`, `setVerificationSkipped`, `getLatestAnalysisResult`, `selectedOption`, `trimNonSelectedOptions`, `resetExecutionAndVerification`, `buildEscalationRequest`, `needsRevision`, `buildRevisionContext`, `buildAnalysisQuery`, `buildExecutionQuery`, `buildVerificationQuery`, `prettyJSON` |
| `approval.go` | — | `getApprovalPolicy`, `getAgenticRunApproval`, `ensureAgenticRunApproval`, `isStageApproved`, `isStageDenied`, `getStageOverrideAgent`, `getStageOption` |
| `resolve.go` | `resolvedStep`, `resolvedWorkflow` | `resolveAgenticRun`, `stepAgentName` |
| `agent.go` | `AgentCaller`, `StubAgentCaller`; `AnalysisOutput`, `ExecutionOutput`, `VerificationOutput`, `EscalationOutput` | Interface methods on `StubAgentCaller` |
| `sandbox_manager.go` | `SandboxManager` | `NewSandboxManager`, `Create`, `Release`, `createBarePod`, `createSandboxClaim`, `releaseBarePod`, `releaseSandboxClaim`, `ensureSA`, `setSAOwner`, `buildInputConfigMap`, `createInputConfigMap`, `podSpecToUnstructured` |
| `sandbox_agent.go` | `SandboxLifecycle` interface; `SandboxAgentCaller` | `Analyze`, `Execute`, `Verify`, `Escalate`, `ReleaseSandboxes`, `launchSandbox`, `patchSandboxInfo`, `buildAgentContext`, `collectFailedResults`, `stepString` |
| `pod_handler.go` | Pod watch handler (methods on `AgenticRunReconciler`); timeout background goroutine | `handlePodEvent`, `completeStep`, `patchStepCondition`, `patchStepResult`, `releaseSandbox`, `runTimeoutLoop`, `handleTimeEvent`, `stepConditionType`, `fetchResultCR`, `podFailMessage` |
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
|------|-------|----------------|
| `reconciler.go` | `Reconciler` (embeds `client.Client`, `EventRecorder`) | `Reconcile`, `SetupWithManager`, `handleActivation`, `handleDeactivation` |
| `reconciler_test.go` | — | Activation/deactivation, event emission, non-terminal run requeue |

**Integration note:** Registered in `cmd/main.go`. Watches the cluster `AgenticOLSConfig` named `cluster` and **Watches** `AgenticRun` objects to requeue the config when run phases change.

---

## Module map: `controller/console/`

| File | Types | Key functions |
|------|-------|----------------|
| `reconciler.go` | `AgenticConsoleConfig` (Image, Namespace); constants for plugin name, cert, nginx config string | `EnsureAgenticConsole` (orchestrates ordered ensures), `labels`, `ensureConfigMap`, `ensureServiceAccount`, `ensureService`, `ensureDeployment`, `ensureConsolePlugin`, `ensureConsoleActivation` |
| `reconciler_test.go` | — | Tests for idempotency, image updates, skip when no image |

**Integration note:** `EnsureAgenticConsole` is registered in `cmd/main.go` as a `manager.RunnableFunc` — it runs once at manager start, not as a reconcile loop. It mutates OpenShift `Console` cluster CR `spec.plugins` via retry-on-conflict.

---

## Data flow: reconcile loop

1. **Watch / enqueue:** controller-runtime delivers `ctrl.Request` for a `AgenticRun` namespaced name. `SetupWithManager` `Owns` child CRs (`AgenticRunApproval`, `AnalysisResult`, `ExecutionResult`, `VerificationResult`, `EscalationResult`), `Owns` Pods and ConfigMaps [OLS-3066], and **Watches** cluster `ApprovalPolicy` and `AgenticOLSConfig` to enqueue all non-terminal runs when either changes. [OLS-3066] Pod watches serve **failure detection** (pod `Failed`, `ImagePullBackOff`); Result CR watches serve **completion detection** (CR created with `Completed` condition).
2. **`Reconcile` load:** `Get` `AgenticRun`; ignore not-found.
3. **Deletion path:** If `DeletionTimestamp` set: (a) RBAC finalizer `agentic.openshift.io/execution-rbac-cleanup` present → `Agent.ReleaseSandboxes` (which calls `Release` per step — handles pod/claim deletion, SA reader-subject removal, execution RBAC cleanup via GC and explicit cross-namespace delete), remove finalizer; (b) templog finalizer present → `Agent.CleanupTemplogs`, remove finalizer. Both finalizers are processed in a **single reconcile pass** — no intermediate requeue.
4. **Suspension check:** Fetch `AgenticOLSConfig` singleton via `isSuspended()`. If `spec.suspended == true` and run is non-terminal: `handleSuspension` releases sandboxes (best-effort via `Agent.ReleaseSandboxes`), sets `EmergencyStopped=True` condition, status patch, return. If CR not found, treat as not suspended. See **what/system-config.md**.
5. **Phase:** `agenticv1alpha1.DerivePhase(proposal.Status.Conditions)` — see **what/** for semantics. Now includes `EmergencyStopped` as highest-precedence terminal phase.
6. **Finalizer add:** If not terminal and finalizer missing, add RBAC cleanup finalizer (re-fetch proposal after patch).
7. **Terminal / failed shortcuts:** Completed/Denied/Escalated/EmergencyStopped → optional sandbox release via `Agent.ReleaseSandboxes`. `AgenticRunPhaseFailed` → `handleFailed`.
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
- **`Release(ctx, run, step)`:** (1) Deletes the pod/claim — Kubernetes GC cascades to SA, ConfigMap, and result RBAC via owner references; (2) removes the per-step SA from all reader ClusterRoleBindings via `removeReaderSubject`; (3) for execution step: explicitly cleans up cross-namespace Roles/ClusterRoles via `cleanupExecutionRBAC` (these cannot use owner refs due to cross-namespace constraints). Routes by `cfg.Sandbox.Mode`. Idempotent (NotFound ignored).

### `PodSpecBuilder` (internal to `SandboxManager`)

- **Build:** Takes base `*corev1.PodSpec` (from config cache) and overlays agent-specific configuration: LLM env vars, credential mounts, skills volumes, MCP config, required secrets, input ConfigMap volume mount [OLS-3066], SA. [OLS-3066] HTTP readiness/liveness probes are no longer set. [PLANNED: OLS-3743] It always injects operator-resolved `LIGHTSPEED_AGENT_TIMEOUT_SECONDS` and `LIGHTSPEED_AGENT_MAX_TURNS` for the selected step Agent.
- Also defines label constants (`LabelManaged`, `LabelRun`, etc.) and shared helpers (`credentialsSecretName`, `providerURL`, `providerTypeString`).

**No log streaming in controller:** logs are cluster-side (`kubectl` / CLI); [OLS-3066] manager watches for Result CR creation, not endpoint readiness.

---

## `SandboxAgentCaller` [OLS-3066: batch model]

- **Constructor:** Struct literal with `Sandbox SandboxLifecycle`, `K8sClient`, `Namespace`, `Audit`.
- **[OLS-3066] Batch flow:** Each `Analyze`/`Execute`/`Verify`/`Escalate` method calls `launchSandbox` which: (a) calls `Sandbox.Create` (which fully encapsulates SA, RBAC, ConfigMap, and pod creation — see `SandboxManager.Create` above), (b) patches sandbox info on the run, (c) returns nil (handler returns `ctrl.Result{}, nil`; re-entry is watch-driven via pod events and the background timeout ticker). No `WaitReady`, no HTTP call, no `serviceAccount` parameter.
- **`buildAgentContext`:** Unchanged — `TargetNamespaces`, `ApprovedOption` / `ExecutionResult` per step, `PreviousAttempts` from failed `StepResultRef` outcomes.
- **`ReleaseSandboxes`:** Iterates `Status.Steps.{Analysis,Execution,Verification,Escalation}.Sandbox.ClaimName` and calls `Sandbox.Release` for each non-empty. `Release` handles all cleanup (pod, SA reader-subjects, execution RBAC).

## `AgentHTTPClient` [OLS-3066: removed]

The `AgentHTTPClient`, `AgentHTTPClientInterface`, `agentRunRequest`, `agentRunResponse`, `ClientFactory`, and `client.go` are removed under OLS-3066. The operator no longer makes HTTP calls to sandbox pods. All I/O is via ConfigMap (input) and Result CR (output).

## `PodEventHandler` and timeout loop [OLS-3794]

- **`PodEventHandler`:** Registered via `Watches(&Pod{}, handler.EnqueueRequestsFromMapFunc)` in `SetupWithManager`. When a sandbox pod terminates (Succeeded/Failed), it: (a) reads the Result CR to determine agent success/failure, (b) patches the step condition on the `AgenticRun`, (c) calls `releaseSandbox` for cleanup. This drives the async lifecycle without reconciler polling.
- **`runTimeoutLoop`:** Background goroutine started by the reconciler via `mgr.Add`. Periodically lists in-progress runs and checks per-step sandbox timeouts. When a timeout is detected, patches the step condition to `False` with reason `SandboxTimeout` and releases the sandbox.

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
- **Execution RBAC creation:** When the approved option has non-empty `RBAC` rules, `SandboxManager.Create` calls `ensureExecutionRBAC`. Creates namespaced `Role`/`RoleBinding` per target namespace (from `Spec.TargetNamespaces` or rule namespace fields), persists comma-joined namespaces in annotation `agentic.openshift.io/rbac-namespaces`, and cluster `ClusterRole`/`ClusterRoleBinding` when cluster rules present.
- **Cleanup:** `SandboxManager.Release` handles all cleanup: (a) deletes pod/claim — GC cascades to SA, ConfigMap, result RBAC via owner references; (b) removes per-step SA from reader CRBs via `removeReaderSubject`; (c) for execution step: `cleanupExecutionRBAC` reads the annotation to delete cross-namespace Roles/RoleBindings and cluster RBAC. No standalone `handleRBACCleanup` — `ReleaseSandboxes` (called from finalizer and terminal paths) handles everything.
- **`normalizeCoreAPIGroup`:** Maps LLM-facing `"core"` to `""` in K8s `PolicyRule.APIGroups`.
- **Read RBAC (multi-binding):** `resolveReaderBindings` discovers **all** `ClusterRoleBinding`s where `lightspeed-agent` is a subject (e.g. `cluster-reader`, `cluster-monitoring-view`), caches the list in `readerBindings` (`atomic.Value` storing `[]string`), and returns it. `addReaderSubject`/`removeReaderSubject` iterate all discovered bindings. Individual binding updates are in `addSubjectToBinding`/`removeSubjectFromBinding` with conflict retry loops. [OLS-3712]
- **Execution outcome override:** In `handleExecution`, when the agent reports `success=false`, the controller calls `hasMutationSuccess(actions)` to check whether all mutating actions actually succeeded. If yes, it overrides `execResult.Success = true` and proceeds to verification (or trust-mode completion). `isObservationAction(type)` classifies non-mutating action types (`pre-check`, `post-check`, `verification`, `check`, `wait`); everything else is treated as a mutation. [OLS-3558]

---

## Key abstractions

- **`AgentCaller`:** Boundary between reconciler and runtime (stub vs sandbox+batch). Methods mirror workflow steps (`Analyze`, `Execute`, `Verify`, `Escalate`) plus `ReleaseSandbox(ctx, run, step)` and `ReleaseSandboxes`. No `serviceAccount` parameter — SA management is fully encapsulated in `SandboxManager`. [OLS-3066] Production implementation no longer makes HTTP calls — it launches sandboxes via `SandboxManager.Create`, then returns. Result processing happens on re-entry when the Result CR appears.
- **`SandboxLifecycle`:** Interface (`Create(ctx, run, step, agent, llm, tools, deadline, query, agentCtx)` / `Release(ctx, run, step)`) for swappable sandbox management (tests can fake). Production implementation: `SandboxManager`. `Create` fully encapsulates SA, RBAC, ConfigMap, and pod lifecycle. All resources use the `ls-` name prefix; `Release` dispatches by reading `cfg.Sandbox.Mode` from the config cache. [OLS-3066] `WaitReady` is removed — the operator watches for pod completion and Result CR creation instead of polling.
- **`PodSpecBuilder`:** Internal to `SandboxManager`. Takes base `*corev1.PodSpec` from config cache and overlays agent config. Produces typed `corev1.PodSpec`; the mode then determines delivery (bare Pod or SandboxTemplate conversion).
- **`resolveAgenticRun`:** Produces `resolvedWorkflow` with cached `Agent` + `LLMProvider` per name; applies per-stage agent overrides from `AgenticRunApproval` via `getStageOverrideAgent`; `Execution`/`Verification` steps nil when corresponding spec sections are zero.

---

## Integration points (who calls whom)

```text
cmd/main.go
  ├─ configuration.NewCache → cfgCache (starts nil)
  ├─ configwatch.Watcher → populates cfgCache on ConfigMap event
  ├─ NewSandboxManager(client, cfgCache, namespace, auditLogger) → SandboxLifecycle
  ├─ SandboxAgentCaller{Sandbox, K8sClient, Namespace, Audit}
  ├─ AgenticRunReconciler{Client, Agent, Config: cfgCache, Namespace}.SetupWithManager
  ├─ agenticolsconfig.Reconciler.SetupWithManager
  └─ inline lightspeed-agent SA creation (manager.RunnableFunc)

AgenticRunReconciler.Reconcile
  ├─ config guard: cfgCache.Available() → false: fail with clear error
  ├─ approval.go, resolve.go
  ├─ handlers.go → results.go (read Result CRs), rbac.go, helpers.go (status, option trim)
  └─ Agent (SandboxAgentCaller) [OLS-3066: batch model]
        ├─ First entry: launchSandbox
        │   └─ Sandbox.Create (SA + reader CRB + execution RBAC + ConfigMap + PodSpec → bare Pod / SandboxClaim)
        │   └─ patchSandboxInfo → return (watch-driven re-entry)
        ├─ Re-entry: check Result CR (Completed condition) → process result
        │   └─ Update run conditions, append result ref
        ├─ PodEventHandler: pod terminated → process result → patch condition → releaseSandbox
        ├─ runTimeoutLoop: periodic check → timeout → patch condition → releaseSandbox
        └─ Sandbox.Release (terminal phases, deletion) → GC SA/CM + remove reader subjects + execution RBAC
```

---

## Implementation notes (gotchas)

- **`cmd/main.go` scheme:** Registers core + `agenticv1alpha1` + `consolev1` + `openshiftv1`. No separate `controller/setup.go` — all wiring is inline in `main.go`. Watching or applying arbitrary CRDs from tests may need extended schemes (see `reconciler_test.go`).
- **Max concurrent reconciles:** `SetupWithManager` reads cluster `ApprovalPolicy` via API reader for `MaxConcurrentRuns`, else `DefaultMaxConcurrentRuns` from API package.
- **Policy watch:** Enqueues **all** non-terminal runs on any `ApprovalPolicy` event — can be chatty.
- **AgenticOLSConfig watch:** Same pattern as policy watch — enqueues all non-terminal runs on any `AgenticOLSConfig` change. When `suspended` flips to `true`, all re-queued runs hit the suspension guard and get terminated.
- **Workflow resolution errors:** Patched onto `AgenticRunConditionAnalyzed` false — see API for exact condition ordering vs `DerivePhase`.
- **`selectedOption` vs trim:** Verification uses latest analysis result’s **first** option (`Options[0]`) when resolving; execution path uses `trimNonSelectedOptions` which respects `AgenticRunApproval` execution option index when multiple options exist.
- **No execution retries:** Execution runs exactly once per analysis iteration; verification failure escalates directly via the `Escalating` phase — there is no `maxAttempts`/retry loop (see **what/run-lifecycle.md** and **what/approval.md**).
- **[OLS-3066] No sandbox FQDN or endpoint:** With the batch model, the operator does not construct agent URLs or connect to sandbox pods over HTTP. The former `Sandbox FQDN` note is obsolete.
- **Logs CLI vs status:** CLI `logs` uses `SandboxInfo.ClaimName` as **pod name** in `GetLogs`; ensure cluster layout matches (if claim name ≠ pod name, logs command would need revision — operational detail for agents touching `logs.go`). [OLS-3066] Log tailing is unchanged — sandbox pods still write progress to stdout during execution.
- **Tests:** `state_machine_test.go` is the primary lifecycle matrix; `testAgentCaller` implements `AgentCaller` with injectable errors/results; fake client uses `WithStatusSubresource` for run and result types.
- **[PLANNED: OLS-3743] Limit resolution:** Resolve timeout and max-turn defaults in one operator helper from the selected `resolvedStep.Agent`, including approval agent overrides. The resolved timeout drives both pod env injection and hard-deadline calculation; these paths MUST NOT resolve independently.
