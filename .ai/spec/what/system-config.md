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
6. **In-flight termination**: When `spec.suspended` becomes `true`, all non-terminal runs MUST be terminated: sandbox pods MUST be deleted (best-effort), execution RBAC MUST be cleaned up, and the `EmergencyStopped` condition MUST be set on each run.
7. **EmergencyStopped condition**: The operator MUST set condition type `EmergencyStopped` with status `True`, reason `SystemSuspended`, and message `"Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)"`.
8. **EmergencyStopped is terminal — no automatic restart**: `EmergencyStopped` is a terminal phase. Runs in this state MUST NOT resume when `spec.suspended` is set back to `false`. To retry work, the admin creates new runs. This is a safety invariant: the kill switch exists for emergencies where agent behavior is harmful, so automatically restarting the same runs that caused the emergency would re-introduce the exact problem the admin stopped. Resumption MUST always require explicit human action (creating new runs).
9. **DerivePhase precedence**: `EmergencyStopped=True` MUST be checked **before** all other conditions in `DerivePhase()`. It takes precedence over `Escalated`, `Denied`, and all progress conditions.
10. **Resumption**: Setting `spec.suspended` back to `false` re-enables the system for **new** runs only. Existing `EmergencyStopped` runs remain terminal.
11. **New run blocking**: While `suspended=true`, runs that are already in `Pending` phase (no conditions set yet) MUST also be terminated with `EmergencyStopped` — suspension applies to all non-terminal runs, not just those with active sandboxes.

### Suspension Status and Observability

5a. **Status subresource**: `AgenticOLSConfig` MUST have a `/status` subresource. The status MUST include a `conditions` array following the standard `metav1.Condition` shape.
5b. **Suspended condition**: When `spec.suspended` is set to `true`, the operator MUST set condition type `Suspended` with status `True`. The condition transitions through two reasons:
   - `Draining`: Set immediately when non-terminal runs still exist. Message SHOULD include the pending count (e.g., `"Waiting for 3 runs to terminate"`).
   - `AdminActivated`: Set once all runs are terminal. Message SHOULD include the count of runs emergency-stopped (e.g., `"System suspended; 12 runs emergency-stopped"`).
5c. **Suspended condition on deactivation**: When `spec.suspended` is set back to `false`, the operator MUST update the `Suspended` condition to status `False`, reason `AdminDeactivated`, preserving the new `lastTransitionTime`.
5d. **Suspension Events**: The operator MUST emit a Kubernetes Event on the `AgenticOLSConfig` object when suspension is activated and when suspension is deactivated. Event format:
   - Activation: `type: Warning`, reason `SuspensionActivated`, message `"System suspended; {N} runs emergency-stopped"`.
   - Deactivation: `type: Normal`, reason `SuspensionDeactivated`, message `"System resumed; agentic operations re-enabled"`.
5e. **Status update timing**: The `Suspended` condition MUST be set immediately when `spec.suspended` becomes `true` — with reason `Draining` if non-terminal runs remain, or reason `AdminActivated` if all are already terminal. As runs terminate, the controller re-reconciles (it watches AgenticRuns as a secondary resource) and updates the condition from `Draining` to `AdminActivated` with the final count. The activation Event MUST be emitted only on the `AdminActivated` transition, not during `Draining`. A dedicated `AgenticOLSConfig` controller handles this status lifecycle.

### Reconciler Integration

12. **Watch and re-queue**: The run reconciler MUST watch `AgenticOLSConfig` and re-queue all non-terminal runs when the CR changes (same pattern as the existing `ApprovalPolicy` watch).
13. **Reconcile guard**: The suspension check MUST execute after the deletion handler but before finalizer addition, terminal phase routing, approval resolution, and phase dispatch.
14. **Order of operations on termination**: For each non-terminal run when suspended: (a) release sandbox claims via `Agent.ReleaseSandboxes` (best-effort, log errors), (b) clean up execution RBAC via `cleanupExecutionRBAC` (best-effort, log errors), (c) set `EmergencyStopped` condition, (d) status patch. Errors in (a) or (b) MUST NOT prevent (c) and (d).
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
- `lightspeed-agentic-operator` — CRD types, run reconciler, `AgenticOLSConfig` status controller, CLI commands; E2E: `test/e2e/suspension_test.go`
- `lightspeed-agentic-console` — `derivePhaseFromConditions` sync, suspension banner, phase display

## Constraints

- `EmergencyStopped` MUST be added to `isTerminal()` in the reconciler and any console/CLI equivalents.
- The `AgenticOLSConfig` controller RBAC MUST include `get`, `list`, `watch` on `agenticolsconfigs` for the run reconciler's service account.
- The `oc agentic suspend` / `resume` commands require the user to have `patch` permissions on `AgenticOLSConfig`.
- Termination of in-flight runs via Approach A (reconciler re-queue) is bounded by `maxConcurrentReconciles`; at default concurrency (5) with 100 runs, termination completes in approximately 4-8 seconds. This is acceptable for v1. If real-world scale requires faster termination, a batch-sweep approach (Approach B) can be added to the `AgenticOLSConfig` reconciler without changing any other component.

## Planned Changes

- [PLANNED: future] Batch-sweep termination (Approach B): if Approach A's reconciler-based termination proves too slow at scale, add a direct sweep in the `AgenticOLSConfig` reconciler that lists and terminates all non-terminal runs in a single pass with goroutine fan-out.
- [PLANNED: future] Additional config fields (e.g., system-wide defaults, feature gates) can be added to the `AgenticOLSConfig` spec as needed.
- [PLANNED: OLS-3267] Admission-time run blocking via `ValidatingAdmissionPolicy` with `paramRef` to reject `AgenticRun` creation at the API server when suspended. See spike OLS-3166 for design. VAP/binding lifecycle mechanism deferred to OLS-3302.
- [PLANNED: OLS-3267] Sandbox pod isolation on suspension — isolate running sandbox pods without deleting them for post-incident forensics. Blocked on durable sandbox pod log mechanism (separate RFE).
- [DONE: OLS-3295] Renamed `Proposal` references to `AgenticRun` in kill switch logic, CLI commands, and console display.
