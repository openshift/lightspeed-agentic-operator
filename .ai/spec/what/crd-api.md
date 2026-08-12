# CRD API semantics (`agentic.openshift.io/v1alpha1`)

Kubernetes API surface for the agentic operator. **Lifecycle and gates** are in `run-lifecycle.md` and `approval.md`. **Sandbox runtime behavior** is in `sandbox-execution.md`.

## Behavioral Rules

1. **Group/version**: All kinds in this specification use API group `agentic.openshift.io` and version `v1alpha1`.
2. **Scope — namespaced**: `AgenticRun`, `AgenticRunApproval`, `AnalysisResult`, `ExecutionResult`, `VerificationResult`, `EscalationResult` MUST be namespace-scoped; their `metadata.namespace` is the tenant/workload namespace.
3. **Scope — cluster**: `Agent`, `LLMProvider`, `ApprovalPolicy`, and `AgenticOLSConfig` MUST be cluster-scoped; `metadata.name` is the global identifier.
4. **AgenticRun identity**: A `AgenticRun` MUST include required immutable fields per CEL: at minimum `spec.request` and `spec.analysis`. Omitting `spec.execution` or `spec.verification` means those steps do not exist for that run (see `run-lifecycle.md`).
5. **AgenticRun — `spec.request`**: Human/agent input text; immutable after creation; max length enforced by validation.
6. **AgenticRun — `spec.revisionFeedback`**: Only mutable spec field; when set/non-empty and `metadata.generation` advances beyond the analyzed condition’s `observedGeneration`, operators MUST trigger re-analysis per `run-lifecycle.md`.
7. **AgenticRun — `spec.targetNamespaces`**: Optional list of namespaces for context and RBAC targeting; immutable once set; when empty, RBAC targeting MAY fall back to namespaces declared in analysis RBAC output at execution time (see `sandbox-execution.md`).
8. **AgenticRun — `spec.analysisOutput`**: Immutable after set. `mode` defaults to full analysis schema when empty/default. `mode=Minimal` REQUIRES `schema` to be set, forbids `spec.execution` and `spec.verification`, and restricts option shape accordingly.
9. **AgenticRun — `spec.tools`**: Default `ToolsSpec` for all steps; immutable once set. Per-step `tools` on `spec.analysis` / `spec.execution` / `spec.verification` replaces the default for that step only when non-zero.
10. **AgenticRun — `spec.analysis|execution|verification`**: Immutable `AgenticRunStep` records after set. Each non-zero step MAY name `agent` (DNS subdomain) defaulting to `default` when empty; MAY carry per-step `tools`; MAY carry per-step `instructions` [PLANNED: OLS-3491].
10a. [PLANNED: OLS-3491] **AgenticRunStep — `instructions`**: Optional string (MaxLength=32768). Full **replacement** of the step’s system instructions (sandbox `systemPrompt` / batch `/input/system-prompt`). MUST NOT contain the step’s task input (`spec.request`, approved option JSON, execution output JSON). When omitted at create time, the defaulting webhook MUST materialize the effective instructions into the field (see rule 10b). Empty string after defaulting is forbidden — materialization MUST always produce a non-empty value for present steps.
10b. [PLANNED: OLS-3491] **Create-time materialization (analysis / execution / verification)**: On AgenticRun create (mutating defaulting webhook), for each present step whose `instructions` is omitted, the operator MUST write the effective instructions into `spec.<step>.instructions` using priority: (1) creator-supplied `instructions` if present, else (2) cluster default from handoff ConfigMap `instructions-<step>` when present, else (3) product built-in for that step **render-then-store** (evaluate any run-shape conditionals such as HasExecution / HasVerification, then store plain final text). After materialization, existing step immutability freezes the value for the life of the run. Operator upgrades that change built-ins MUST NOT alter already-materialized runs.
10c. [PLANNED: OLS-3491] **Escalation instructions**: There is no `spec.escalation` / per-run escalation `instructions` field in this change. Escalation resolves at escalation time: handoff ConfigMap `instructions-escalation` if present, else product built-in. Cluster changes MAY affect runs that have not yet escalated. [FOLLOW-UP RFE] Full `spec.escalation` as an `AgenticRunStep` (agent / tools / instructions) is out of scope for OLS-3491.
10d. [PLANNED: OLS-3491] **Channel split**: Step **system instructions** travel on the system channel (`systemPrompt` / `/input/system-prompt`). Step **input** travels on the query channel (`query` / `/input/query`): analysis uses `spec.request` (plus existing revision suffix behavior unchanged); execution uses approved option JSON; verification uses option + execution output JSON; escalation uses its dynamic payload (run metadata, request, result refs). Role text MUST NOT be embedded in `query`.
10e. [PLANNED: OLS-3491] **Revision feedback**: `spec.revisionFeedback` / revision context template remain query-side append behavior; not part of `instructions` override in OLS-3491.
10f. [PLANNED: OLS-3491] **Agent CR**: `Agent` remains compute tier only (model / provider / timeouts). Domain or step instructions MUST NOT be added to `Agent`.
11. **AgenticRun — `status`**: Observed-only. `status.conditions` holds map-merge conditions (types include `Analyzed`, `Executed`, `Verified`, `Denied`, `Escalated`, `EmergencyStopped`). `status.steps` holds per-step sandbox info and result refs.
12. **Phase display types**: `AgenticRunPhase` and `StepPhase` string enums in the API describe display labels only; they are not stored fields on `AgenticRun` (phase is derived — see `run-lifecycle.md`). `AgenticRunPhase` values include `EmergencyStopped` (terminal, set by kill switch — see `system-config.md`) and `NoActionRequired` [PLANNED: OLS-3268] (terminal, set when analysis determines no remediation is needed). `StepPhase` values include `PendingApproval`, `Running`, `Completed`, `Failed`, `Skipped`.
13. **Sandbox step enum**: `SandboxStep` values `Analysis`, `Execution`, `Verification`, `Escalation` identify workflow steps for approvals, sandbox labels, and policies.
14. **Agent — `spec.llmProvider`**: Required reference by name to a cluster `LLMProvider`.
15. **Agent — `spec.model`**: Required provider-specific model identifier string; validation restricts charset.
16. **Agent — `spec.timeouts`**: Optional per-step and chat timeouts in seconds with min/max bounds per field.
17. **Agent — `spec.maxTurns`**: Optional bound on tool-use turns per invocation.
18. **Agent — `spec.reasoningConfig`**: Optional freeform map (`map[string]interface{}`, JSON key `reasoningConfig`). When present, the operator MUST serialize it as `LIGHTSPEED_REASONING_CONFIG` JSON env var on the sandbox pod (see `sandbox-execution.md` rule 16a). When absent, the env var MUST be omitted and the sandbox uses SDK defaults. Contents are provider- and model-specific (e.g., Claude `thinking`/`effort`, Gemini `thinking_budget`/`thinking_level`, OpenAI `reasoning.effort`/`verbosity`); the operator passes the map as-is without validation — the sandbox and upstream SDK/API validate at invocation time. This field is aligned with the classic OLS operator's `ModelParametersSpec.ReasoningConfig` ([OLS-3452]).
19. **Agent — `status.conditions`**: Observed readiness; `Ready` condition documents whether all referenced resources (LLMProvider, Secrets) are accessible. The operator does not currently set these conditions, but the field is reserved for future health reporting.
20. **LLMProvider — discriminator**: `spec.type` MUST match exactly one embedded config: `anthropic`, `googleCloudVertex`, `openAI`, `azureOpenAI`, or `awsBedrock`; CEL enforces mutual exclusion.
21. **LLMProvider — secrets**: Each provider’s `credentialsSecret` references a `Secret` **by name** in the operator namespace (documented on fields as the deployment namespace for the operator, e.g. OpenShift Lightspeed namespace); required secret **keys** are defined per provider type on the API field comments (e.g. API key env file key names).
22. **LLMProvider — endpoints**: Optional URL overrides per provider; validation enforces HTTP/HTTPS URL shape. Azure requires `endpoint`; optional separate URL override field exists where defined.
23. **ApprovalPolicy — singleton name**: CRD validation requires `metadata.name` equals `cluster`.
24. **ApprovalPolicy — `spec.stages`**: Optional list keyed by `name` (`SandboxStep`). Each entry sets `approval` to `Automatic` or `Manual`. Stages not listed default to **Manual** per API comments.
25. [REMOVED] `ApprovalPolicy.spec.maxAttempts` has been removed. Execution runs exactly once per analysis iteration; verification failure escalates directly.
26. **ApprovalPolicy — `spec.maxConcurrentRuns`**: Caps concurrent reconciles when positive; operator falls back to a default constant when unset.
27. **AgenticRunApproval — pairing**: For each `AgenticRun`, the controller MUST create (if missing) a same-named `AgenticRunApproval` in the same namespace with controller owner reference to the `AgenticRun`.
28. **AgenticRunApproval — `spec.stages`**: Append-only map list keyed by `type` (`ApprovalStageType`). Each stage carries a discriminated union: exactly one of `analysis`, `execution`, `verification`, `escalation` MUST be present matching `type`. Optional `decision` may be `Approved` (default when omitted) or `Denied`; `Denied` is terminal per API rules.
29. **AgenticRunApproval — immutability CEL**: Stages cannot be removed; decisions cannot change once set.
30. **Execution approval fields**: `spec.stages[].execution.option` selects 0-based analysis option index; `agent` overrides the `AgenticRun` step’s agent when set.
31. **AnalysisResult**: `spec.agenticRunName` immutable; `status.options` holds `RemediationOption` entries; `status.sandbox` and `status.failureReason` optional; conditions use shared result condition types. [PLANNED: OLS-3268] `status.actionRequired` (bool) indicates whether remediation is needed; `status.diagnosis` (top-level `DiagnosisResult`: summary, rootCause) captures the agent’s explanation when no action is required. When `actionRequired` is false, `status.options` may be empty (`minItems: 0`).
32. **ExecutionResult**: `status.actionsTaken`, optional `failureReason`, `sandbox`.
33. **VerificationResult**: `status.checks`, `status.summary`, optional `failureReason`, `sandbox`.
34. **EscalationResult**: `status.summary`, `status.content`, optional `failureReason`, `sandbox`.
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

