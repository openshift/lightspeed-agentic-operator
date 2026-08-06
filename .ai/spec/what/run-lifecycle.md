# Run lifecycle (state machine)

Behavioral specification for the `AgenticRun` resource lifecycle. **Approval gates, sandbox calls, and RBAC** are defined in `approval.md` and `sandbox-execution.md`. **Field semantics** are in `crd-api.md`.

## Behavioral Rules

1. **Source of truth**: `status.conditions` (Kubernetes conditions keyed by `type`) is authoritative. The **phase** is a derived display value only; it is not persisted as its own field.
2. **Phases**: The system MUST derive exactly one phase label from `status.conditions` using the algorithm in rule 9 (and precedence rules 10–11). Valid labels: `Pending`, `Analyzing`, `Proposed`, `Executing`, `Verifying`, `Completed`, `Failed`, `Denied`, `Escalating`, `Escalated`, `EmergencyStopped`, `NoActionRequired`.
3. **Condition types (run-level)**: The workflow uses `Analyzed`, `Executed`, `Verified`, `Denied`, `Escalated`, `EmergencyStopped` (string values as defined on the API). Status values are `True`, `False`, or `Unknown`.
4. **Terminal phases**: `Completed`, `Denied`, `Escalated`, `Failed`, `EmergencyStopped`, and `NoActionRequired` are terminal for reconciliation progression. After `Completed`, `Denied`, `Escalated`, `EmergencyStopped`, or `NoActionRequired`, the controller MUST stop active work and MAY release sandbox claims when present. `Failed` triggers failure cleanup behaviors (see `sandbox-execution.md` for RBAC cleanup interactions). `EmergencyStopped` indicates the run was terminated by the system kill switch (see `system-config.md`). `NoActionRequired` indicates the analysis agent determined no remediation is needed (see rule 9).
5. **Workflow shape**: `spec.analysis` is always required. `spec.execution` and `spec.verification` MAY be omitted; omission skips those steps subject to rules 20–22.
6. **Revision loop**: If `spec.revisionFeedback` is non-empty AND `metadata.generation` is greater than `Analyzed.observedGeneration`, the system MUST treat the run as needing **re-analysis** before continuing downstream steps. Re-analysis MUST append revision context to the user-visible request text (after `spec.request`), then reset execution/verification/escalation progress as implemented for revision handling, and MUST NOT advance execution until the new analysis completes. Revision feedback is supported from the `NoActionRequired` terminal phase — patching `spec.revisionFeedback` resets conditions and re-runs analysis.
7. **Verification failure → escalation**: When `spec.verification` is present, after a successful execution the verification step MAY fail **objectively** if the agent reports failure **or** any verification check records a non-pass outcome (even when a coarse success flag might otherwise read true). On verification failure, the system MUST NOT retry execution. The system MUST set `Verified` to `False` with reason `VerificationFailed` and MUST set `Escalated` to `Unknown`, entering the escalating phase. The escalation summary includes the execution result and failed verification result so a human operator can assess what happened.
8. **No execution retries**: The operator does not re-execute remediation after verification failure. Convergence-dependent checks (alerts, metrics, pod readiness) are handled within the verification agent's single sandbox call via prompt-guided wait-and-retry. This avoids the risk of re-executing non-idempotent remediations against a cluster in an unknown intermediate state.
9. **DerivePhase — precedence (first match in order)**:
   - If `EmergencyStopped` exists with status `True` → phase `EmergencyStopped`.
   - Else if `Escalated` exists with status `True` → phase `Escalated`.
   - Else if `Denied` exists with status `True` → phase `Denied`.
   - Else if `Escalated` exists → if status is `Unknown` → phase `Escalating`; otherwise → phase `Failed`.
   - Else evaluate `Verified` if present:
     - If `Verified` is `True` → phase `Completed`.
     - If `Verified` is `Unknown` → phase `Verifying`.
     - If `Verified` is `False` → phase `Failed` (unless `Escalated` is set, which takes precedence per rule ordering).
   - Else evaluate `Executed` if present:
     - If `Executed` is `True` → phase `Verifying`.
     - If `Executed` is `Unknown` → phase `Executing`.
     - If `Executed` is `False` → phase `Failed`.
   - Else evaluate `Analyzed` if present:
     - If `Analyzed` is `True` AND reason is `NoActionRequired` → phase `NoActionRequired`.
     - If `Analyzed` is `True` → phase `Proposed`.
     - If `Analyzed` is `Unknown` → phase `Analyzing`.
     - If `Analyzed` is `False` → phase `Failed`.
   - Else → phase `Pending`.
10. **EmergencyStopped vs other terminals in derivation**: `EmergencyStopped=True` MUST win over all other conditions because derivation checks it first. `Escalated=True` MUST win over `Denied=True` if both are present because derivation checks complete escalation before denial. Otherwise `Denied=True` MUST win over non-terminal progress (`Analyzed`, `Executed`, `Verified` combinations).
11. **Advisory completion**: If execution is absent and verification is absent, after successful analysis the controller MAY set `Executed` and `Verified` to `True` with skip reasons such that the derived phase is `Completed`.
12. **Trust mode completion**: If execution is present and verification is absent, after successful execution the controller MUST set `Verified` to `True` with a skip reason such that the derived phase is `Completed`.
13. **Skipped steps**: `Executed=True` with skip reason and `Verified=True` with skip reason together MUST derive `Completed` when that is the intended advisory outcome per tests and valid condition combinations.
14. **Step phases (display vocabulary)**: The API defines per-step display phases `PendingApproval`, `Running`, `Completed`, `Failed`, `Skipped` (see `crd-api.md`). A conforming implementation SHOULD map: `Running` ↔ corresponding run-level step condition `Unknown` with in-progress reason; `Completed` ↔ `True` with complete/passed/skipped reason as applicable; `Failed` ↔ `False`; `Skipped` ↔ `True` with skipped reason on execution/verification where applicable; `PendingApproval` ↔ step not yet active while run phase waits on approval for that step (see `approval.md`).
14a. **[OLS-3066] Step-level conditions**: The controller MUST populate `status.steps.<step>.conditions` for each step. These conditions serve both observability (console/CLI can show step progress) and re-entry logic (controller uses them to determine what to do next in the async reconcile pattern — see `sandbox-execution.md` rules 43–43e). Condition reasons:

