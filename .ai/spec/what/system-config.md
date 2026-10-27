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
6. **In-flight termination**: When suspension is activated, workflow progression MUST stop immediately for every non-terminal run. The run controller MUST persist `Stopping=True` before normal progression or asynchronous workflow writes can continue, then invoke the hub-local hard-stop contract: delete active bare Pods with zero grace, remove managed claims and other workload-creation sources, and revoke temporary ServiceAccount and RBAC access. Cleanup MUST discover resources through run status and managed UID/owner identity. A successful DELETE request alone is not confirmation. Confirmed workload and access removal plus durable stop acceptance records `EmergencyStopped=True/SystemSuspended` and completes `Stopping` in one status update. If per-run cleanup remains unresolved at the activation deadline, the run MUST record a terminal `Failed` outcome with the phase-relevant `EmergencyStopFailed` condition and complete `Stopping` with `TeardownFailed`; it MUST retain unconfirmed sandbox references and MUST NOT claim `EmergencyStopped`. Cleanup continues after either terminal outcome without restarting the run. See `agentic-run-termination.md`.
7. **EmergencyStopped condition**: The operator MUST set condition type `EmergencyStopped` with status `True` and reason `SystemSuspended` only after that run's managed sandbox workloads and associated access are confirmed removed. It MUST NOT write this successful outcome for an unconfirmed sandbox; unresolved run teardown uses terminal `Failed/EmergencyStopFailed` after the initial deadline.
8. **EmergencyStopped is terminal — no automatic restart**: `EmergencyStopped` is a terminal phase. Runs in this state MUST NOT resume when `spec.suspended` is set back to `false`. To retry work, the admin creates new runs. This is a safety invariant: the kill switch exists for emergencies where agent behavior is harmful, so automatically restarting the same runs that caused the emergency would re-introduce the exact problem the admin stopped. Resumption MUST always require explicit human action (creating new runs).
9. **DerivePhase precedence**: `EmergencyStopped=True` MUST be checked **before** all other conditions in `DerivePhase()`. It takes precedence over `Escalated`, `Denied`, and all progress conditions.
10. **Resumption**: After a suspended activation has a terminal `Suspended=True` outcome (`AdminActivated` or `AdminActivatedWithErrors`), setting `spec.suspended` to `false` re-enables only new runs. Both `Suspended` and `SafeToResume` MUST be `True` for the object's current generation; terminal error completion alone is not permission. Existing accepted runs remain terminal and cannot restart. The resume request uses ordinary Kubernetes authorization and is accepted or denied by the API server (rule 11j).
11. **New run blocking**: While `suspended=true`, runs that are already in `Pending` phase (no conditions set yet) MUST also be terminated with `EmergencyStopped` — suspension applies to all non-terminal runs, not just those with active sandboxes.

### Admission-Time Blocking (OLS-3267)

Primary enforcement so new runs never persist while the system is suspended. Spike design: OLS-3166 / closed PR #112 (updated for `AgenticRun` rename).

