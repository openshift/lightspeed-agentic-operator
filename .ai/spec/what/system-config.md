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
6. **In-flight termination**: When `spec.suspended` becomes `true`, all non-terminal runs MUST be terminated with `EmergencyStopped`. The operator MUST invoke the shared hard-stop contract: revoke sandbox ServiceAccount access and execution RBAC, delete bare Pods with zero grace, delete SandboxClaims, and terminate active backing Pods. Cleanup MUST use both run status and managed-resource discovery; cleanup errors remain retryable after the run condition is written. See `agentic-run-termination.md`.
7. **EmergencyStopped condition**: The operator MUST set condition type `EmergencyStopped` with status `True`, reason `SystemSuspended`, and message `"Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)"`.
8. **EmergencyStopped is terminal — no automatic restart**: `EmergencyStopped` is a terminal phase. Runs in this state MUST NOT resume when `spec.suspended` is set back to `false`. To retry work, the admin creates new runs. This is a safety invariant: the kill switch exists for emergencies where agent behavior is harmful, so automatically restarting the same runs that caused the emergency would re-introduce the exact problem the admin stopped. Resumption MUST always require explicit human action (creating new runs).
9. **DerivePhase precedence**: `EmergencyStopped=True` MUST be checked **before** all other conditions in `DerivePhase()`. It takes precedence over `Escalated`, `Denied`, and all progress conditions.
10. **Resumption**: Once the `Suspended` condition reaches `AdminActivated`, setting `spec.suspended` back to `false` re-enables the system for **new** runs only. Existing `EmergencyStopped` runs remain terminal. Deactivation is rejected while teardown is Draining (rule 11j).
11. **New run blocking**: While `suspended=true`, runs that are already in `Pending` phase (no conditions set yet) MUST also be terminated with `EmergencyStopped` — suspension applies to all non-terminal runs, not just those with active sandboxes.

### Admission-Time Blocking (OLS-3267)

Primary enforcement so new runs never persist while the system is suspended. Spike design: OLS-3166 / closed PR #112 (updated for `AgenticRun` rename).

11a. **AgenticRun CREATE rejection**: While `AgenticOLSConfig.spec.suspended` is `true`, the API server MUST reject `AgenticRun` CREATE requests at admission time with a clear error message stating that the agentic system is suspended. Enforcement MUST use a `ValidatingAdmissionPolicy` whose `spec.paramKind` is explicitly `apiVersion: agentic.openshift.io/v1alpha1`, `kind: AgenticOLSConfig`, with a binding `paramRef` to that kind.
11b. **CEL source of truth**: The admission policy CEL expression MUST evaluate `params.spec.suspended` on the referenced `AgenticOLSConfig`. It MUST NOT key off `status.conditions` (including the `Suspended` condition) — admission follows the admin-set kill switch immediately. Without an explicit `paramKind`, `params` is unavailable and this CEL cannot evaluate. The expression MUST use presence-safe navigation (e.g. `!has(params.spec.suspended) || !params.spec.suspended`) so an absent optional field evaluates as not suspended. The CRD MUST default `spec.suspended` to `false` for stored objects; that defaulting is the primary guarantee that `has(params.spec.suspended)` is true after normal create/apply.
11c. **Absence allows creation**: If no `AgenticOLSConfig` CR exists, admission MUST allow `AgenticRun` creation. Consistent with rule 3 (absence = `suspended=false`). Enforced via `parameterNotFoundAction: Allow` on the policy binding, with `paramRef.name` equal to `cluster`.
11d. **CREATE-only on `agenticruns`**: The admission policy's AgenticRun resource rule MUST only intercept `CREATE` operations on `agenticruns` (`apiGroup: agentic.openshift.io`, `apiVersion: v1alpha1`). Updates to existing `AgenticRun` objects MUST NOT be blocked. Other kinds in the API group (`AgenticRunApproval`, `Agent`, `LLMProvider`, `ApprovalPolicy`, result CRs) MUST NOT be matched. `AgenticOLSConfig` UPDATE is the sole exception, governed by rule 11j. `matchPolicy` MAY remain the API default (`Equivalent`) so future equivalent served API versions of `agenticruns` remain covered; do not set `Exact` unless deliberately pinning to a single version.
11e. **Policy evaluation failure**: The `ValidatingAdmissionPolicy` MUST set `failurePolicy: Fail` so that if an installed and active policy cannot be evaluated, `AgenticRun` CREATE is denied. `failurePolicy: Fail` does not deny CREATE when the policy or binding is absent, ignored, or never installed — rule 11h (reconciler) covers that case.
11f. **Deny action**: The `ValidatingAdmissionPolicyBinding` MUST set `validationActions: [Deny]`.
11g. **Static install with operator**: The `ValidatingAdmissionPolicy` and `ValidatingAdmissionPolicyBinding` MUST be static manifests under `config/admission/`, wired into `config/default` (so `make deploy` / `make undeploy` install and remove them) and applied by the quickstart `hack/quickstart/deploy-operator.sh` (removed by `undeploy-operator.sh`). They require no runtime create/update/delete management by an operator reconciler. There is no OLM bundle for this operator today; do not assume OLM applies these kinds.

11h. **Defense-in-depth**: The reconciler guard (rules 12–15) MUST remain as fallback enforcement. The admission policy is primary for new CREATE; the reconciler catches race conditions during suspension toggle, pre-existing non-terminal runs, and VAP removal or misinstallation.
11i. **Approvals while suspended**: `AgenticRunApproval` CREATE/UPDATE (including human approve/deny PATCH) MUST remain allowed while suspended. The run reconciler MUST NOT start new workflow steps while suspended (rules 5, 13), so an approval written during suspension has no execution effect on a run that is or will become `EmergencyStopped`. Extending admission to block approvals or other kinds is deferred ([PLANNED: future]) if product needs require it later.
11j. [PLANNED: OLS-4018] **Safe resumption**: The admission policy MUST reject an `AgenticOLSConfig` UPDATE that changes `spec.suspended` from `true` to `false` while the existing `Suspended` condition is `True` with reason `Draining`. This keeps `spec.suspended=true` — and therefore AgenticRun CREATE admission blocking and reconcile guards — in force until managed sandbox workloads and access are confirmed removed. The policy MUST allow deactivation once reason `AdminActivated` is observed.

