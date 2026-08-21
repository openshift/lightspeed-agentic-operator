# Human approval and policy

Behavioral specification for gating asynchronous workflow steps. **Phase derivation** is in `run-lifecycle.md`. **CRD field shapes** are in `crd-api.md`.

## Behavioral Rules

1. **Policy resource**: Default cluster behavior is read from a cluster-scoped `ApprovalPolicy` named `cluster`. If the object is absent, the controller MUST behave as if `Analysis`, `Execution`, and `Verification` require manual human approval entries (no automatic stages); the read-only `Escalation` step MUST default to automatic (see rule 4).
2. **Per-run resource**: Each `AgenticRun` MUST have a paired `AgenticRunApproval` with identical `metadata.name` and `metadata.namespace`. The controller MUST create it on first reconcile if missing, with an initial `spec.stages` containing only stages marked `Automatic` in the policy **that the run actually defines** (each auto stage is inserted with empty parameters and without an explicit `decision` — see rule 6). Stages for steps the run omits (e.g. `Verification` on an analysis-only run) MUST NOT be seeded regardless of the policy.
3. **Owner reference**: The created `AgenticRunApproval` MUST be owned by its `AgenticRun` (controller owner ref, block owner deletion) so it is garbage-collected with the run.
4. **Stage coverage**: Policy lists optional `spec.stages[]` entries named `Analysis`, `Execution`, `Verification`, or `Escalation`. Any of `Analysis`, `Execution`, or `Verification` **not** listed in the policy defaults to **Manual** gate behavior. `Escalation` is read-only (it produces a report, mutates nothing) and defaults to **Automatic**; it gates only when the policy lists it explicitly as `Escalation` with `approval: Manual`.
5. **API vocabulary — `ApprovalMode`**: `Automatic` means the step is treated as approved without requiring a user-appended `AgenticRunApproval` stage entry (policy alone suffices). `Manual` means the step MUST have a matching entry in `AgenticRunApproval.spec.stages` unless no longer needed because the workflow ended earlier.
6. **Auto seed shapes**: On creation only, automatic stages MUST be represented as `ApprovalStage` entries with the correct `type`, empty nested `analysis|execution|verification|escalation` object (`{}`, no `agent` field), and **no** explicit `decision` field (implicitly not denied).
7. **Approved vs denied — `AgenticRunApproval` entry**: A stage is **approved** for gating if a `AgenticRunApproval.spec.stages` element exists with matching `type` and `decision` is **not** `Denied`. Omitted `decision` MUST be treated as approved (not denied).
8. **Denied terminal**: If any stage has `decision: Denied`, the controller MUST transition the run to a denied terminal condition (`Denied=True` at run level). Denial MUST be honored at the gate for the matching step when evaluated (analysis, execution, verification, or escalation).
9. **Combined gate function**: For a sandbox step `S`, the effective **approve** signal is true if either (a) `AgenticRunApproval` contains an approved (non-denied) stage entry for `S`, OR (b) `ApprovalPolicy.spec.stages` contains `name=S` with `approval: Automatic`, OR (c) `S` is `Escalation` and the policy does **not** list it with `approval: Manual`. Clauses (a) and (b) allow policy changes after creation to unblock runs that still have empty `AgenticRunApproval` stages (fallback path); clause (c) makes the read-only escalation step auto-approve unless explicitly gated.
10. **Order of operations**: Analysis MUST pass its gate before the controller runs analysis. Execution MUST pass execution gate after analysis succeeds AND before RBAC + execution. Verification MUST pass verification gate before verification runs. Escalation MUST pass escalation gate before escalation runs.
11. **Proposed idle**: When analysis is complete and execution is configured, the run remains in the **Proposed** derived phase until the execution gate opens (manual approval or automatic policy) — reconciliation MUST be idempotent while waiting.
12. **Verifying idle**: After execution succeeds and verification is configured, the run remains **Verifying** while waiting for verification approval when manual.
13. **Escalation idle**: When escalation is required, the run remains **Escalating** until escalation is approved (or auto policy) and the escalation agent completes or fails.
14. **Execution option selection**: When multiple `RemediationOption` entries exist on the latest `AnalysisResult`, the user MUST select one via `AgenticRunApproval` execution stage `option`. If omitted, the effective index MUST default to option `0`.
15. **Trim-on-execute**: Before executing, when more than one option remains stored, the controller SHOULD persist a trimmed `AnalysisResult.status.options` containing only the selected option; when only one option exists, trimming is a no-op beyond selection.
16. **Execution agent override**: `AgenticRunApproval` execution stage `agent`, when set (non-empty), MUST override `spec.execution.agent` / default agent naming for resolving the `Agent` CR for that step. When omitted, the run's execution step agent is used (no override).
17. **Analysis/verification/escalation agent overrides**: Each stage's nested object MAY carry `agent`. When non-empty it MUST override the run's step agent for that stage; when omitted there is no override (escalation without override uses the analysis agent — per controller resolution rules). Nested approval objects MUST NOT default `agent` to `"default"` in the CRD schema.
18. **Single execution attempt**: Execution runs exactly once per analysis iteration. On verification failure, the operator escalates directly — there are no execution retries. The `maxAttempts` field has been removed from `ApprovalPolicy` and `AgenticRunApproval`.
19. **CEL invariants on `AgenticRunApproval`**: Users MUST NOT remove prior stages; MUST NOT flip decisions — CRD validation enforces these.
20. **Append-only human workflow**: Operators SHOULD instruct users to add new stages by patching `spec.stages` append-only rather than replacing the whole list, to respect CEL.
21. **Escalation stage existence**: Escalation is not in `AgenticRun.spec`; it appears when verification fails. Approval for escalation follows the same Automatic/Manual rules as other stages.

