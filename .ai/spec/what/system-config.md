# System configuration and kill switch (`AgenticOLSConfig`)

Behavioral specification for the cluster-wide agentic system configuration CR, its **emergency suspension** (kill switch) capability, deployment model, and image publishing. **Proposal lifecycle phases** are in `proposal-lifecycle.md`. **CRD field semantics** for other kinds are in `crd-api.md`.

Jira tracking: OLS-3018.

## Behavioral Rules

### Deployment Model

1. The lightspeed-agentic-operator controller is deployed via the lightspeed-operator OLM bundle. Both controllers are installed side by side at operator installation time. OLM applies all static manifests (deployments, service accounts, roles, role bindings, CRDs) for both controllers simultaneously.
2. The agentic controller is inert until its CRs (`AgenticOLSConfig`, `Agent`, `LLMProvider`, `ApprovalPolicy`) are created. No feature gate on the lightspeed-operator's `OLSConfig` controls the agentic controller.
3. The agentic controller deploys its own console plugin (the agentic console plugin), separate from the Lightspeed chat console plugin deployed by the lightspeed-operator.

### Image Publishing

4. The `lightspeed-agentic-operator` repo must have a Konflux pipeline that builds and publishes the agentic controller container image.
5. The published image is referenced in the lightspeed-operator bundle CSV as the agentic controller deployment's container image.

### AgenticOLSConfig CRD

6. **Kind and scope**: `AgenticOLSConfig` MUST be cluster-scoped in API group `agentic.openshift.io`, version `v1alpha1`.
7. **Singleton**: CRD validation MUST enforce `metadata.name == "cluster"` via CEL (same pattern as `ApprovalPolicy`).
8. **Absence semantics**: When no `AgenticOLSConfig` CR exists, the system MUST behave as if `spec.suspended` is `false` — the CR is not required for normal operation.
9. **Spec structure**: The spec MUST include:
   - `suspended` (bool, optional, default `false`): When `true`, halts all agentic operations cluster-wide.

### Emergency Suspension (`spec.suspended`)

10. **Activation**: Setting `spec.suspended` to `true` MUST immediately prevent the proposal reconciler from starting any new workflow steps (analysis, execution, verification, escalation) for any proposal cluster-wide.
11. **In-flight termination**: When `spec.suspended` becomes `true`, all non-terminal proposals MUST be terminated: sandbox pods MUST be deleted (best-effort), execution RBAC MUST be cleaned up, and the `EmergencyStopped` condition MUST be set on each proposal.
12. **EmergencyStopped condition**: The operator MUST set condition type `EmergencyStopped` with status `True`, reason `SystemSuspended`, and message `"Terminated by system kill switch (AgenticOLSConfig.spec.suspended=true)"`.
13. **EmergencyStopped is terminal — no automatic restart**: `EmergencyStopped` is a terminal phase. Proposals in this state MUST NOT resume when `spec.suspended` is set back to `false`. To retry work, the admin creates new proposals. This is a safety invariant: the kill switch exists for emergencies where agent behavior is harmful, so automatically restarting the same proposals that caused the emergency would re-introduce the exact problem the admin stopped. Resumption MUST always require explicit human action (creating new proposals).
14. **DerivePhase precedence**: `EmergencyStopped=True` MUST be checked **before** all other conditions in `DerivePhase()`. It takes precedence over `Escalated`, `Denied`, and all progress conditions.
15. **Resumption**: Setting `spec.suspended` back to `false` re-enables the system for **new** proposals only. Existing `EmergencyStopped` proposals remain terminal.
16. **New proposal blocking**: While `suspended=true`, proposals that are already in `Pending` phase (no conditions set yet) MUST also be terminated with `EmergencyStopped` — suspension applies to all non-terminal proposals, not just those with active sandboxes.

### Reconciler Integration