### Suspension Status and Observability

5a. **Status subresource**: `AgenticOLSConfig` MUST have a `/status` subresource. The status MUST include a `conditions` array following the standard `metav1.Condition` shape.
5b. **Suspended condition**: When `spec.suspended` is set to `true`, the operator MUST set condition type `Suspended` with status `True`. The condition transitions through two reasons:
   - `Draining`: Set immediately while any active managed sandbox workload or associated sandbox access remains. Message SHOULD include the active sandbox count and MUST surface list/deletion/verification errors.
   - `AdminActivated`: Set only after the operator confirms that no active managed sandbox workload or associated access remains. Message SHOULD include the count of runs emergency-stopped (e.g., `"System suspended; 12 runs emergency-stopped"`).
5c. **Suspended condition on deactivation**: A request to set `spec.suspended=false` MUST be rejected while `Suspended=True` has reason `Draining` (rule 11j). Once teardown reaches `AdminActivated`, the accepted deactivation MUST update the `Suspended` condition to status `False`, reason `AdminDeactivated`, with a new `lastTransitionTime`.
5d. **Suspension Events**: The operator MUST emit a Kubernetes Event on the `AgenticOLSConfig` object when suspension is activated and when suspension is deactivated. The activation Event is emitted once when `spec.suspended` transitions to `true`, independent of whether teardown is still Draining. Event format:
   - Activation: `type: Warning`, reason `SuspensionActivated`, message `"System suspension activated; draining {N} active sandbox workloads"`.
   - Deactivation: `type: Normal`, reason `SuspensionDeactivated`, message `"System resumed; agentic operations re-enabled"`.
5e. **Status update timing**: The `Suspended` condition MUST be set immediately when `spec.suspended` becomes `true` — with reason `Draining` if any active managed sandbox workload or associated access remains, or reason `AdminActivated` if none remains. As teardown completes, the controller re-reconciles (it watches AgenticRuns and managed sandbox resources as applicable) and updates the condition from `Draining` to `AdminActivated`. A dedicated `AgenticOLSConfig` controller handles this status lifecycle.

### Reconciler Integration

12. **Watch and re-queue**: The run reconciler MUST watch `AgenticOLSConfig` and re-queue all non-terminal runs when the CR changes (same pattern as the existing `ApprovalPolicy` watch).
13. **Reconcile guard**: The suspension check MUST execute after the deletion handler and before finalizer addition, terminal phase routing, approval resolution, revision handling, and phase dispatch. Per-run cancellation is evaluated immediately after the suspension check and before those same operations.
14. **Order of operations on termination**: For each non-terminal run when suspended: (a) invoke hard-stop cleanup, including direct discovery of active managed workloads; (b) set `EmergencyStopped` condition; (c) status patch. Cleanup errors MUST NOT prevent the condition/status write, but MUST keep the config status `Draining` and trigger retry. The same ordering applies to per-run cancellation, except it sets the phase-relevant failure condition with reason `CancelledByUser`.
15. **Config fetch failure**: If the `AgenticOLSConfig` CR cannot be fetched and the error is not `NotFound`, the reconciler MUST return the error for retry. `NotFound` MUST be treated as `suspended=false`.

### Console Visibility

16. **Suspension banner**: The console plugin MUST display a cluster-wide danger alert banner when `AgenticOLSConfig.spec.suspended == true`. The banner MUST be visible on all agentic views without requiring page reload when the state changes.
17. **EmergencyStopped phase display**: The console MUST render `EmergencyStopped` runs with a distinct visual treatment (status badge, color) that is clearly different from `Failed`.
18. **DerivePhase sync**: The console's `derivePhaseFromConditions` function in `src/models/proposal.ts` MUST be updated to handle the `EmergencyStopped` condition with the same precedence as the Go implementation (per the existing `// SYNC:` contract).

### CLI Visibility

19. **Status command**: `oc agentic status` (or equivalent top-level command) MUST report the system suspension state: `"Agentic System: SUSPENDED"` when suspended, `"Agentic System: Active"` when not. When `status.conditions` includes `Suspended=True`:
   - Reason `Draining`: output SHOULD show `"SUSPENDED (draining, {message})"`.
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
- `status.conditions` — new condition type `EmergencyStopped`
- Derived phase `EmergencyStopped` added to `AgenticRunPhase` enum

### Affected repositories
- `lightspeed-agentic-operator` — CRD types, run reconciler, `AgenticOLSConfig` status controller, CLI commands, admission manifests (`ValidatingAdmissionPolicy` / binding); E2E: `test/e2e/suspension_test.go` (including admission rejection while suspended)
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
- [PLANNED: OLS-4018] Sandbox hard-stop on suspension — delete active managed Pods/SandboxClaims with zero grace, revoke associated access, keep suspension `Draining` until teardown is confirmed, and reject resumption while draining. Durable audit storage, Result CRs already published, and Kubernetes audit are the forensic sources.
- [PLANNED: future] Extend admission-time blocking beyond `AgenticRun` CREATE (e.g. reject `AgenticRunApproval` updates while suspended) only if product needs require it.
- [DONE: OLS-3295] Renamed `Proposal` references to `AgenticRun` in kill switch logic, CLI commands, and console display.
