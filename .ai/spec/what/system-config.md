# System configuration and kill switch (`AgenticOLSConfig`)

Behavioral specification for the cluster-wide agentic system configuration CR and its **emergency suspension** (kill switch) capability. **Run lifecycle phases** are in `run-lifecycle.md`. **CRD field semantics** for other kinds are in `crd-api.md`.

Jira tracking: OLS-3018 (base kill switch), OLS-3267 (hardening).

## Behavioral Rules

### AgenticOLSConfig CRD

1. **Kind and scope**: `AgenticOLSConfig` MUST be cluster-scoped in API group `agentic.openshift.io`, version `v1alpha1`.
2. **Singleton**: CRD validation MUST enforce `metadata.name == "cluster"` via CEL (same pattern as `ApprovalPolicy`).
3. **Absence semantics**: When no `AgenticOLSConfig` CR exists, the system MUST behave as if `spec.suspended` is `false` — the CR is not required for normal operation.
4. **Spec structure**: The spec MUST include:
   - `suspended` (bool, optional, default `false`): When `true`, halts all agentic operations cluster-wide.

### Emergency Suspension (`spec.suspended`)

5. **Activation**: Setting `spec.suspended` to `true` MUST immediately prevent the run reconciler from starting any new workflow steps (analysis, execution, verification, escalation) for any run cluster-wide.
6. **In-flight termination**: When `spec.suspended` becomes `true`, workflow progression MUST stop immediately for all non-terminal runs, and each such run MUST receive the run-level `Stopping` condition before teardown (see `run-lifecycle.md`). The operator MUST invoke the shared hard-stop contract: revoke sandbox ServiceAccount access and execution RBAC, delete bare Pods with zero grace, delete SandboxClaims, and terminate active backing Pods. The `EmergencyStopped` outcome is recorded per run only after that run's teardown is confirmed (rule 14). Cleanup MUST use both run status and managed-resource discovery; cleanup errors keep the run visibly `Stopping` and remain retryable. See `agentic-run-termination.md`. [PLANNED: OLS-4131]
7. **EmergencyStopped condition**: The operator MUST set condition type `EmergencyStopped` with status `True`, reason `SystemSuspended`, and message `"Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)"` in the same status update that completes the run's `Stopping` condition — only after the run's sandbox workloads and access are confirmed removed. [PLANNED: OLS-4131]
8. **EmergencyStopped is terminal — no automatic restart**: `EmergencyStopped` is a terminal phase. Runs in this state MUST NOT resume when `spec.suspended` is set back to `false`. To retry work, the admin creates new runs. This is a safety invariant: the kill switch exists for emergencies where agent behavior is harmful, so automatically restarting the same runs that caused the emergency would re-introduce the exact problem the admin stopped. Resumption MUST always require explicit human action (creating new runs).
9. **DerivePhase precedence**: `EmergencyStopped=True` MUST be checked **before** all other conditions in `DerivePhase()`. It takes precedence over `Escalated`, `Denied`, and all progress conditions.
10. **Resumption**: Once the `Suspended` condition reaches `AdminActivated` or `AdminActivatedWithErrors`, setting `spec.suspended` back to `false` re-enables the system for **new** runs only. Existing `EmergencyStopped` runs remain terminal. Deactivation is accepted only once one of those terminal reasons is observed; any other condition state is rejected (rule 11j).
11. **New run blocking**: While `suspended=true`, runs that are already in `Pending` phase (no conditions set yet) MUST also be terminated with `EmergencyStopped` — suspension applies to all non-terminal runs, not just those with active sandboxes.

### Admission-Time Blocking (OLS-3267)

Primary enforcement so new runs never persist while the system is suspended. Spike design: OLS-3166 / closed PR #112 (updated for `AgenticRun` rename).