11a. **AgenticRun CREATE rejection**: While `AgenticOLSConfig.spec.suspended` is `true`, the API server MUST reject `AgenticRun` CREATE requests at admission time with a clear error message stating that the agentic system is suspended. Enforcement MUST use a `ValidatingAdmissionPolicy` whose `spec.paramKind` is explicitly `apiVersion: agentic.openshift.io/v1alpha1`, `kind: AgenticOLSConfig`, with a binding `paramRef` to that kind.
11b. **CEL source of truth**: The admission policy CEL expression MUST evaluate `params.spec.suspended` on the referenced `AgenticOLSConfig`. It MUST NOT key off `status.conditions` (including the `Suspended` condition) — admission follows the admin-set kill switch immediately. Without an explicit `paramKind`, `params` is unavailable and this CEL cannot evaluate. The expression MUST use presence-safe navigation (e.g. `!has(params.spec.suspended) || !params.spec.suspended`) so an absent optional field evaluates as not suspended. The CRD MUST default `spec.suspended` to `false` for stored objects; that defaulting is the primary guarantee that `has(params.spec.suspended)` is true after normal create/apply.
11c. **Absence allows creation**: If no `AgenticOLSConfig` CR exists, admission MUST allow `AgenticRun` creation. Consistent with rule 3 (absence = `suspended=false`). Enforced via `parameterNotFoundAction: Allow` on the policy binding, with `paramRef.name` equal to `cluster`.
11d. **CREATE-only on `agenticruns`**: The AgenticRun policy resource rule MUST only intercept `CREATE` operations on `agenticruns` (`apiGroup: agentic.openshift.io`, `apiVersion: v1alpha1`). Updates to existing `AgenticRun` objects MUST NOT be blocked. Other kinds in the API group MUST NOT be matched by this run-creation policy. Separate `AgenticOLSConfig` UPDATE and DELETE protection is defined by rule 11j. `matchPolicy` MAY remain the API default (`Equivalent`) so future equivalent served API versions of `agenticruns` remain covered; do not set `Exact` unless deliberately pinning to a single version.
11e. **Policy evaluation failure**: The `ValidatingAdmissionPolicy` MUST set `failurePolicy: Fail` so that if an installed and active policy cannot be evaluated, `AgenticRun` CREATE is denied. `failurePolicy: Fail` does not deny CREATE when the policy or binding is absent, ignored, or never installed — rule 11h (reconciler) covers that case.
11f. **Deny action**: The `ValidatingAdmissionPolicyBinding` MUST set `validationActions: [Deny]`.
11g. **Static install with operator**: The AgenticRun CREATE policy/binding and the separate AgenticOLSConfig UPDATE/DELETE safety policy/binding MUST be static manifests under `config/admission/`, wired into `config/default` (so `make deploy` / `make undeploy` install and remove them), and applied by quickstart `deploy-operator.sh`. Quickstart deployment MUST apply each policy before its binding; `undeploy-operator.sh` MUST remove bindings before policies, and explicit uninstall MUST remove admission before CRD teardown. No operator reconciler manages these resources. There is no OLM bundle for this operator today; do not assume OLM applies them.

11h. **Defense-in-depth**: The reconciler guard (rules 12–15) MUST remain as fallback enforcement. The admission policy is primary for new CREATE; the reconciler catches race conditions during suspension toggle, pre-existing non-terminal runs, and VAP removal or misinstallation.
11i. **Approvals while suspended**: `AgenticRunApproval` CREATE/UPDATE (including human approve/deny PATCH) MUST remain allowed while suspended. The run reconciler MUST NOT start new workflow steps while suspended (rules 5, 13), so an approval written during suspension has no execution effect on a run that is or will become `EmergencyStopped`. Extending admission to block approvals or other kinds is deferred ([PLANNED: future]) if product needs require it later.
11j. [DONE: OLS-4133] **Safe resumption and suspended-config deletion**: A static self-object `ValidatingAdmissionPolicy` and binding MUST reject an `AgenticOLSConfig` UPDATE that changes `spec.suspended` from `true` to `false`, omits/removes the field, or a DELETE of a suspended config unless the old object has both a current-generation `Suspended=True` condition with reason `AdminActivated` or `AdminActivatedWithErrors` and a current-generation `SafeToResume=True` condition. Missing, stale, `False`, `Unknown`, or `Stopping` status MUST be rejected, including before the first controller status write. The policy MUST match only main-resource `AgenticOLSConfig` UPDATE/DELETE and MUST NOT use `paramKind` or `paramRef`. Updates retaining `spec.suspended=true`, metadata-only updates, and `/status` writes remain allowed; update and deletion are allowed when the config is not suspended. `resume` remains an ordinary API patch and MUST surface the server's admission error. This gate remains effective after error completion until cleanup and affected-run stop protection are confirmed.

### Suspension Status and Observability

