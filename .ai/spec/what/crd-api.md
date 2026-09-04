# CRD API semantics (`agentic.openshift.io/v1alpha1`)

Kubernetes API surface for the agentic operator. **Lifecycle and gates** are in `run-lifecycle.md` and `approval.md`. **Sandbox runtime behavior** is in `sandbox-execution.md`.

## Behavioral Rules

1. **Group/version**: All kinds in this specification use API group `agentic.openshift.io` and version `v1alpha1`.
2. **Scope — namespaced**: `AgenticRun`, `AgenticRunApproval`, `AnalysisResult`, `ExecutionResult`, `VerificationResult`, `EscalationResult` MUST be namespace-scoped; their `metadata.namespace` is the tenant/workload namespace.
3. **Scope — cluster**: `Agent`, `LLMProvider`, `ApprovalPolicy`, and `AgenticOLSConfig` MUST be cluster-scoped; `metadata.name` is the global identifier.
4. **AgenticRun identity**: A `AgenticRun` MUST include required immutable fields per CEL: at minimum `spec.request` and `spec.analysis`. Omitting `spec.execution` or `spec.verification` means those steps do not exist for that run (see `run-lifecycle.md`).
5. **AgenticRun — `spec.request`**: Human/agent input text; immutable after creation; max length enforced by validation.
6. **AgenticRun — `spec.revisionFeedback`**: User-mutable spec field for iterative feedback; when set/non-empty and `metadata.generation` advances beyond the analyzed condition’s `observedGeneration`, operators MUST trigger re-analysis per `run-lifecycle.md`. `spec.ttlAfterTerminal` (rule 6a) and one-way `spec.cancelled` (rule 6f) are the other mutable spec fields; the controller MUST advance `Analyzed.observedGeneration` in the same write whenever it patches `ttlAfterTerminal`, so that its own generation bump is never misread as an outstanding revision request.
6a. **AgenticRun — `spec.ttlAfterTerminal`**: Optional time-to-live in seconds after the run reaches a terminal state (see `run-lifecycle.md` rule 4 for the terminal set), after which the operator deletes the `AgenticRun` CR (Kubernetes GC cascades to owned resources via owner references). Mutable — adapters or admins MAY pre-set it before the run reaches terminal state; once terminal, if unset, the operator stamps it from `AgenticOLSConfig.spec.lifecycle.terminalTTL` (cluster default) without overwriting a pre-set value. `0` explicitly disables auto-deletion for that run. When omitted and no cluster default exists, no automatic deletion occurs (backwards-compatible). See `run-lifecycle.md` rule 23.
6b. **AgenticRun — `status.terminalTime`**: Timestamp the operator stamps once, the first time a run reaches a terminal state; not updated again while the run remains terminal. Cleared by the revision handler when a terminal run re-enters analysis (rule 6/23), so a subsequent terminal phase gets a fresh timestamp instead of reusing the prior terminal event's. Used with `spec.ttlAfterTerminal` to compute the deletion deadline.
6c. [PLANNED: OLS-3661] **`TokenUsage` struct**: Embedded struct with two required fields: `inputTokens` (int64, >= 0) and `outputTokens` (int64, >= 0). Both fields are required whenever the struct is present. `TokenUsage` carries token consumption data at the granularity of a single step (on Result CRs) or an entire run (on `AgenticRunStatus`).
6d. [PLANNED: OLS-3661] **AgenticRun — `status.tokenUsage`**: Optional `TokenUsage` (rule 6c). Cumulative input and output token counts across all completed steps of the run. The operator sets this field by aggregating `status.tokenUsage` from individual Result CRs (see `sandbox-execution.md` rule 43.1); users MUST NOT modify it. When no steps have completed, the field is absent (not zero-valued).
6e. [PLANNED: OLS-3661] **Result CR — `status.tokenUsage`**: All Result CR kinds (`AnalysisResult`, `ExecutionResult`, `VerificationResult`, `EscalationResult`) gain an optional `status.tokenUsage` (`TokenUsage`, rule 6c) field. The sandbox populates this field with per-step token counts before publishing the Result CR (see sandbox `run-api.md` rule 21). The operator reads it during result processing for run-level aggregation (see `sandbox-execution.md` rule 43.1).
6f. [PLANNED: OLS-3298] **AgenticRun — `spec.cancelled`**: Optional boolean with effective default `false`. A caller with `patch` permission on `agenticruns` MAY set it from absent/`false` to `true`; the value MUST be one-way and MUST NOT be cleared. On a non-terminal run, it requests immediate termination and the run derives as `Failed` through the phase-relevant condition with `status=False` and reason `CancelledByUser`. It is not a new phase, condition type, or Result CR. Global suspension takes precedence when both signals are pending (see `system-config.md` and `agentic-run-termination.md`).
7. **AgenticRun — `spec.targetNamespaces`**: Optional list of namespaces for context and RBAC targeting; immutable once set; when empty, RBAC targeting MAY fall back to namespaces declared in analysis RBAC output at execution time (see `sandbox-execution.md`).
8. **AgenticRun — `spec.analysisOutput`**: Immutable after set. `mode` defaults to full analysis schema when empty/default. `mode=Minimal` REQUIRES `schema` to be set, forbids `spec.execution` and `spec.verification`, and restricts option shape accordingly.
9. **AgenticRun — `spec.tools`**: Default `ToolsSpec` for all steps; immutable once set. Per-step `tools` on `spec.analysis` / `spec.execution` / `spec.verification` replaces the default for that step only when non-zero.
10. **AgenticRun — `spec.analysis|execution|verification`**: Immutable `AgenticRunStep` records after set. Each non-zero step MAY name `agent` (DNS subdomain) defaulting to `default` when empty; MAY carry per-step `tools`.
10a–10g. ~~[SUPERSEDED by OLS-3491 redesign]~~ Rules 10a–10g removed and replaced. Per-step instructions now live on the `Agent` CR, not on `AgenticRunStep` or `OLSConfig`. See rules 10h–10l below.
10h. [DONE: OLS-4098] **Agent — `spec.instructions`**: Optional `AgentInstructions` struct with per-step `StepInstructions` fields: `analysis`, `execution`, `verification`, `escalation`. Each `StepInstructions` has two optional strings (MaxLength=32768): `systemPrompt` (LLM system message → `/input/system-prompt`; default when empty: sandbox built-in `"You are an AI agent."`) and `userPrompt` (Go template replacing built-in query template → `/input/query`; supports the same template variables as the built-in templates, e.g. `{{.Request}}`, `{{.HasExecution}}`, `{{.HasVerification}}` for analysis, `{{.OptionJSON}}` for execution, `{{.OptionJSON}}`/`{{.ExecutionJSON}}` for verification; default when empty: built-in templates in `controller/agenticrun/templates/*.tmpl`). When absent or empty, product built-in defaults are used. The Agent becomes a complete "compute + behavior" entity.
10i. [DONE: OLS-4098] **Instruction resolution at sandbox setup**: `buildInputConfigMap` resolves both prompts via `resolvePrompts`. For `systemPrompt`: Agent CR value or empty (sandbox defaults to `"You are an AI agent."`). For `userPrompt`: Agent CR Go template or built-in template file (`templates/*.tmpl`). Both rendered with the same template data as built-in templates (e.g. `{{.Request}}`, `{{.HasExecution}}`). The `system-prompt` key is only included in the ConfigMap when non-empty.
10j. [DONE: OLS-4098] **Channel split**: `systemPrompt` travels on `/input/system-prompt`. `userPrompt` (rendered) travels on `/input/query`. Analysis query uses `spec.request`; execution uses approved option JSON; verification uses option + execution output JSON; escalation uses run metadata, request, and result refs. Revision feedback appends to query-side via `buildRevisionContext`.
10k. [DONE: OLS-4098] **Revision feedback**: `spec.revisionFeedback` / revision context template remain query-side append behavior; not part of `instructions`.
10l. [DONE: OLS-4098] **Precedence**: `Agent.spec.instructions.<step>.{systemPrompt,userPrompt}` (when non-empty) > product built-in. Two layers only. Different Agents carry different instructions for different use cases.
11. **AgenticRun — `status`**: Observed-only. `status.conditions` holds map-merge conditions (types include `Analyzed`, `Executed`, `Verified`, `Denied`, `Escalated`, `EmergencyStopped`). `status.steps` holds per-step sandbox info and result refs.
12. **Phase display types**: `AgenticRunPhase` and `StepPhase` string enums in the API describe display labels only; they are not stored fields on `AgenticRun` (phase is derived — see `run-lifecycle.md`). `AgenticRunPhase` values include `EmergencyStopped` (terminal, set by kill switch — see `system-config.md`). When analysis determines no remediation is needed, the run derives as `Completed` with `Analyzed` condition reason `NoActionRequired`. `StepPhase` values include `PendingApproval`, `Running`, `Completed`, `Failed`, `Skipped`.
13. **Sandbox step enum**: `SandboxStep` values `Analysis`, `Execution`, `Verification`, `Escalation` identify workflow steps for approvals, sandbox labels, and policies.
14. **Agent — `spec.llmProvider`**: Required reference by name to a cluster `LLMProvider`.
15. **Agent — `spec.model`**: Required provider-specific model identifier string; validation restricts charset.
16. **[PLANNED: OLS-3743] Agent — `spec.timeouts`**: Optional per-step agent invocation budgets in seconds. Fields are `analysisSeconds`, `executionSeconds`, `verificationSeconds`, and `escalationSeconds`, each validated from 1 through 3600. The operator resolves omitted fields to 600 seconds except verification, which defaults to 1800 seconds. A budget covers the complete agent invocation, including model calls, tools, MCP calls, and processing across turns. `chatSeconds` is removed because providers do not expose one consistent per-turn timeout.
17. **[PLANNED: OLS-3743] Agent — `spec.maxTurns`**: Optional upper bound on provider iteration per invocation, validated from 1 through 500. The operator resolves an omitted value to 200. Provider mappings differ (DeepAgents recursion limit, Gemini maximum LLM calls, OpenAI maximum turns), but all enforce the common upper-bound contract.
18. **Agent — `spec.reasoningConfig`**: Optional freeform map (`map[string]interface{}`, JSON key `reasoningConfig`). When present, the operator MUST serialize it as `LIGHTSPEED_REASONING_CONFIG` JSON env var on the sandbox pod (see `sandbox-execution.md` rule 16a). When absent, the env var MUST be omitted and the sandbox uses SDK defaults. Contents are provider- and model-specific (e.g., Claude `thinking`/`effort`, Gemini `thinking_budget`/`thinking_level`, OpenAI `reasoning.effort`/`verbosity`); the operator passes the map as-is without validation — the sandbox and upstream SDK/API validate at invocation time. This field is aligned with the classic OLS operator's `ModelParametersSpec.ReasoningConfig` ([OLS-3452]).
19. **Agent — `status.conditions`**: Observed readiness; `Ready` condition documents whether all referenced resources (LLMProvider, Secrets) are accessible. The operator does not currently set these conditions, but the field is reserved for future health reporting.
20. **LLMProvider — discriminator**: `spec.type` MUST match exactly one embedded config: `anthropic`, `googleCloudVertex`, `openAI`, `azureOpenAI`, or `awsBedrock`; CEL enforces mutual exclusion.
21. **LLMProvider — secrets**: Each provider’s `credentialsSecret` references a `Secret` **by name** in the operator namespace (documented on fields as the deployment namespace for the operator, e.g. OpenShift Lightspeed namespace); required secret **keys** are defined per provider type on the API field comments (e.g. API key env file key names).
21a. **LLMProvider — `azureOpenAI` credential validation** [OLS-3050]: An `azureOpenAI` `credentialsSecret` MUST contain **either** `apitoken` (API-key auth) **or** all three of `client_id`, `tenant_id`, `client_secret` (Entra ID / service-principal auth). A secret with an incomplete service-principal set (one or two of the three keys) and no `apitoken` MUST be rejected during credential validation with a descriptive error. This mirrors the classic operator (`lightspeed-operator/.ai/spec/what/security.md` rule 11) and matches the credential shape the sandbox resolves (`lightspeed-agentic-sandbox/.ai/spec/what/configuration.md` rule 9a). The keys flow to the sandbox unchanged via the unconditional credential mount (`sandbox-execution.md` rule 16); no operator wiring code changes are required, only this validation.
21b. **LLMProvider — `awsBedrock` credential validation** [OLS-4092]: An `awsBedrock` `credentialsSecret` MUST contain both `aws_access_key_id` and `aws_secret_access_key`; `role_arn` is optional and, when present, selects STS assume-role for short-lived credentials. A secret missing either required key MUST be rejected during credential validation with a descriptive error. This matches the credential shape the sandbox resolves (`lightspeed-agentic-sandbox/.ai/spec/what/configuration.md` rule 9b) and the classic Bedrock auth (IAM keys + optional `role_arn`, OLS-1895). The keys flow to the sandbox unchanged via the unconditional credential mount (`sandbox-execution.md` rule 16); this validation changes no operator wiring and does not touch the Anthropic-on-Bedrock model path.
22. **LLMProvider — endpoints**: Optional URL overrides per provider; validation enforces HTTP/HTTPS URL shape. Azure requires `endpoint`; optional separate URL override field exists where defined.
23. **ApprovalPolicy — singleton name**: CRD validation requires `metadata.name` equals `cluster`.
24. **ApprovalPolicy — `spec.stages`**: Optional list keyed by `name` (`SandboxStep`). Each entry sets `approval` to `Automatic` or `Manual`. Unlisted stages default to **Manual** per API comments, except the read-only `Escalation` step, which defaults to **Automatic** unless listed explicitly as `Manual`.
25. [REMOVED] `ApprovalPolicy.spec.maxAttempts` has been removed. Execution runs exactly once per analysis iteration; verification failure escalates directly.
26. **ApprovalPolicy — `spec.maxConcurrentRuns`**: Caps concurrent reconciles when positive; operator falls back to a default constant when unset.
27. **AgenticRunApproval — pairing**: For each `AgenticRun`, the controller MUST create (if missing) a same-named `AgenticRunApproval` in the same namespace with controller owner reference to the `AgenticRun`.
28. **AgenticRunApproval — `spec.stages`**: Append-only map list keyed by `type` (`ApprovalStageType`). Each stage carries a discriminated union: exactly one of `analysis`, `execution`, `verification`, `escalation` MUST be present matching `type`. Optional `decision` may be `Approved` (default when omitted) or `Denied`; `Denied` is terminal per API rules.
29. **AgenticRunApproval — immutability CEL**: Stages cannot be removed; decisions cannot change once set.
30. **Execution approval fields**: `spec.stages[].execution.option` selects 0-based analysis option index; `agent` overrides the `AgenticRun` step’s agent when set.
31. **AnalysisResult**: `spec.agenticRunName` immutable; `status.options` holds `RemediationOption` entries; `status.sandbox`, `status.failureReason`, and `status.tokenUsage` (rule 6e) optional; conditions use shared result condition types. [PLANNED: OLS-3268] `status.actionRequired` (bool) indicates whether remediation is needed; `status.diagnosis` (top-level `DiagnosisResult`: summary, rootCause) captures the agent’s explanation when no action is required. When `actionRequired` is false, `status.options` may be empty (`minItems: 0`).
32. **ExecutionResult**: `status.actionsTaken`, optional `failureReason`, `sandbox`, `tokenUsage` (rule 6e).
33. **VerificationResult**: `status.checks`, `status.summary`, optional `failureReason`, `sandbox`, `tokenUsage` (rule 6e).
34. **EscalationResult**: `status.summary`, `status.content`, optional `failureReason`, `sandbox`, `tokenUsage` (rule 6e).
35. **RemediationOption**: Cohesion rules require `diagnosis` and `remediationPlan` to be paired when present; `components` holds schemaless JSON for adapter data shaped by `spec.analysisOutput.schema`. Each action in `remediationPlan.actions` includes `command` (required, 1-4096 chars, exact bash command using kubectl/oc), `type` (required, 1-256 chars, phase category: pre-check, mutation, wait, post-check), and `description` (required, 1-4096 chars). All three fields are required on `ProposedAction`. [OLS-3441]
36. **RBACResult / RBACRule**: Analysis MAY request namespace-scoped and cluster-scoped rules with verb/apigroup/resource metadata and mandatory `justification`; `namespace` on rules MUST align with run targeting rules (validated at runtime by policy engine per field comments).
37. **ToolsSpec**: MAY include `skills` (unique images), `mcpServers` (unique names), and `requiredSecrets` (unique names). `SkillsSource.image` MUST be a valid pullspec; optional `paths` restrict mounted subtrees.
37a. [PLANNED: OLS-3594] **ToolsSpec — `disableDefaultMCP`**: Deferred with default ocp-mcp auto-injection. Not part of the current API. If auto-injection is later adopted, an opt-out field may be added; details land with that implementation.
38. **SecretRequirement**: Names a namespace-local `Secret`; `mountAs` discriminates `EnvVar` vs `FilePath` with required nested config per type.
39. **MCPHeaderValueSource**: Discriminated by `type`; `Secret` requires nested `secret` name reference.
40. **Result CR ownership**: Result CRs MUST declare controller `ownerReferences` to their `AgenticRun` for GC; naming follows operator conventions (see `sandbox-execution.md` for when they are created).
41. **Label conventions**: Operator uses labels for run name, step, component, and managed template markers (exact keys are implementation-specific; behavior: selectors for GC/list, not duplicated here).
42. **CEL immutability (AgenticRun): Enforced transitions include: `request`, `targetNamespaces`, `analysisOutput`, `tools`, `analysis`, `execution`, `verification` immutability after initial set as encoded in API markers.
43. **AgenticOLSConfig — singleton name**: CRD validation requires `metadata.name` equals `cluster` (same pattern as `ApprovalPolicy`).
44. **AgenticOLSConfig — `spec.suspended`**: Bool, optional, default `false`. When `true`, halts all agentic operations cluster-wide and terminates in-flight runs with `EmergencyStopped` condition. See `system-config.md` for full semantics.
45. **AgenticOLSConfig — absence**: When no `AgenticOLSConfig` CR exists, the system MUST behave as if `spec.suspended` is `false`.
46. **AgenticOLSConfig — status subresource**: `AgenticOLSConfig` MUST have a `/status` subresource with `conditions` array (`metav1.Condition`). Condition type `Suspended` tracks whether the operator has acknowledged and acted on `spec.suspended`. See `system-config.md` rules 5a–5e for full semantics.
47. **AgenticOLSConfig — status RBAC**: The operator’s service account MUST have `get`, `update`, `patch` on `agenticolsconfigs/status` in addition to existing permissions on the main resource.
48. **AgenticOLSConfig — `spec.lifecycle.terminalTTL`**: Optional cluster-wide default time-to-live in seconds for terminal `AgenticRun` resources, applied only when a given run's `spec.ttlAfterTerminal` is unset (per-run override always wins — see rule 6a). `spec.lifecycle` requires at least one property when present (`minProperties: 1`) and is omitted entirely from the object when zero-valued. When omitted or no `AgenticOLSConfig` exists, no cluster-wide default gets stamped, so runs relying on the default are never auto-deleted — but this does NOT affect runs that already carry a pre-set `spec.ttlAfterTerminal` independently of the cluster config; those are still deleted on schedule regardless of whether `AgenticOLSConfig` exists.

