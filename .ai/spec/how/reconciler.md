# Proposal controller — architecture (how)

Audience: AI agents. Behavioral rules and phase semantics live in **what/** specs (e.g. `what/proposal-lifecycle.md`, `what/crd-api.md`, `what/approval.md`, `what/sandbox-execution.md`). This document maps **structure, call graph, and implementation mechanics** only.

---

## Entry point: `cmd/main.go` → `controller.Setup`

- Parses flags: `metrics-bind-address`, `health-probe-bind-address`, `namespace` (falls back to `POD_NAMESPACE`), `agentic-console-image`, `agentic-sandbox-image`, `sandbox-mode` (default `bare-pod`).
- Builds controller-runtime `Manager` with core + `agenticv1alpha1` + OpenShift console/operator scheme.
- Calls `controller.Setup(mgr, opts)` which wires **dependency injection**:
  - **Mode-based provider selection** (`--sandbox-mode`):
    - `sandbox-claim`: `proposal.NewSandboxManager(mgr.GetClient(), namespace, "lightspeed-agent")` → `SandboxProvider`.
    - `bare-pod` (default): `proposal.NewBarePodManager(mgr.GetClient(), &proposal.PodSpecBuilder{Image: sandboxImage}, namespace)` → `SandboxProvider`.
  - `proposal.NewSandboxAgentCaller(sandboxProvider, mgr.GetClient(), proposal.NewAgentHTTPClient, namespace)` → satisfies `proposal.AgentCaller`.
  - `proposal.ProposalReconciler{ Client, Log, Agent, Namespace }` → `SetupWithManager(mgr)`.
  - `agenticconsole.EnsureAgenticConsole` registered as `manager.RunnableFunc` for console plugin deployment.
  - `agenticsandbox.EnsureBootstrapResources` registered as `manager.RunnableFunc` — creates SA always, creates `SandboxTemplate` only in `sandbox-claim` mode.
- Registers health/readiness probes. See `how/project-structure.md` for full flag reference.

---

## Module map: `controller/proposal/`

| File | Types / primary responsibilities | Key functions / methods |
|------|----------------------------------|-------------------------|
| `reconciler.go` | `ProposalReconciler` (embeds `client.Client`, `Agent AgentCaller`, `Log`) | `Reconcile`, `SetupWithManager` |
| `handlers.go` | (methods on `ProposalReconciler`) | `handleAnalysis`, `handleRevision`, `handleExecution`, `handleVerification`, `handleEscalation`, `handleFailed`, `denyProposal`, `conditionTime` |
| `helpers.go` | `revisionData`, `analysisQuery`, `executionQuery`, `verificationQuery`, `escalationData`; embedded templates via `//go:embed templates/*.tmpl` | `renderTemplate`, `failStep`, `statusPatch`, `hasSandboxClaims`, `isTerminal`, `setVerificationSkipped`, `getLatestAnalysisResult`, `selectedOption`, `trimNonSelectedOptions`, `resetExecutionAndVerification`, `maxAttempts`, `buildEscalationRequest`, `needsRevision`, `buildRevisionContext`, `buildAnalysisQuery`, `buildExecutionQuery`, `buildVerificationQuery`, `prettyJSON` |
| `approval.go` | — | `getApprovalPolicy`, `getProposalApproval`, `ensureProposalApproval`, `isStageApproved`, `isStageDenied`, `getStageOverrideAgent`, `getStageOption` |
| `resolve.go` | `resolvedStep`, `resolvedWorkflow` | `resolveProposal`, `stepAgentName` |
| `agent.go` | `AgentCaller`, `StubAgentCaller`; `AnalysisOutput`, `ExecutionOutput`, `VerificationOutput`, `EscalationOutput` | Interface methods on `StubAgentCaller` |
| `sandbox.go` | `SandboxProvider`, `SandboxManager` | `NewSandboxManager`, `SetStep`, `Claim`, `WaitReady`, `Release`, `buildClaim` |
| `bare_pod_manager.go` | `BarePodManager` | `NewBarePodManager`, `SetStep`, `Claim`, `WaitReady`, `Release` |
| `podspec_builder.go` | `PodSpecBuilder` | `Build`, `buildSkills`, `buildMCPServers`, `buildRequiredSecrets`, `addProviderSpecificEnv` |
| `sandbox_agent.go` | `SandboxAgentCaller`; private JSON DTOs for unmarshaling agent responses local to this file | `NewSandboxAgentCaller`, `Analyze`, `Execute`, `Verify`, `Escalate`, `ReleaseSandboxes`, `callWithSandbox`, `patchSandboxInfo`, `buildAgentContext`, `collectFailedResults`, `stepString` |
| `sandbox_templates.go` | `templateHashInput`; label constants (`LabelManaged`, `LabelProposal`, etc.); MCP env DTOs | `EnsureAgentTemplate`, `SandboxTemplateServiceAccount`, `computeTemplateHash`, `agentTemplateName`, `gcOldTemplates`, `patchLLMCredentials`, `credentialsSecretName`, `providerURL`, `patchRequiredSecrets`, `patchMCPServers`, `patchSkillsImage`, `patchSkillsPaths`, `patchProbes`, unstructured helpers (`firstContainer`, `setEnvVar`, `addEnvFromSecret`, …) |
| `client.go` | `AgentHTTPClientInterface`, `AgentHTTPClient`; `agentRunRequest`, `agentContext`, `agentExecutionResult`, `agentPreviousAttempt`, `agentRunResponse` | `NewAgentHTTPClient`, `(*AgentHTTPClient).Run`, `executionOutputToAgentResult` |
| `schemas.go` | Package vars: default/minimal analysis schemas, execution/verification/escalation schemas; `defaultOutputSchemas`, `builtInPropertyJSON` | `init` (precompute property JSON), `injectBuiltInProperty`, `outputSchemaForStep` |
| `rbac.go` | — | `ensureExecutionRBAC`, `cleanupExecutionRBAC`, `annotatedRBACNamespaces`, `deleteIfExists`, `rbacTargetNamespaces`, `truncateK8sName`, `executionRoleName`, `clusterRoleName`, `rbacLabels`, `rbacRulesToPolicyRules`, `normalizeCoreAPIGroup` |
| `results.go` | `statusHolder` interface (defined; no references elsewhere in this package) | `resultCRName`, `proposalOwnerRef`, `resultLabels`, `executionRetryIndex`, `resultConditions`, `createAnalysisResult`, `createExecutionResult`, `createVerificationResult`, `createEscalationResult`, `createIdempotent` |
| `templates/*.tmpl` | Text templates | Names: `analysis_query.tmpl`, `execution_query.tmpl`, `verification_query.tmpl`, `revision_context.tmpl`, `escalation_request.tmpl` |
| `reconciler_test.go` | `testAgentCaller`, fixtures | `testScheme`, `testDefaultAgent`, `testProposal`, `reconcileOnce`, `getProposal`, … |
| `state_machine_test.go` | Policy/combo tests | Helpers: `testManualPolicy`, `newManualReconciler`, `approveStage`, `denyStage`, `assertPhase`, … |
| `approval_test.go` | Tests for approval helpers | — |
| `client_test.go` | HTTP client tests | — |
| `handlers_test.go` | Handler-focused tests | — |
| `helpers_test.go` | Helper tests | — |
| `results_test.go` | Result CR tests | — |
| `resolve_test.go` | Resolution tests | — |
| `revision_test.go` | Revision flow tests | — |
| `rbac_test.go` | RBAC ensure/cleanup tests | — |
| `sandbox_test.go` | Sandbox manager tests | — |
| `sandbox_agent_test.go` | Agent caller tests | — |
| `sandbox_templates_test.go` | Template ensure/GC tests; `TestPatchProbes` (rule 30 in `what/sandbox-execution.md`) | — |
| `schemas_test.go` | Output schema assembly tests | — |

---

## Module map: `controller/console/`

| File | Types | Key functions |
|------|-------|----------------|
| `reconciler.go` | `AgenticConsoleConfig` (Image, Namespace); constants for plugin name, cert, nginx config string | `EnsureAgenticConsole` (orchestrates ordered ensures), `labels`, `ensureConfigMap`, `ensureServiceAccount`, `ensureService`, `ensureDeployment`, `ensureConsolePlugin`, `ensureConsoleActivation` |
| `reconciler_test.go` | — | Tests for idempotency, image updates, skip when no image |

**Integration note:** `EnsureAgenticConsole` is registered in `controller/setup.go` as a `manager.RunnableFunc` — it runs once at manager start, not as a reconcile loop. It mutates OpenShift `Console` cluster CR `spec.plugins` via retry-on-conflict.

---

## Data flow: reconcile loop

1. **Watch / enqueue:** controller-runtime delivers `ctrl.Request` for a `Proposal` namespaced name. `SetupWithManager` also `Owns` child CRs (`ProposalApproval`, `AnalysisResult`, `ExecutionResult`, `VerificationResult`, `EscalationResult`) and **Watches** cluster `ApprovalPolicy` and `AgenticOLSConfig` to enqueue all non-terminal proposals when either changes.
2. **`Reconcile` load:** `Get` `Proposal`; ignore not-found.
3. **Deletion path:** If `DeletionTimestamp` set and finalizer `agentic.openshift.io/execution-rbac-cleanup` present: `Agent.ReleaseSandboxes`, `cleanupExecutionRBAC`, remove finalizer, return.
4. **[PLANNED: OLS-3018] Suspension check:** Fetch `AgenticOLSConfig` singleton. If `spec.suspended == true` and proposal is non-terminal: release sandboxes, clean up RBAC, set `EmergencyStopped=True` condition, status patch, return. If CR not found, treat as not suspended. See **what/system-config.md**.
5. **Phase:** `agenticv1alpha1.DerivePhase(proposal.Status.Conditions)` — see **what/** for semantics. Now includes `EmergencyStopped` as highest-precedence terminal phase.
6. **Finalizer add:** If not terminal and finalizer missing, add RBAC cleanup finalizer (re-fetch proposal after patch).
7. **Terminal / failed shortcuts:** Completed/Denied/Escalated/EmergencyStopped → optional sandbox release via `Agent.ReleaseSandboxes`. `ProposalPhaseFailed` → `handleFailed` (RBAC cleanup if annotation set).
8. **Shared prelude:** `getApprovalPolicy` (cluster singleton name `cluster`), `ensureProposalApproval`, `resolveProposal`. Resolution failure → set `ProposalConditionAnalyzed=False` with `reasonWorkflowFailed`, status patch, return (no requeue).
9. **Phase switch:** Routes to `handleRevision` (if `needsRevision`) before analysis/execution/escalation arms; otherwise `handleAnalysis`, `handleExecution`, `handleVerification`, `handleEscalation`, or no-op.
10. **Handlers** set step conditions (`Unknown` → agent call → `True`/`False`), create result CRs, append `Status.Steps.*.Results`, `statusPatch` proposal.
11. **Agent path:** All agent steps go through `r.Agent.*` which (in production) is `SandboxAgentCaller`: `callWithSandbox` calls `SetStep` on the provider → `Claim` (provider-specific: `SandboxManager.Claim` handles template derivation, `BarePodManager.Claim` builds pod directly) → `patchSandboxInfo` on proposal → `WaitReady` → normalize URL → `outputSchemaForStep` → `ClientFactory(endpoint).Run` → JSON unmarshal into outputs.

---

## Handler dispatch pattern

- **Single `Reconcile`** dispatches on **derived phase** and **revision predicate** (`needsRevision`: non-empty `Spec.RevisionFeedback` and `Generation > ObservedGeneration` on `ProposalConditionAnalyzed`).
- **Revision** clears downstream conditions and step sandboxes for execution/verification, resets analyzed condition to `Unknown`, appends revision context to request text, re-runs analysis path logic.
- **In-progress idempotency:** Each handler checks existing condition status (`Unknown` / `True`) to avoid duplicate agent invocations on requeue.
- **Approval gates:** Handlers call `isStageDenied` / `isStageApproved` before progressing; waiting states return `(Result{}, nil)` without error.

---

## `SandboxProvider` implementations

### `SandboxManager` (sandbox-claim mode)

- Implements `SandboxProvider`.
- **SetStep:** Stores resolved step config (Agent, LLMProvider, Tools) for the next `Claim` call.
- **Claim:** Calls `EnsureAgentTemplate` internally (template derivation is encapsulated), then builds unstructured `SandboxClaim` (`extensions.agents.x-k8s.io/v1alpha1`, kind `SandboxClaim`) with labels `agentic.openshift.io/proposal`, `agentic.openshift.io/step`, `spec.sandboxTemplateRef`, `lifecycle.shutdownPolicy=Delete`. Name pattern `ls-{step}-{proposal}` truncated.
- **WaitReady:** Polls claim → reads `status.sandbox.name` → loads `Sandbox` (`agents.x-k8s.io/v1alpha1`) until `status.conditions` contains `Ready=True`, then returns `status.serviceFQDN`.
- **Release:** Deletes claim; treats NotFound as success.

### `BarePodManager` (bare-pod mode)

- Implements `SandboxProvider`.
- **SetStep:** Stores resolved step config (Agent, LLMProvider, Tools) for the next `Claim` call.
- **Claim:** Builds pod spec via `PodSpecBuilder`, creates a `Pod` in the operator namespace. Name pattern `ls-{step}-{proposal}` truncated. Labels include proposal name and step. Idempotent via `AlreadyExists`.
- **WaitReady:** Polls Pod conditions until `Ready=True`, returns `status.podIP`.
- **Release:** Deletes Pod; treats NotFound as success.

### `PodSpecBuilder` (shared)

- Used by `BarePodManager` directly and by `EnsureAgentTemplate` (via shared helper functions).
- **Build:** Produces `corev1.PodSpec` with agent container, LLM env vars, credential mounts, skills volumes, MCP config, required secrets, probes, security context, SA.
- Shared helper functions (`providerTypeString`, `credentialsSecretName`, `providerURL`, MCP DTOs) are also called by the unstructured template patchers in `sandbox_templates.go`.

**No log streaming in controller:** logs are cluster-side (`kubectl` / CLI); manager only waits for endpoint.

---

## `SandboxAgentCaller` and HTTP

- **Constructor:** Accepts `SandboxProvider`, `client.Client`, `ClientFactory func(endpoint string) AgentHTTPClientInterface`, operator namespace. `Timeout` defaults to `defaultSandboxTimeout` const.
- **`callWithSandbox` order:** `SetStep` on provider → `Claim` → `patchSandboxInfo` (status subresource merge) → `WaitReady` → normalize URL (`http://{endpoint}:8080` if no scheme) → `outputSchemaForStep` → `ClientFactory(endpoint).Run(ctx, "", query, schema, agentCtx)`. Template derivation (sandbox-claim mode) happens inside `SandboxManager.Claim`; bare-pod mode builds the pod spec inside `BarePodManager.Claim`.
- **`Run` contract:** Empty `systemPrompt`; full payload in POST body per `client.go` (`query`, `outputSchema`, `context`). Path constant `/v1/agent/run`.
- **`buildAgentContext`:** `TargetNamespaces`, `ApprovedOption` / `ExecutionResult` per step, `PreviousAttempts` from failed `StepResultRef` outcomes across analysis/execution/verification result lists.
- **`ReleaseSandboxes`:** Iterates `Status.Steps.{Analysis,Execution,Verification,Escalation}.Sandbox.ClaimName` and calls `Release` for each non-empty.

---

## `AgentHTTPClient` / `AgentHTTPClientInterface`

- **`AgentHTTPClientInterface`:** `Run(ctx, systemPrompt, query, outputSchema, agentCtx) (*agentRunResponse, error)`.
- **`NewAgentHTTPClient`:** Returns concrete type with long HTTP timeout, TLS `InsecureSkipVerify` for in-cluster calls.
- **`Run`:** Marshals `agentRunRequest`, POSTs, reads capped body size, non-200 → error with truncated body; 200 → raw JSON in `agentRunResponse.Response` for caller to unmarshal phase-specific structs.

---

## Template system

- **Embed:** `helpers.go` embeds `templates/*.tmpl` into `templateFS`; `template.Must(ParseFS(...))`.
- **Query builders:** `buildAnalysisQuery` (`analysis_query.tmpl` + `analysisQuery`), `buildExecutionQuery` (`execution_query.tmpl` + pretty-printed option JSON), `buildVerificationQuery` (`verification_query.tmpl` + option + execution JSON via `executionOutputToAgentResult`).
- **Revision:** `buildRevisionContext` → `revision_context.tmpl`.
- **Escalation:** `buildEscalationRequest` → `escalation_request.tmpl` with proposal identity, request, and slices of `StepResultRef` from status (`Name`, `Outcome` per API — verify template field names match; `StepResultRef` has no `Success` field).

---

## Result CR creation

- **Naming:** `resultCRName(proposalName, step, len(existingResults)+1)` with K8s name truncation.
- **`createIdempotent`:** `Create` object (API server drops status on create), then `Status().Patch(MergeFrom)` from a deep copy that retained full status — required because status subresource is separate.
- **Owner:** Controller ref to `Proposal`; labels `LabelProposal`, `LabelStep`.
- **Execution/Verification result CRs:** `Spec.RetryIndex` from `executionRetryIndex` ( ties to verification retry semantics in **what/** specs).

---

## RBAC resource lifecycle

- **Creation:** `handleExecution`, when selected option has non-empty `RBAC` rules, calls `ensureExecutionRBAC(ctx, Client, proposal, &selectedOption.RBAC, defaultSandboxSA, proposal.Namespace)`. Creates namespaced `Role`/`RoleBinding` per target namespace (from `Spec.TargetNamespaces` or rule namespace fields), persists comma-joined namespaces in annotation `agentic.openshift.io/rbac-namespaces`, and cluster `ClusterRole`/`ClusterRoleBinding` when cluster rules present. Sandbox SA name constant `defaultSandboxSA` (`lightspeed-agent` in `helpers.go`).
- **Cleanup:** `cleanupExecutionRBAC` reads annotation to delete bindings/roles; deletes cluster RBAC by derived name. Invoked on: proposal deletion (finalizer), `handleFailed` if annotation set, after successful escalation completion, and terminal phases via sandbox release path is separate.
- **`normalizeCoreAPIGroup`:** Maps LLM-facing `"core"` to `""` in K8s `PolicyRule.APIGroups`.

---

## Key abstractions

- **`AgentCaller`:** Boundary between reconciler and runtime (stub vs sandbox+HTTP). Methods mirror workflow steps plus `ReleaseSandboxes`.
- **`SandboxProvider`:** Swappable claim/wait/release (tests can fake). Implementations: `SandboxManager` (sandbox-claim mode), `BarePodManager` (bare-pod mode). `SetStep` provides resolved step config before each `Claim` call.
- **`PodSpecBuilder`:** Shared pod-spec assembly. Produces typed `corev1.PodSpec` from image + resolved step config. Used directly by `BarePodManager`; shared helper functions also used by `EnsureAgentTemplate` (unstructured path).
- **`resolveProposal`:** Produces `resolvedWorkflow` with cached `Agent` + `LLMProvider` per name; applies per-stage agent overrides from `ProposalApproval` via `getStageOverrideAgent`; `Execution`/`Verification` steps nil when corresponding spec sections are zero.
- **`EnsureAgentTemplate`:** Deterministic derived `SandboxTemplate` name from hash of LLM spec, model, skills, MCP servers, required secrets, step, and **base template resourceVersion**. Patches pod template env/volumes for credentials, Vertex/Bedrock/Azure extras, skills image/paths, and MCP JSON env. GC older templates labeled for same agent+step.

---

## Integration points (who calls whom)

```
cmd/main (--sandbox-mode)
  └─ controller.Setup
       ├─ bare-pod: NewBarePodManager + PodSpecBuilder
       └─ sandbox-claim: NewSandboxManager
       └─ NewSandboxAgentCaller / ProposalReconciler.SetupWithManager

ProposalReconciler.Reconcile
  └─ approval.go, resolve.go
  └─ handlers.go → results.go, rbac.go, helpers.go (status, option trim)
  └─ Agent (SandboxAgentCaller)
        └─ SandboxProvider.SetStep → SandboxProvider.Claim/WaitReady/Release
        │   ├─ SandboxManager.Claim → sandbox_templates.go (EnsureAgentTemplate)
        │   └─ BarePodManager.Claim → podspec_builder.go (PodSpecBuilder.Build)
        └─ helpers.go (query templates), schemas.go (outputSchemaForStep)
        └─ client.go (HTTP Run)
```

---

## Implementation notes (gotchas)

- **`cmd/main.go` scheme:** Registers core + `agenticv1alpha1` + `consolev1` + `openshiftv1`. Watching or applying arbitrary CRDs from tests may need extended schemes (see `reconciler_test.go`).
- **Max concurrent reconciles:** `SetupWithManager` reads cluster `ApprovalPolicy` via API reader for `MaxConcurrentProposals`, else `DefaultMaxConcurrentProposals` from API package.
- **Policy watch:** Enqueues **all** non-terminal proposals on any `ApprovalPolicy` event — can be chatty.
- **[PLANNED: OLS-3018] AgenticOLSConfig watch:** Same pattern as policy watch — enqueues all non-terminal proposals on any `AgenticOLSConfig` change. When `suspended` flips to `true`, all re-queued proposals hit the suspension guard and get terminated.
- **Workflow resolution errors:** Patched onto `ProposalConditionAnalyzed` false — see API for exact condition ordering vs `DerivePhase`.
- **`selectedOption` vs trim:** Verification uses latest analysis result’s **first** option (`Options[0]`) when resolving; execution path uses `trimNonSelectedOptions` which respects `ProposalApproval` execution option index when multiple options exist.
- **`maxAttempts`:** Combines `ApprovalPolicy.Spec.MaxAttempts` ceiling with per-approval execution override (`helpers.go`); retry semantics interact with verification failure branch in `handleVerification` (see **what/proposal-lifecycle.md**).
- **Sandbox FQDN:** Agent URL assumes port `8080` unless endpoint already has `http` prefix.
- **Logs CLI vs status:** CLI `logs` uses `SandboxInfo.ClaimName` as **pod name** in `GetLogs`; ensure cluster layout matches (if claim name ≠ pod name, logs command would need revision — operational detail for agents touching `logs.go`).
- **Tests:** `state_machine_test.go` is the primary lifecycle matrix; `testAgentCaller` implements `AgentCaller` with injectable errors/results; fake client uses `WithStatusSubresource` for proposal and result types.