5a. **Status subresource**: `AgenticOLSConfig` MUST have a `/status` subresource. The status MUST include a `conditions` array following the standard `metav1.Condition` shape, including `Suspended` and the independent `SafeToResume` condition.
5b. **Suspended condition**: When `spec.suspended` is set to `true`, the operator MUST set condition type `Suspended` with status `True`. It reports the interim reason `Stopping` and one terminal outcome:
   - Global reason `Stopping`: The config controller records current-generation `Suspended=True/Stopping` and `SafeToResume=False` before cleanup. This is the interim outcome during the single activation window. The message SHOULD identify pending managed resources, affected run outcomes, and unsaved stop markers.
   - `AdminActivated`: The operator confirms hub-local workload/access removal and durable stop acceptance and terminal outcomes for all affected non-terminal runs before the initial deadline.
   - `AdminActivatedWithErrors`: At the activation-start-plus-ten-seconds deadline, cleanup, a run marker/outcome, or known remote work remains unresolved. This is terminal for that activation, includes actionable diagnostics, and does not itself permit resumption. Background cleanup MUST NOT return this activation to `Stopping` or clean completion; it updates `SafeToResume` independently.
5c. **Suspended condition on deactivation**: An UPDATE that clears `spec.suspended` and a DELETE of a suspended config MUST be rejected unless both current-generation conditions required by rule 11j are present. After accepted deactivation, the controller MUST set `Suspended=False/AdminDeactivated` with a new `lastTransitionTime`.
5d. **Suspension Events**: The operator MUST emit Kubernetes Events for suspension activation and deactivation on the `AgenticOLSConfig` object.
   Activation emission occurs when the operator accepts suspension, before cleanup completes.
   Event delivery remains best-effort across crashes; the operator does not maintain an Event ledger.
   - Activation: `type: Warning`, reason `SuspensionActivated`, with a message describing pending workload and access cleanup.
   - Deactivation: `type: Normal`, reason `SuspensionDeactivated`, message `"System resumed; agentic operations re-enabled"`.
5e. **Status update timing**: Before cleanup, the config controller MUST persist the current suspended generation's activation start and `Suspended=True/Stopping` with `SafeToResume=False`. The single deadline is activation start plus ten seconds; retries, reason/message changes, and operator restarts MUST NOT extend it. Clean work produces `AdminActivated`; unresolved work at the deadline produces terminal `AdminActivatedWithErrors`. Cleanup may continue while suspended, but it MUST preserve that error outcome and update only independent safety after the deadline. A late resource found before the deadline may return the condition to `Stopping` without resetting the deadline. The activation Event MUST be emitted when `spec.suspended` becomes true.
5f. **Resumption-safety condition**: `SafeToResume=True` MUST mean confirmed hub workload/access cleanup and durable stop acceptance for the current suspended generation.
   Run outcomes may remain pending when durable stop acceptance already prevents workflow progression.
   Admission additionally requires a current terminal `Suspended` outcome.
   Missing, stale, false, unknown, or unverified cleanup evidence MUST NOT be treated as safe.
   Background cleanup MAY establish safety while preserving terminal `AdminActivatedWithErrors`.
   Known unverified remote work keeps safety false; this change adds no remote discovery or verification.

### Reconciler Integration