11a. **AgenticRun CREATE rejection**: While `AgenticOLSConfig.spec.suspended` is `true`, the API server MUST reject `AgenticRun` CREATE requests at admission time with a clear error message stating that the agentic system is suspended. Enforcement MUST use a `ValidatingAdmissionPolicy` whose `spec.paramKind` is explicitly `apiVersion: agentic.openshift.io/v1alpha1`, `kind: AgenticOLSConfig`, with a binding `paramRef` to that kind.
11b. **CEL source of truth**: The admission policy CEL expression MUST evaluate `params.spec.suspended` on the referenced `AgenticOLSConfig`. It MUST NOT key off `status.conditions` (including the `Suspended` condition) — admission follows the admin-set kill switch immediately. Without an explicit `paramKind`, `params` is unavailable and this CEL cannot evaluate. The expression MUST use presence-safe navigation (e.g. `!has(params.spec.suspended) || !params.spec.suspended`) so an absent optional field evaluates as not suspended. The CRD MUST default `spec.suspended` to `false` for stored objects; that defaulting is the primary guarantee that `has(params.spec.suspended)` is true after normal create/apply.
11c. **Absence allows creation**: If no `AgenticOLSConfig` CR exists, admission MUST allow `AgenticRun` creation. Consistent with rule 3 (absence = `suspended=false`). Enforced via `parameterNotFoundAction: Allow` on the policy binding, with `paramRef.name` equal to `cluster`.
11d. **CREATE-only on `agenticruns`**: The admission policy's AgenticRun resource rule MUST only intercept `CREATE` operations on `agenticruns` (`apiGroup: agentic.openshift.io`, `apiVersion: v1alpha1`). Updates to existing `AgenticRun` objects MUST NOT be blocked. Other kinds in the API group (`AgenticRunApproval`, `Agent`, `LLMProvider`, `ApprovalPolicy`, result CRs) MUST NOT be matched. `AgenticOLSConfig` UPDATE and DELETE are the sole exceptions, governed by rule 11j. `matchPolicy` MAY remain the API default (`Equivalent`) so future equivalent served API versions of `agenticruns` remain covered; do not set `Exact` unless deliberately pinning to a single version.
11e. **Policy evaluation failure**: The `ValidatingAdmissionPolicy` MUST set `failurePolicy: Fail` so that if an installed and active policy cannot be evaluated, `AgenticRun` CREATE is denied. `failurePolicy: Fail` does not deny CREATE when the policy or binding is absent, ignored, or never installed — rule 11h (reconciler) covers that case.
11f. **Deny action**: The `ValidatingAdmissionPolicyBinding` MUST set `validationActions: [Deny]`.
11g. **Static install with operator**: The `ValidatingAdmissionPolicy` and `ValidatingAdmissionPolicyBinding` MUST be static manifests under `config/admission/`, wired into `config/default` (so `make deploy` / `make undeploy` install and remove them) and applied by the quickstart `hack/quickstart/deploy-operator.sh` (removed by `undeploy-operator.sh`). They require no runtime create/update/delete management by an operator reconciler. There is no OLM bundle for this operator today; do not assume OLM applies these kinds.

11h. **Defense-in-depth**: The reconciler guard (rules 12–15) MUST remain as fallback enforcement. The admission policy is primary for new CREATE; the reconciler catches race conditions during suspension toggle, pre-existing non-terminal runs, and VAP removal or misinstallation.
11i. **Approvals while suspended**: `AgenticRunApproval` CREATE/UPDATE (including human approve/deny PATCH) MUST remain allowed while suspended. The run reconciler MUST NOT start new workflow steps while suspended (rules 5, 13), so an approval written during suspension has no execution effect on a run that is or will become `EmergencyStopped`. Extending admission to block approvals or other kinds is deferred ([PLANNED: future]) if product needs require it later.
11j. [DONE: OLS-4133] **Safe resumption**: The admission policy MUST reject an `AgenticOLSConfig` UPDATE that changes `spec.suspended` from `true` to `false`, and an `AgenticOLSConfig` DELETE while `spec.suspended` is `true`, unless the existing `Suspended` condition is `True` with reason `AdminActivated` or `AdminActivatedWithErrors`. Because the status controller reconciles separately and the condition write can lag the spec change, an absent, `False`, or `Stopping` condition MUST all be rejected — rejection MUST NOT depend on the condition already reading `Stopping`. A DELETE is allowed when the existing spec is not suspended. Unrelated `AgenticOLSConfig` updates (including edits that leave `spec.suspended=true`) MUST remain allowed. The policy and binding are static manifests under `config/admission/`, wired into `config/default` and the quickstart apply/remove scripts; no finalizer is added. The operator status RBAC remains operator-only because the policy trusts `oldObject.status.conditions`; break-glass is removing the binding.