## Configuration Surface (by path)

### AgenticRun
- `metadata.*`
- `spec.request`, `spec.targetNamespaces`, `spec.revisionFeedback`, `spec.cancelled`, `spec.ttlAfterTerminal`, `spec.analysisOutput`, `spec.tools`, `spec.analysis`, `spec.execution`, `spec.verification`
- ~~`spec.analysis.instructions`, `spec.execution.instructions`, `spec.verification.instructions`~~ [REMOVED: OLS-3491 redesign — instructions live on Agent CR]
- `status.conditions`, `status.steps.analysis|execution|verification|escalation.*`, `status.terminalTime`, `status.tokenUsage` [PLANNED: OLS-3661]

### Agent
- `metadata.name`, `spec.llmProvider.name`, `spec.model`, `spec.reasoningConfig`, `spec.timeouts.*`, `spec.maxTurns`, `spec.instructions.{analysis,execution,verification,escalation}.{systemPrompt,userPrompt}` [OLS-4098], `status.conditions`

### LLMProvider
- `metadata.name`, `spec.type`, `spec.anthropic.*`, `spec.googleCloudVertex.*`, `spec.openAI.*`, `spec.azureOpenAI.*`, `spec.awsBedrock.*`

### ApprovalPolicy
- `metadata.name` (must be `cluster`), `spec.stages[]`, `spec.maxConcurrentRuns`

