# Shared stop path for per-run cancellation

Date: 2026-11-10
Status: Approved
Target: OLS-4132, building on the global suspension behavior shipped by OLS-4133

## Context

OLS-4133 introduced the run-level `Stopping` condition, a bounded global-suspension outcome, and hub cleanup confirmation. OLS-4132 adds the one-way `AgenticRun.spec.cancelled` request. Maintaining separate cancellation and suspension state machines would duplicate cleanup, deadline, precedence, and finality behavior, while a second controller writing run status would introduce races.

The global kill-switch decision in ADR 0001 intentionally deferred a shared stop extraction until a second stop source existed. OLS-4132 now supplies that source and records the implementation decision below.

## Decision

### One source-parameterized stop path

The run controller uses one `handleRunStop` path parameterized by `stopSource` (`suspension` or `cancellation`). It persists stop acceptance, invokes hub cleanup and any required spoke cleanup, records the confirmed or failed outcome, and continues background cleanup after a failed outcome. The existing condition-derived phase model remains unchanged: cancellation is not a new phase.

`spec.cancelled` is optional, effectively defaults to `false`, and is one-way under CRD CEL. Ordinary resource-wide `patch agenticruns` authorization is sufficient; no field-level cancellation permission or new operator ClusterRole is added. The accepted cancellation marker is `Stopping=True/CancelledByUser`; its 10-second cleanup deadline starts at that condition's `lastTransitionTime` (`SuspensionStopWindow`). Global suspension keeps its existing 10-second deadline from activation start.

### Stop-source preference and finality

1. A cancellation request first observed after a run already has a historical terminal outcome does not accept a new cancellation or rewrite that outcome. A terminal run eligible for an active global-suspension cohort may receive auxiliary cleanup intent, but its workflow conditions and TTL history remain unchanged; definitively pre-activation terminal runs remain untouched.
2. Current global suspension is evaluated before cancellation. If both are pending and there is no final stop outcome, suspension wins, including takeover of `Stopping=True/CancelledByUser`.
3. Once `Stopping=True/SystemSuspended` is accepted, that source and its activation timestamp remain authoritative across config resumption or deletion. A later `spec.cancelled=true` MUST NOT convert the accepted suspension to cancellation.
4. Otherwise, cancellation is accepted for a non-terminal run before approval, revision, terminal routing, or phase dispatch. `readRunnableRun` protects workflow writes, async result/timeout paths, and sandbox creation after acceptance.
5. A recorded stop outcome is final. Confirmed cancellation records the phase-relevant condition `False/CancelledByUser` with message `Run stopped by user`, `Stopping=False/TeardownComplete`, and cleared sandbox references; this derives the existing `Failed` phase. Unconfirmed cancellation at its acceptance deadline records `False/CancellationFailed` and `Stopping=False/TeardownFailed`, retaining references. If later cleanup confirms, the run controller may only set `Stopping=False/TeardownComplete` and clear references; it never rewrites the `CancellationFailed` condition or `Failed` phase.

The cancellation phase-condition mapping remains the existing mapping: `Pending`/`Analyzing` → `Analyzed`, `Proposed`/`Executing` → `Executed`, `Verifying` → `Verified`, and `Escalating` → `Escalated`.

### Deterministic-name spoke discovery

The run controller confirms target-cluster access with GET/DELETE by the deterministic names used when it creates spoke resources, followed by an authoritative GET. An initial GET returning `NotFound` confirms absence; otherwise the helper checks run-UID identity, uses a UID-preconditioned DELETE, and treats a final GET returning `NotFound` as confirmation. No spoke LIST calls are made.

The known names are:

- `sandboxSAName(run, step)` for each workflow step in `SpokeAccess.Namespace`;
- `perRunCRBName(runUID, step, index)` for every `spokeReaderBindingNames` entry and step;
- `executionRoleName(runUID)` for each execution Role and RoleBinding namespace in `agentic.openshift.io/rbac-namespaces`;
- `clusterRoleName(runUID)` for the execution ClusterRole and ClusterRoleBinding.

The `rbac-namespaces` annotation is a sorted, deduplicated union of its existing values and namespaces targeted by each new execution. This preserves the namespaces that may still contain execution RBAC across revisions. The union MUST be persisted on the AgenticRun with an optimistic-lock update before creating the first namespace-scoped spoke Role or RoleBinding. A failed or conflicting write aborts creation without that RBAC; a retry re-reads and persists the union again. This write-ahead inventory ensures a partial create or crash cannot leave access outside deterministic-name cleanup.