### Suspension Status and Observability

5a. **Status subresource**: `AgenticOLSConfig` MUST have a `/status` subresource. The status MUST include a `conditions` array following the standard `metav1.Condition` shape.
5b. **Suspended condition**: When `spec.suspended` is set to `true`, the operator MUST set condition type `Suspended` with status `True`. The condition transitions through three reasons:
   - `Stopping` [PLANNED: OLS-4131 renames this reason from `Draining`; the condition type is unchanged]: Set immediately while any active managed sandbox workload or associated sandbox access remains, or while any AgenticRun remains non-terminal (its stopped outcome not yet recorded), except for a run whose only remaining blocker is an unverified spoke under `AdminActivatedWithErrors`. A run with no sandbox workload or access — for example `Pending` — is trivially torn down, so this run-outcome check is what keeps the condition `Stopping` until the run reconciler writes `EmergencyStopped`. Message SHOULD include the active sandbox count and the count of runs still stopping, and MUST surface list/deletion/verification errors.
   - `AdminActivated`: Set only after the operator confirms that no active managed sandbox workload or associated access remains and no AgenticRun remains non-terminal — every run that was non-terminal when suspension was accepted has recorded its outcome. Message SHOULD include the count of runs emergency-stopped (e.g., `"System suspended; 12 runs emergency-stopped"`).
   - `AdminActivatedWithErrors`: [PLANNED: OLS-4133] Terminal degraded completion, set when the only remaining unverified work is access on a gone or unreachable spoke. Every non-terminal AgenticRun MUST either be absent or have `Stopping=True` with that spoke as its `spec.targetCluster`; a run targeting a healthy spoke keeps the condition `Stopping`. A missing or invalid spoke kubeconfig MAY degrade immediately; network failures MUST first retry for a bounded window (60s operator constant). The message MUST name the unverified spokes and the runs still stopping, and include the compact error. The controller MUST emit a Warning `SuspensionCompletedWithErrors` event. Cleanup keeps retrying and MAY return the condition to clean `AdminActivated` if the spoke recovers before resume.
5c. **Suspended condition on deactivation**: A request to set `spec.suspended=false` MUST be rejected unless `Suspended=True` has reason `AdminActivated` or `AdminActivatedWithErrors` (rule 11j). Once the condition has reached either terminal reason, the accepted deactivation MUST update the `Suspended` condition to status `False`, reason `AdminDeactivated`, with a new `lastTransitionTime`.
5d. **Suspension Events**: The operator MUST emit a Kubernetes Event on the `AgenticOLSConfig` object when suspension is activated and when suspension is deactivated. The activation Event is emitted once when `spec.suspended` transitions to `true`, independent of whether teardown is still stopping. Event format:
   - Activation: `type: Warning`, reason `SuspensionActivated`, message `"System suspension activated; stopping {N} active sandbox workloads"`.
   - Deactivation: `type: Normal`, reason `SuspensionDeactivated`, message `"System resumed; agentic operations re-enabled"`.
5e. **Status update timing**: The `Suspended` condition MUST be set immediately when `spec.suspended` becomes `true` — with reason `Stopping` if any active managed sandbox workload or associated access remains or any AgenticRun is still non-terminal, or reason `AdminActivated` if neither remains. As teardown and run outcomes complete, the controller re-reconciles (it watches AgenticRuns and managed sandbox resources as applicable) and updates the condition from `Stopping` to `AdminActivated`. If a non-terminal AgenticRun appears while suspended (for example an admission race), the controller MUST set the condition back to reason `Stopping` until that run's outcome is recorded, unless the degraded exception in rule 5b applies. A dedicated `AgenticOLSConfig` controller handles this status lifecycle.

