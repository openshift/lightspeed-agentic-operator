# Human approval and policy

Behavioral specification for gating asynchronous workflow steps. **Phase derivation** is in `proposal-lifecycle.md`. **CRD field shapes** are in `crd-api.md`.

## Behavioral Rules

1. **Policy resource**: Default cluster behavior is read from a cluster-scoped `ApprovalPolicy` named `cluster`. If the object is absent, the controller MUST behave as if all stages require manual human approval entries (no automatic stages).
2. **Per-proposal resource**: Each `Proposal` MUST have a paired `ProposalApproval` with identical `metadata.name` and `metadata.namespace`. The controller MUST create it on first reconcile if missing, with an initial `spec.stages` containing only stages marked `Automatic` in the policy (each auto stage is inserted with empty parameters and without an explicit `decision` — see rule 6).
3. **Owner reference**: The created `ProposalApproval` MUST be owned by its `Proposal` (controller owner ref, block owner deletion) so it is garbage-collected with the proposal.
4. **Stage coverage**: Policy lists optional `spec.stages[]` entries named `Analysis`, `Execution`, `Verification`, or `Escalation`. Any workflow step **not** listed in the policy defaults to **Manual** gate behavior.
5. **API vocabulary — `ApprovalMode`**: `Automatic` means the step is treated as approved without requiring a user-appended `ProposalApproval` stage entry (policy alone suffices). `Manual` means the step MUST have a matching entry in `ProposalApproval.spec.stages` unless no longer needed because the workflow ended earlier.
6. **Auto seed shapes**: On creation only, automatic stages MUST be represented as `ApprovalStage` entries with the correct `type`, empty nested `analysis|execution|verification|escalation` struct, and **no** explicit `decision` field (implicitly not denied).
7. **Approved vs denied — `ProposalApproval` entry**: A stage is **approved** for gating if a `ProposalApproval.spec.stages` element exists with matching `type` and `decision` is **not** `Denied`. Omitted `decision` MUST be treated as approved (not denied).
8. **Denied terminal**: If any stage has `decision: Denied`, the controller MUST transition the proposal to a denied terminal condition (`Denied=True` at proposal level). Denial MUST be honored at the gate for the matching step when evaluated (analysis, execution, verification, or escalation).
9. **Combined gate function**: For a sandbox step `S`, the effective **approve** signal is true if either (a) `ProposalApproval` contains an approved (non-denied) stage entry for `S`, OR (b) `ApprovalPolicy.spec.stages` contains `name=S` with `approval: Automatic`. This allows policy changes after creation to unblock proposals that still have empty `ProposalApproval` stages (fallback path).
10. **Order of operations**: Analysis MUST pass its gate before the controller runs analysis. Execution MUST pass execution gate after analysis succeeds AND before RBAC + execution. Verification MUST pass verification gate before verification runs. Escalation MUST pass escalation gate before escalation runs.
11. **Proposed idle**: When analysis is complete and execution is configured, the proposal remains in the **Proposed** derived phase until the execution gate opens (manual approval or automatic policy) — reconciliation MUST be idempotent while waiting.
12. **Verifying idle**: After execution succeeds and verification is configured, the proposal remains **Verifying** while waiting for verification approval when manual.
13. **Escalation idle**: When escalation is required, the proposal remains **Escalating** until escalation is approved (or auto policy) and the escalation agent completes or fails.
14. **Execution option selection**: When multiple `RemediationOption` entries exist on the latest `AnalysisResult`, the user MUST select one via `ProposalApproval` execution stage `option`. If omitted, the effective index MUST default to option `0`.
15. **Trim-on-execute**: Before executing, when more than one option remains stored, the controller SHOULD persist a trimmed `AnalysisResult.status.options` containing only the selected option; when only one option exists, trimming is a no-op beyond selection.
16. **Execution agent override**: `ProposalApproval` execution stage `agent`, when set, MUST override `spec.execution.agent` / default agent naming for resolving the `Agent` CR for that step.
17. **Analysis/verification/escalation agent overrides**: Each stage’s nested object MAY carry `agent` which MUST override the proposal’s step agent for that stage when non-empty (escalation default remains analysis agent if no override — per controller resolution rules).
18. **Max attempts — ceiling**: Let `ceiling` be `ApprovalPolicy.spec.maxAttempts` when that value is positive; otherwise `ceiling` MUST be treated as `1` for attempt budgeting.
19. **Max attempts — execution cap**: When `ProposalApproval` execution stage sets `maxAttempts` > 0, the effective maximum attempts MUST be the lesser of that value and `ceiling`. When execution stage omits `maxAttempts` or sets zero, the effective maximum MUST be `ceiling`.
20. **Max attempts — policy absent**: With no `ApprovalPolicy` object, `ceiling` MUST be `1`; execution stage `maxAttempts` MUST still be capped by that effective ceiling (cannot raise attempts without a policy defining a higher ceiling).
21. **Attempts semantics**: “Max attempts” counts total execution runs in the current analysis context (initial run plus post-verification retries) until success or escalation per `proposal-lifecycle.md`.
22. **Verification approval persistence**: User approval for verification MUST apply across verification retries within the same analysis iteration (tests verify a single verification approval covers multiple verify invocations after execution retries).
23. **CEL invariants on `ProposalApproval`**: Users MUST NOT remove prior stages; MUST NOT flip decisions; MUST NOT lower execution `maxAttempts` once set — CRD validation enforces these.
24. **Append-only human workflow**: Operators SHOULD instruct users to add new stages by patching `spec.stages` append-only rather than replacing the whole list, to respect CEL.
25. **Escalation stage existence**: Escalation is not in `Proposal.spec`; it appears only when retry/verification logic escalates. Approval for escalation follows the same Automatic/Manual rules as other stages.

## Configuration Surface

- `ApprovalPolicy.metadata.name` (must equal `cluster`)
- `ApprovalPolicy.spec.stages[].name`, `ApprovalPolicy.spec.stages[].approval`
- `ApprovalPolicy.spec.maxAttempts`, `ApprovalPolicy.spec.maxConcurrentProposals`
- `ProposalApproval.metadata.name`, `ProposalApproval.metadata.namespace`
- `ProposalApproval.spec.stages[].type`, `ProposalApproval.spec.stages[].decision`
- `ProposalApproval.spec.stages[].analysis.agent`
- `ProposalApproval.spec.stages[].execution.agent`, `.option`, `.maxAttempts`
- `ProposalApproval.spec.stages[].verification.agent`
- `ProposalApproval.spec.stages[].escalation.agent`

## Constraints

- Product documentation that speaks in terms such as “always approve”, “always require approval”, or “require approval only for execution” MUST be translated into explicit `Automatic`/`Manual` combinations on `ApprovalPolicy.spec.stages`; the CRD does **not** encode those phrases as enumerated `ApprovalMode` values.
- Policy MUST NOT be namespace-scoped in the current API — only the cluster singleton is read by name `cluster`.

## Planned Changes

- [PLANNED: OLS-2894] **Proposal-scoped policy hints** (e.g. annotations) evaluated **before** cluster `ApprovalPolicy` for enterprise overrides.
- [PLANNED: OLS-2894] **Namespace-scoped `ApprovalPolicy`** or additional policy CRDs if tenancy demands policies per namespace instead of a single cluster singleton.
- [PLANNED: OLS-3019] Re-authorization or **re-approval** for long-running or deviating executions beyond append-only stage semantics.