## Configuration Surface

- `ApprovalPolicy.metadata.name` (must equal `cluster`)
- `ApprovalPolicy.spec.stages[].name`, `ApprovalPolicy.spec.stages[].approval`
- `ApprovalPolicy.spec.maxConcurrentRuns`
- `AgenticRunApproval.metadata.name`, `AgenticRunApproval.metadata.namespace`
- `AgenticRunApproval.spec.stages[].type`, `AgenticRunApproval.spec.stages[].decision`
- `AgenticRunApproval.spec.stages[].analysis.agent`
- `AgenticRunApproval.spec.stages[].execution.agent`, `.option`
- `AgenticRunApproval.spec.stages[].verification.agent`
- `AgenticRunApproval.spec.stages[].escalation.agent`

### Approval Authorization

22. **Cluster-admin gate.** Only users in the `system:cluster-admins` group MAY approve run execution via `patch` on `agenticrunapprovals`. This is enforced by Kubernetes RBAC.
23. **Dedicated approver ClusterRole.** The operator ships a ClusterRole `agentic-run-approver` granting `get`, `list`, `watch`, and `patch` on `agenticrunapprovals`, plus `get`, `list`, `watch` on `agenticruns` (so approvers can see what they're approving). A ClusterRoleBinding `agentic-run-approver-binding` binds this role to the `system:cluster-admins` group. No other operator-shipped binding grants `patch agenticrunapprovals` to human actors. The operator's own `agentic-operator-manager-role` retains `patch` since it seeds AgenticRunApproval CRs programmatically (bound to the `controller-manager` ServiceAccount, not a human).
24. **Implementation.** Manifests live in `config/rbac/run_approver_role.yaml` and `config/rbac/run_approver_binding.yaml`, included via `config/rbac/kustomization.yaml`. Applied automatically on `make deploy`.

## Constraints

- Product documentation that speaks in terms such as "always approve", "always require approval", or "require approval only for execution" MUST be translated into explicit `Automatic`/`Manual` combinations on `ApprovalPolicy.spec.stages`; the CRD does **not** encode those phrases as enumerated `ApprovalMode` values.
- Policy MUST NOT be namespace-scoped in the current API — only the cluster singleton is read by name `cluster`.
- The cluster-admin approval gate is binary. Namespace-scoped approval delegation is out of scope for the current release (see the workspace-level agentic security spec at `ols/.ai/spec/what/agentic-security.md` Planned Changes).

## Planned Changes

- [PLANNED: OLS-2894] **Run-scoped policy hints** (e.g. annotations) evaluated **before** cluster `ApprovalPolicy` for enterprise overrides.
- [PLANNED: OLS-2894] **Namespace-scoped `ApprovalPolicy`** or additional policy CRDs if tenancy demands policies per namespace instead of a single cluster singleton.
- [PLANNED: OLS-3019] Re-authorization or **re-approval** for long-running or deviating executions beyond append-only stage semantics.
- [DONE: OLS-3295] Renamed `ProposalApproval` CRD to `AgenticRunApproval`, `proposals` RBAC resource to `agenticruns`, approver ClusterRole to `agentic-run-approver`.