### Reconciler Integration

12. **Watch and re-queue**: The run reconciler MUST watch `AgenticOLSConfig` and re-queue all non-terminal runs when the CR changes (same pattern as the existing `ApprovalPolicy` watch).
13. **Reconcile guard**: The suspension check MUST execute after the deletion handler and before finalizer addition, terminal phase routing, approval resolution, revision handling, and phase dispatch. Per-run cancellation [PLANNED: OLS-4132] would be evaluated immediately after the suspension check and before those same operations.
14. **Order of operations on termination**: For each non-terminal run when suspended: (a) set the run's `Stopping` condition and invoke hard-stop cleanup, including direct discovery of active managed workloads; (b) once cleanup is confirmed complete, set the `EmergencyStopped` condition and complete `Stopping` in a single status patch. A cleanup error MUST keep the run `Stopping` and trigger retry; the outcome condition is not written while an active workload or sandbox access remains. A degradeable spoke failure may instead complete the config with `AdminActivatedWithErrors` once the bounded retry window has elapsed and every remaining non-terminal run is `Stopping` on that spoke. [PLANNED: OLS-4131]
15. **Config fetch failure**: If the `AgenticOLSConfig` CR cannot be fetched and the error is not `NotFound`, the reconciler MUST return the error for retry. `NotFound` MUST be treated as `suspended=false`.
15a. [PLANNED: OLS-4133] **Deletion with unverified spoke access**: When an `AgenticRun` targeting a gone or unreachable spoke is deleted, the deletion handler MUST retry sandbox and RBAC cleanup for the same bounded 60s window. If the only remaining failure is a degradeable spoke error after that window, the operator MAY remove the `agentic.openshift.io/execution-rbac-cleanup` finalizer and emit a Warning Event so Kubernetes can delete the run. A missing spoke kubeconfig is hard gone and MAY release immediately; an invalid kubeconfig or network failure retries for the window. A hub permission, API, or non-degradeable cleanup error MUST keep the finalizer. The cluster suspension sweep remains the retry owner for labeled spoke access while suspension is active. The run-level `Stopping` condition is not rewritten to a terminal outcome by this release.

### Console Visibility

16. **Suspension banner**: The console plugin MUST display a cluster-wide danger alert banner when `AgenticOLSConfig.spec.suspended == true`. The banner MUST be visible on all agentic views without requiring page reload when the state changes.
17. **EmergencyStopped phase display**: The console MUST render `EmergencyStopped` runs with a distinct visual treatment (status badge, color) that is clearly different from `Failed`.
18. **DerivePhase sync**: The console's `derivePhaseFromConditions` function in `src/models/proposal.ts` MUST be updated to handle the `EmergencyStopped` condition with the same precedence as the Go implementation (per the existing `// SYNC:` contract).

### CLI Visibility

19. **Status command**: `oc agentic status` (or equivalent top-level command) MUST report the system suspension state: `"Agentic System: SUSPENDED"` when suspended, `"Agentic System: Active"` when not. When `status.conditions` includes `Suspended=True`:
   - Reason `Stopping` [PLANNED: OLS-4131]: output SHOULD show `"SUSPENDED (stopping, {message})"`.
   - Reason `AdminActivated`: output SHOULD include relative and absolute `lastTransitionTime` and the condition message (e.g. run emergency-stop count).
   - When `spec.suspended` is false and the latest `Suspended` condition has reason `AdminDeactivated`, the output SHOULD include when the system was resumed.
20. **Suspend/resume commands**: The CLI MUST provide `oc agentic suspend` and `oc agentic resume` commands that patch `AgenticOLSConfig.spec.suspended` to `true` and `false` respectively.
21. **Suspend confirmation**: `oc agentic suspend` MUST prompt for confirmation before proceeding: `"All agentic operations will be halted and in-flight runs will be terminated. Continue? [y/N]"`.
22. **Run list**: `oc agentic runs` (or equivalent list command) MUST display `EmergencyStopped` as a distinct phase value in the phase/status column.