| Status | Reason | Meaning |
|---|---|---|
| `Unknown` | `WaitingForSandbox` | Pod/SandboxClaim created, waiting for pod to start |
| `Unknown` | `Running` | Pod is running, agent is working |
| `True` | `Succeeded` | Result CR exists with `success: true` |
| `False` | `AgentFailed` | Result CR exists with `success: false` |
| `False` | `SandboxTimeout` | Per-step timeout fired, pod killed (see `sandbox-execution.md` rule 40) |
| `False` | `SandboxFailed` | Pod exited without creating Result CR |
| `False` | `ImagePullFailed` | Pod stuck in ImagePullBackOff |
15. **Success**: `Verified=True` MUST yield `Completed` once rule 9 reaches the `Verified` branch, unless an earlier branch already returned `Escalated` or `Denied` per rules 9–10.
16. **Step failure**: Any of `Analyzed`, `Executed`, or `Verified` with status `False` and reasons that are not the dedicated retrying-execution reason MUST yield `Failed` when reached by the derivation order in rule 9 (unless superseded by `Escalated` / `Denied` per rules 9–10).
17. **Escalation failure**: `Escalated` with status `False` MUST yield `Failed` once rule 9 evaluates the `Escalated` presence branch (non-`True`, non-`Unknown`).
18. **Result CR linkage**: Each analysis/execution/verification/escalation attempt SHOULD append a `status.steps.*.results[]` entry naming the corresponding result resource with an outcome matching agent success/failure for that attempt. **Exception:** when the execution agent reports `success=false` but all mutating actions succeeded (only inline verification checks failed), the controller MUST override the outcome to `Succeeded` and proceed to the verification step. Observation action types (`pre-check`, `post-check`, `verification`, `check`, `wait`) are not considered when determining mutation success.
19. **Observed generation**: Conditions SHOULD carry `observedGeneration` aligned with `metadata.generation` when the controller updates them for the current spec generation, except revision completion MAY pin the analyzed condition to the generation that triggered the revision, per existing reconciliation behavior.
20. **Immutable spec (excluding revision)**: Once set, `spec.request`, `spec.targetNamespaces`, `spec.analysisOutput`, `spec.tools`, `spec.analysis`, `spec.execution`, and `spec.verification` MUST NOT change; CEL on the CRD enforces this. Only `spec.revisionFeedback` is mutable for iterative feedback.
21. **Option trim after analysis**: When multiple remediation options exist, execution MUST use the option selected through the approval resource; non-selected options MAY be removed from the stored analysis result before execution (see `approval.md`).
22. **Selected option for verification**: Verification MUST use the same selected remediation option as execution (latest trimmed analysis result).

## Configuration Surface

- `spec.request`
- `spec.revisionFeedback`
- `spec.targetNamespaces`
- `spec.analysisOutput` / `spec.analysisOutput.mode` / `spec.analysisOutput.schema`
- `spec.tools` and per-step `spec.analysis.tools`, `spec.execution.tools`, `spec.verification.tools`
- `spec.analysis`, `spec.execution`, `spec.verification`
- `metadata.generation` (revision detection vs `status.conditions`)
- `status.conditions[*].type`, `status.conditions[*].status`, `status.conditions[*].reason`, `status.conditions[*].observedGeneration`
- `status.steps.*.results`, `status.steps.*.sandbox`

## Constraints

- Derivation MUST be a pure function of `status.conditions` for phase display (same conditions → same phase).
- Downstream steps MUST NOT run before approval and precondition rules in `approval.md` are satisfied.
- Execution runs exactly once per analysis iteration. Verification failure escalates, never re-executes.

## Planned Changes

- ~~[PLANNED: OLS-2913]~~ [PLANNED: OLS-3066] Populate `status.steps.<step>.conditions` for observability and async re-entry. See rule 14a. Subsumes the original OLS-2913 step-conditions intent.
- [PLANNED: OLS-2894] **Per-run approval overrides** (e.g. annotations) and **namespace-scoped approval policy** if product requires policy resolution beyond cluster singleton `ApprovalPolicy` named `cluster` (current code: cluster singleton only; see `approval.md`).
- [DONE: OLS-3018] `EmergencyStopped` phase and condition type added to run lifecycle. See `system-config.md` for full kill switch specification.
- [DONE: OLS-3268] `NoActionRequired` terminal phase: when analysis returns `actionRequired=false`, the operator sets `Analyzed=True` with reason `NoActionRequired` and the run auto-completes, bypassing approval/execution/verification.
- [DONE: OLS-3295] Renamed `Proposal` CRD kind to `AgenticRun`, `ProposalApproval` to `AgenticRunApproval`, and updated all associated API surface (labels, RBAC resources, CLI commands, audit events, OTEL spans).
- [DONE: OLS-3558] Execution outcome override — controller no longer hard-fails when `success=false` but all mutating actions succeeded; defers outcome to the verification step. See `sandbox-execution.md` rule 21b.