### AgenticOLSConfig
- `metadata.name` (must be `cluster`), `spec.suspended`, `spec.templog`, `spec.lifecycle.terminalTTL`
- `spec.templog` (bool, default `true`): When `true` or absent, the lightspeed-operator deploys a custom OTel Collector for temporary audit log storage in PostgreSQL. See `templog.md`.
- `spec.lifecycle.terminalTTL` (optional int, seconds): Cluster-wide default TTL for terminal `AgenticRun` deletion; see rule 48.
- `status.conditions` — condition types: `Suspended`
- See `system-config.md` for full behavioral rules

### AgenticRunApproval
- `metadata.name`, `metadata.namespace`, `spec.stages[]`, `status.stages[]`

### AnalysisResult / ExecutionResult / VerificationResult / EscalationResult
- `metadata.name`, `metadata.namespace`, `spec.*`, `status.*`

### Shared / embedded types
- `AgenticRunStep`: `agent`, `tools`
- `AgentInstructions`: `analysis`, `execution`, `verification`, `escalation` (each with `systemPrompt`, `userPrompt`) [DONE: OLS-4098]
- `ToolsSpec`: `skills[]`, `mcpServers[]`, `requiredSecrets[]` (`disableDefaultMCP` deferred — see rule 37a / OLS-3594)
- `SkillsSource`: `image`, `paths[]`
- `SecretRequirement`: `name`, `description`, `mountAs.*`
- `StepResultRef`: `name`, `outcome`
- `SandboxInfo`: `claimName`, `namespace`
- `TokenUsage`: `inputTokens`, `outputTokens` [PLANNED: OLS-3661]