## Configuration Surface

### AgenticOLSConfig
- `metadata.name` (must be `cluster`)
- `spec.suspended` (bool, default `false`)
- `status.conditions` — condition types: `Suspended`

### Affected AgenticRun fields
- `status.conditions` — condition types `EmergencyStopped` and the termination-progress `Stopping` [PLANNED: OLS-4131]
- Derived phase `EmergencyStopped` added to `AgenticRunPhase` enum

### Affected repositories
- `lightspeed-agentic-operator` — CRD types, run reconciler, `AgenticOLSConfig` status controller, cluster sandbox sweep, CLI commands, admission manifests (`ValidatingAdmissionPolicy` / binding for AgenticRun CREATE and AgenticOLSConfig UPDATE/DELETE); E2E: `test/e2e/suspension_test.go` (including admission rejection while suspended and resume/delete gating)
- `lightspeed-agentic-console` — `derivePhaseFromConditions` sync, suspension banner, phase display

## Constraints

- `EmergencyStopped` MUST be added to `isTerminal()` in the reconciler and any console/CLI equivalents.
- The `AgenticOLSConfig` controller RBAC MUST include `get`, `list`, `watch` on `agenticolsconfigs` for the run reconciler's service account.
- The `oc agentic suspend` / `resume` commands require the user to have `patch` permissions on `AgenticOLSConfig`.
- Termination of in-flight runs via reconciler re-queue is bounded by `maxConcurrentReconciles`; global completion additionally waits for direct managed-sandbox discovery and teardown. If real-world scale requires faster termination, a batch-sweep optimization can be added without changing the termination outcomes.
- Admission-time blocking (rules 11a–11j) requires OpenShift 4.17+ (Kubernetes 1.30+, where `ValidatingAdmissionPolicy` is GA). Quickstart documents OpenShift 4.22+.
- No additional operator ServiceAccount RBAC is required for the VAP/binding: they are cluster-scoped install-time resources applied by the install path (kustomize / quickstart), not created by the operator at runtime.


## Planned Changes

- [PLANNED: future] Batch-sweep optimization: if reconciler-based termination proves too slow at scale, add direct fan-out without changing the hard-stop or status semantics.
- [PLANNED: future] Additional config fields (e.g., system-wide defaults, feature gates) can be added to the `AgenticOLSConfig` spec as needed.
- [DONE: OLS-3267] Implement admission-time blocking per rules 11a–11i (VAP + binding manifests, kustomize + quickstart wiring, E2E for CREATE rejection while suspended).
- [DONE: OLS-4133] Global suspension sweep and safe resume — extend the `AgenticOLSConfig` reconciler with a cluster sweep of hub-local workloads and hub/spoke RBAC, a direct orphan sweep, and `AdminActivated` / `AdminActivatedWithErrors` terminal reasons. The static resume policy rejects `AgenticOLSConfig` UPDATE true→false and DELETE until the `Suspended` condition is terminal. Local tests cover bare-pod/list-based cleanup and the condition lifecycle; live cluster e2e covers the VAP.
- [PLANNED: OLS-4018, OLS-4131] Sandbox hard-stop on suspension — delete active managed Pods/SandboxClaims with zero grace, revoke associated access, keep the `Suspended` condition reason `Stopping` until teardown is confirmed and every stopped run has recorded its outcome, and reject resumption until reason `AdminActivated` or `AdminActivatedWithErrors` is observed (including before the condition is first recorded). Durable audit storage, Result CRs already published, and Kubernetes audit are the forensic sources.
- [PLANNED: OLS-4131] Confirmed-teardown stopping contract: run-level `Stopping` condition shared by cancellation and suspension; stopped outcomes recorded per run only after its teardown is confirmed, then final. See `agentic-run-termination.md` in the parent repo.
- [PLANNED: future] Extend admission-time blocking beyond `AgenticRun` CREATE (e.g. reject `AgenticRunApproval` updates while suspended) only if product needs require it.
- [DONE: OLS-3295] Renamed `Proposal` references to `AgenticRun` in kill switch logic, CLI commands, and console display.