## Configuration Surface (by path)

### AgenticRun
- `metadata.*`
- `spec.request`, `spec.targetNamespaces`, `spec.revisionFeedback`, `spec.analysisOutput`, `spec.tools`, `spec.analysis`, `spec.execution`, `spec.verification`
- `spec.analysis.instructions`, `spec.execution.instructions`, `spec.verification.instructions` [PLANNED: OLS-3491]
- `status.conditions`, `status.steps.analysis|execution|verification|escalation.*`

### Agent
- `metadata.name`, `spec.llmProvider.name`, `spec.model`, `spec.reasoningConfig`, `spec.timeouts.*`, `spec.maxTurns`, `status.conditions`

### LLMProvider
- `metadata.name`, `spec.type`, `spec.anthropic.*`, `spec.googleCloudVertex.*`, `spec.openAI.*`, `spec.azureOpenAI.*`, `spec.awsBedrock.*`

### ApprovalPolicy
- `metadata.name` (must be `cluster`), `spec.stages[]`, `spec.maxConcurrentRuns`

### AgenticOLSConfig
- `metadata.name` (must be `cluster`), `spec.suspended`, `spec.templog`
- `spec.templog` (bool, default `true`): When `true` or absent, the lightspeed-operator deploys a custom OTel Collector for temporary audit log storage in PostgreSQL. See `templog.md`.
- `status.conditions` — condition types: `Suspended`
- See `system-config.md` for full behavioral rules