## Constraints

- Cross-object references (`Agent`, `LLMProvider`, `Secret`) MUST resolve or reconciliation surfaces resolution errors as workflow failures per controller behavior.
- **User-facing policy modes** in product docs mentioning “always approve / require approval for execution only” MUST map onto the actual API values `Automatic` and `Manual` plus stage lists; there is no separate enum for those phrases in the CRD.

## Planned Changes

- [PLANNED: OLS-2940] Autonomous workflow CRD migrations may rename or reshape fields; specs MUST be updated when `v1alpha1` changes.
- [DONE: OLS-4098] Configurable per-step `instructions` on `Agent` CR with `systemPrompt` and `userPrompt` per step. Two-layer precedence: Agent instructions > product built-in. `systemPrompt` → `/input/system-prompt`; `userPrompt` (Go template) → `/input/query`. See rules 10h–10l.
- [OLS-3328] Add `spec.templog` to `AgenticOLSConfig` CRD for temporary audit log storage.
- [DONE: OLS-3295] Renamed `Proposal` → `AgenticRun`, `ProposalApproval` → `AgenticRunApproval` CRD kinds and all associated field names, RBAC resources, and label keys.
- [PLANNED: OLS-3594] Optional `disableDefaultMCP` (and related auto-injection) — deferred; blocked by OLS-3526 and OLS-3572. Not near-term.
- [DONE: OLS-3566] Added `AgenticOLSConfig.spec.lifecycle.terminalTTL` (cluster default) and `AgenticRun.spec.ttlAfterTerminal` / `status.terminalTime` (per-run override + stamp) for automatic terminal-run cleanup. Added `oc agentic run cleanup` CLI command for manual batch cleanup (see `how/cli.md`).
- [PLANNED: OLS-3661] `TokenUsage` struct (`inputTokens`, `outputTokens`) on Result CR statuses and `AgenticRunStatus.tokenUsage` for run-level aggregation. Sandbox populates per-step counts; operator aggregates on Result CR completion. See rules 6c–6e, 31–34, and `sandbox-execution.md` rule 43.1.
- [PLANNED: OLS-3743] Replace unwired `spec.timeouts.chatSeconds` with `spec.timeouts.escalationSeconds`; wire all step budgets and `spec.maxTurns` to the batch sandbox with operator-owned defaults.
- [PLANNED: OLS-3298] Add one-way `AgenticRun.spec.cancelled` with `Failed / CancelledByUser` semantics and global-suspension precedence. See `agentic-run-termination.md`.
- [PLANNED: OLS-3050] Validate `azureOpenAI` `credentialsSecret` for either `apitoken` or the full Entra ID service-principal key set (`client_id`, `tenant_id`, `client_secret`). See rule 21a.
- [PLANNED: OLS-4092] Validate `awsBedrock` `credentialsSecret` for `aws_access_key_id` + `aws_secret_access_key` (optional `role_arn` for STS assume-role). See rule 21b.