Name-only discovery is selected over label inventory plus a LIST permission grant. The standing MCE spoke credential has GET and DELETE permissions on known RBAC objects and per-step ServiceAccounts, but not LIST; expanding that cross-cluster credential would require a hub change. The operator creates spoke objects only under the deterministic names above, so known-name GET/DELETE is sufficient without a hub RBAC change. A missing kubeconfig, unreachable spoke, forbidden GET/DELETE, or any other non-`NotFound` GET/DELETE/confirmation error is unconfirmed, and the run controller does not fall back to hub credentials. This choice can miss a run-labeled spoke object created under an unexpected or legacy name; the operator does not create such objects.

### Controller responsibilities and safety resolution

The run controller is the only component that performs spoke I/O. For every accepted stop it runs `CleanupSuspension(ctx, run.UID, nil)` for the hub and, for a target-cluster run, `cleanupSpokeRunAccess` for spoke resources. It owns per-run outcomes, late cleanup, and the deletion finalizer.

The config controller owns global activation status, global hub inventory, durable suspension stop markers, and `Suspended`/`SafeToResume` reporting. It performs no spoke reads or deletes. It classifies each affected run from the run controller's recorded status: Resolved (`StopCleanupConfirmed`: all `status.steps.*.sandbox` references are clear and either no `Stopping` condition exists or `Stopping=False/TeardownComplete` is recorded); Unverified (a recorded final failure with `Stopping=False/TeardownFailed`); or Pending (any other unconfirmed state, including `Stopping=True`, `Stopping=Unknown`, or retained references without `TeardownFailed`). Unverified runs count as unresolved for the activation outcome and are listed as `run stop unverified: <ns>/<name> uid=<uid>`, but do not by themselves block `SafeToResume=True`. Pending runs are listed as `run stop unresolved: <ns>/<name> uid=<uid>` and block it. `RemoteExecutionEvidence` is not the resolution signal for the shared stop path.

`SafeToResume=True` additionally requires confirmed global hub cleanup, saved stop-acceptance markers, a readable run inventory, and no Pending runs for the current suspended generation. When Unverified runs remain, the condition uses reason `RunCleanupUnverified` and a message naming them; otherwise it uses `CleanupConfirmed`. A terminal `AdminActivatedWithErrors` outcome remains unchanged after recovery; background cleanup may update `SafeToResume` independently when the requirements hold, including while Unverified outcomes remain.

For a deleting target-cluster run with an accepted cancellation or suspension, the cleanup finalizer remains while hub/spoke cleanup is unconfirmed, including after the 10-second stop-outcome deadline. After the 60-second deletion retry window, finalizer release MAY be degraded for either accepted stop source only when the recorded `Stopping=False/TeardownFailed` outcome matches that source, hub cleanup is confirmed, and the spoke kubeconfig Secret is NotFound or a live spoke probe returns a qualifying network/transport error (`isDegradableSpokeError`). The recorded `Stopping.message` can only veto release when it contains a non-degradable marker (forbidden/unauthorized, label/name mismatch, missing UID, kubeconfig key/parse problem, insecure TLS, external credential provider, bad request, method not allowed, permission denied, or at least 1024 runes); it need not contain a transport marker. A degraded release emits a Warning Event with reason `SpokeCleanupDegraded`. The degraded path MUST NOT run while `AgenticOLSConfig.spec.suspended=true`, so the finalizer remains retained then. A deleting run with recorded `TeardownFailed` is Unverified, not Pending, for `SafeToResume`; its retained finalizer and `deletionTimestamp` do not alone block safety, though other unconfirmed run states still do. Resumption may therefore proceed while spoke cleanup remains unverified if the `SafeToResume` prerequisites hold; the run controller continues retries after resume, but spoke access may remain if that spoke returns before cleanup is confirmed.

## Supersession scope

This ADR supersedes ADR 0001's forward-looking deferral of extracting a shared stop path for future cancellation. For recorded `TeardownFailed` runs only, it also supersedes ADR 0001's conclusions that unverified remote access keeps `SafeToResume=False` and unresolved execution blocks resumption: those runs are Unverified, not Pending, and may permit resumption when the current `SafeToResume` prerequisites are met. ADR 0001 remains the historical record of the OLS-4133 global kill-switch decision; its unrelated requirements are not retroactively rewritten or superseded.