### AgenticRunApproval
- `metadata.name`, `metadata.namespace`, `spec.stages[]`, `status.stages[]`

### AnalysisResult / ExecutionResult / VerificationResult / EscalationResult
- `metadata.name`, `metadata.namespace`, `spec.*`, `status.*`

### Shared / embedded types
- `AgenticRunStep`: `agent`, `tools`, `instructions` [PLANNED: OLS-3491]
- `ToolsSpec`: `skills[]`, `mcpServers[]`, `requiredSecrets[]` (`disableDefaultMCP` deferred — see rule 37a / OLS-3594)
- `SkillsSource`: `image`, `paths[]`
- `SecretRequirement`: `name`, `description`, `mountAs.*`
- `StepResultRef`: `name`, `outcome`
- `SandboxInfo`: `claimName`, `namespace`

## Constraints

- Cross-object references (`Agent`, `LLMProvider`, `Secret`) MUST resolve or reconciliation surfaces resolution errors as workflow failures per controller behavior.
- **User-facing policy modes** in product docs mentioning “always approve / require approval for execution only” MUST map onto the actual API values `Automatic` and `Manual` plus stage lists; there is no separate enum for those phrases in the CRD.

## Planned Changes

- [PLANNED: OLS-2940] Autonomous workflow CRD migrations may rename or reshape fields; specs MUST be updated when `v1alpha1` changes.
- [PLANNED: OLS-3491] Configurable per-step `instructions` on `AgenticRunStep` + cluster defaults via OLSConfig `spec.agenticOLS.instructions` / handoff ConfigMap. See rules 10a–10f and `sandbox-execution.md`. Follow-up RFE: full `spec.escalation` as `AgenticRunStep`.
- ~~[PLANNED: OLS-2894] Explicit **Agent** fields for per-step system prompts~~ **Superseded by OLS-3491** — instructions live on `AgenticRunStep` / cluster `agenticOLS`, not on `Agent` (compute tier).
- [OLS-3328] Add `spec.templog` to `AgenticOLSConfig` CRD for temporary audit log storage.
- [DONE: OLS-3295] Renamed `Proposal` → `AgenticRun`, `ProposalApproval` → `AgenticRunApproval` CRD kinds and all associated field names, RBAC resources, and label keys.
- [PLANNED: OLS-3594] Optional `disableDefaultMCP` (and related auto-injection) — deferred; blocked by OLS-3526 and OLS-3572. Not near-term.