12. **Watch and re-queue**: The run reconciler MUST watch `AgenticOLSConfig` and re-queue all non-terminal runs when the CR changes (same pattern as the existing `ApprovalPolicy` watch).
13. **Reconcile guard**: The run reconciler MUST evaluate authoritative suspension and persisted accepted-stop state before config-cache readiness, approval resolution, revision handling, normal terminal shortcuts, or phase dispatch. Async result/timeout writers and sandbox creation MUST respect the same state; a persisted stop marker remains effective after resume or config deletion.
14. **Order of operations on termination**: For each non-terminal run when suspended, the operator MUST persist `Stopping=True` before permitting any further workflow progression. It MUST then attempt workload and access cleanup and confirm through authoritative API reads. Confirmed cleanup records `EmergencyStopped=True/SystemSuspended` and `Stopping=False/TeardownComplete` together. If run cleanup remains unresolved at its copied activation deadline, it MUST record terminal `Failed/EmergencyStopFailed` and `Stopping=False/TeardownFailed` together, retaining unconfirmed sandbox references and retrying cleanup without changing the terminal outcome. The config controller MUST report `AdminActivatedWithErrors` at the global deadline even if a run stop marker or outcome could not be saved; that unsaved protection keeps `SafeToResume=False`. Per-run cancellation remains a separate deferred trigger.
15. **Config fetch failure**: If the `AgenticOLSConfig` CR cannot be fetched and the error is not `NotFound`, the reconciler MUST return the error for retry. `NotFound` MUST be treated as `suspended=false`.

### Console Visibility

16. **Suspension banner**: The console plugin MUST display a cluster-wide danger alert banner when `AgenticOLSConfig.spec.suspended == true`. The banner MUST be visible on all agentic views without requiring page reload when the state changes.
17. **EmergencyStopped phase display**: The console MUST render `EmergencyStopped` runs with a distinct visual treatment (status badge, color) that is clearly different from `Failed`.
18. **DerivePhase sync**: The console's `derivePhaseFromConditions` function in `src/models/proposal.ts` MUST be updated to handle the `EmergencyStopped` condition with the same precedence as the Go implementation (per the existing `// SYNC:` contract).

### CLI Visibility

19. **Status command**: `oc agentic status` MUST report whether the system is active or suspended. For current-generation `Suspended=True`, it SHOULD show the outcome or progress and diagnostics; terminal outcomes SHOULD include relative/absolute transition time, and `AdminActivatedWithErrors` diagnostics MUST remain visible. While suspended, it MUST report whether current-generation `SafeToResume=True` is confirmed. Missing or stale safety MUST be reported as unconfirmed and MUST NOT appear resumable. With `spec.suspended=false` and `Suspended/AdminDeactivated`, output SHOULD include when the system resumed.
20. **Suspend/resume commands**: The CLI MUST provide `oc agentic suspend` and `oc agentic resume` commands that patch `AgenticOLSConfig.spec.suspended` to `true` and `false` respectively. `resume` MUST leave authorization to Kubernetes RBAC and MUST return API admission denials unchanged to the caller.
21. **Suspend confirmation**: `oc agentic suspend` MUST prompt for confirmation before proceeding: `"All agentic operations will be halted and in-flight runs will be terminated. Continue? [y/N]"`.
22. **Run list**: `oc agentic runs` (or equivalent list command) MUST display `EmergencyStopped` as a distinct phase value in the phase/status column.

## Configuration Surface

### AgenticOLSConfig
- `metadata.name` (must be `cluster`)
- `spec.suspended` (bool, default `false`)
- `status.conditions` — condition types: `Suspended` and `SafeToResume`

### Affected AgenticRun fields
- `status.conditions` — condition types `EmergencyStopped` and `Stopping`
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
- [DONE: OLS-4133] Implement the bounded global suspension outcome and independent current-generation safety contract: one ten-second activation deadline, terminal `AdminActivatedWithErrors` with diagnostics when work remains, continued cleanup without resetting the outcome, and resume/deletion admission requiring both current-generation terminal and safe conditions.
- [PLANNED: OLS-3298] Per-run cancellation remains deferred; this global-suspension work adds no `spec.cancelled` field, cancellation-specific authorization, or new cancellation trigger. The shared run-level `Stopping` condition is used by global suspension.
- [PLANNED: future] Extend admission-time blocking beyond `AgenticRun` CREATE (e.g. reject `AgenticRunApproval` updates while suspended) only if product needs require it.
- [DONE: OLS-3295] Renamed `Proposal` references to `AgenticRun` in kill switch logic, CLI commands, and console display.