17. **Watch and re-queue**: The proposal reconciler MUST watch `AgenticOLSConfig` and re-queue all non-terminal proposals when the CR changes (same pattern as the existing `ApprovalPolicy` watch).
18. **Reconcile guard**: The suspension check MUST execute after the deletion handler but before finalizer addition, terminal phase routing, approval resolution, and phase dispatch.
19. **Order of operations on termination**: For each non-terminal proposal when suspended: (a) release sandbox claims via `Agent.ReleaseSandboxes` (best-effort, log errors), (b) clean up execution RBAC via `cleanupExecutionRBAC` (best-effort, log errors), (c) set `EmergencyStopped` condition, (d) status patch. Errors in (a) or (b) MUST NOT prevent (c) and (d).
20. **Config fetch failure**: If the `AgenticOLSConfig` CR cannot be fetched and the error is not `NotFound`, the reconciler MUST return the error for retry. `NotFound` MUST be treated as `suspended=false`.

### Console Visibility

21. **Suspension banner**: The console plugin MUST display a cluster-wide danger alert banner when `AgenticOLSConfig.spec.suspended == true`. The banner MUST be visible on all agentic views without requiring page reload when the state changes.
22. **EmergencyStopped phase display**: The console MUST render `EmergencyStopped` proposals with a distinct visual treatment (status badge, color) that is clearly different from `Failed`.
23. **DerivePhase sync**: The console's `derivePhaseFromConditions` function in `src/models/proposal.ts` MUST be updated to handle the `EmergencyStopped` condition with the same precedence as the Go implementation (per the existing `// SYNC:` contract).

### CLI Visibility

24. **Status command**: `oc agentic status` (or equivalent top-level command) MUST report the system suspension state: `"Agentic System: SUSPENDED"` when suspended, `"Agentic System: Active"` when not.
25. **Suspend/resume commands**: The CLI MUST provide `oc agentic suspend` and `oc agentic resume` commands that patch `AgenticOLSConfig.spec.suspended` to `true` and `false` respectively.
26. **Suspend confirmation**: `oc agentic suspend` MUST prompt for confirmation before proceeding: `"All agentic operations will be halted and in-flight proposals will be terminated. Continue? [y/N]"`.
27. **Proposal list**: `oc agentic proposals` (or equivalent list command) MUST display `EmergencyStopped` as a distinct phase value in the phase/status column.

## Configuration Surface

### AgenticOLSConfig
- `metadata.name` (must be `cluster`)
- `spec.suspended` (bool, default `false`)

### Affected Proposal fields
- `status.conditions` — new condition type `EmergencyStopped`
- Derived phase `EmergencyStopped` added to `ProposalPhase` enum

### Affected repositories
- `lightspeed-agentic-operator` — CRD types, proposal reconciler, CLI commands, Konflux pipeline
- `lightspeed-operator` — OLM bundle manifests (CSV, CRD sync), agentic controller deployment spec
- `lightspeed-agentic-console` — `derivePhaseFromConditions` sync, suspension banner, phase display

## Constraints

- `EmergencyStopped` MUST be added to `isTerminal()` in the reconciler and any console/CLI equivalents.
- The `AgenticOLSConfig` controller RBAC MUST include `get`, `list`, `watch` on `agenticolsconfigs` for the proposal reconciler's service account.
- The `oc agentic suspend` / `resume` commands require the user to have `patch` permissions on `AgenticOLSConfig`.
- Termination of in-flight proposals via Approach A (reconciler re-queue) is bounded by `maxConcurrentReconciles`; at default concurrency (5) with 100 proposals, termination completes in approximately 4-8 seconds. This is acceptable for v1. If real-world scale requires faster termination, a batch-sweep approach (Approach B) can be added to the `AgenticOLSConfig` reconciler without changing any other component.

## Planned Changes

- [PLANNED: future] Batch-sweep termination (Approach B): if Approach A's reconciler-based termination proves too slow at scale, add a direct sweep in the `AgenticOLSConfig` reconciler that lists and terminates all non-terminal proposals in a single pass with goroutine fan-out.
- [PLANNED: future] Additional config fields (e.g., system-wide defaults, feature gates) can be added to the `AgenticOLSConfig` spec as needed.
