# Global kill-switch confirmation and bounded stopping

Date: 2026-10-26
Status: Approved, including the resumption-safety revision; the user selected subagent-driven implementation on 2026-10-26.
Target: Follow-up to merged operator PR #575, before implementing per-run cancellation.

## Intent

Strengthen the existing global kill switch before adding another stop trigger.
Suspension must immediately request sandbox Pod deletion and remove temporary sandbox access.
The operator must distinguish confirmed cleanup from cleanup that could not finish.
Stopping must finish with an explicit outcome rather than hide unresolved cleanup behind an indefinite stopping label.

## Agreed requirements

The user selected these requirements during the design discussion:

- Build global suspension first, extending the current PR.
- Extend existing controllers and helpers; avoid premature shared-core extraction.
- Request immediate sandbox Pod deletion, then confirm removal.
- Remove temporary sandbox access and confirm that cleanup too.
- Show stopping progress until cleanup finishes or the cleanup window expires.
- Reject resume and suspension-config deletion while stopping.
- Treat completion with errors as terminal, but permit resumption only after old sandbox execution is prevented.
- Use a ten-second total cleanup window, rather than the previously proposed sixty seconds.
- Record unsuccessful run cleanup as terminal `Failed` with reason `EmergencyStopFailed`, retaining unresolved sandbox references.
- Report unverified spoke cleanup as global completion with errors, without adding new multicluster cleanup machinery.
- Keep resume and config deletion blocked when old workloads or irreversible run-stop protection remain unconfirmed.

The previously established scope remains hub-local.
Per-run cancellation and new multicluster handling remain deferred.

## Approaches

| Approach | Tradeoff | Decision |
|---|---|---|
| Extend existing controllers and cleanup helpers | Keeps the change focused; later cancellation may require extracting common logic. | Selected by the user. |
| Extract a shared stop operation immediately | Prepares two triggers, but restructures code before the second trigger exists. | Rejected as premature. |

No third controller, generic teardown framework, or new dependency is required.

## Current behavior

`agenticrun.handleSuspension` requests zero-grace release through `ReleaseSandboxes(..., true)`.
It logs cleanup errors but still writes `EmergencyStopped=True`.
Cleanup mainly follows sandbox references recorded in run status.

The config controller already uses the global condition reason `Stopping`.
It decides completion from run phases, without confirming resource removal.
The existing admission policy rejects new runs while suspended.
It does not protect config UPDATE or DELETE during stopping.

Normal completion and deletion also use existing release helpers.
Their ordinary cleanup behavior must remain unchanged outside suspension.

## Outcomes and administrative behavior

| Config condition | Meaning | Resume or config deletion |
|---|---|---|
| `Suspended=True`, reason `Stopping` | Cleanup or affected run outcomes remain pending within the cleanup window. | Rejected. |
| `Suspended=True`, reason `AdminActivated` | Cleanup is confirmed and affected run outcomes are recorded. | Allowed only with current `SafeToResume=True`. |
| `Suspended=True`, reason `AdminActivatedWithErrors` | The initial stopping window ended with unresolved cleanup or failed confirmation. | Rejected until current `SafeToResume=True`. |
| `Suspended=False`, reason `AdminDeactivated` | An administrator resumed the system. | Normal behavior. |

Completion never automatically resumes the system.
Both completion outcomes keep admission blocking new runs until an administrator resumes.
Error completion must identify failed operations and resources whose removal remains unconfirmed.
An error outcome does not certify that an inaccessible sandbox stopped running.

Use one additional config status condition, `SafeToResume`, to separate stopping outcome from resumption safety.
It remains false while required workload/access cleanup or affected run-stop acceptance remains unconfirmed.
Require current-generation confirmation before setting it true.
Use conservative cleanup confirmation rather than introducing harmless-versus-dangerous RBAC error classification.
Terminal error completion may therefore remain administratively suspended until the reported safety issue is resolved.
This deliberately supersedes the earlier proposal that every terminal error immediately permits resumption.

## Activation and deadline

The operator records a durable activation start before beginning cleanup.
The deadline is that activation start plus ten seconds.
The window covers cleanup attempts, retries, and confirmation together.
It is not ten seconds per resource, run, controller, or retry.

Implementation choice: reuse the global condition timestamp and observed generation when sufficient.
Every new suspended generation must establish a fresh activation, including rapid resume/suspend sequences.
Reason changes, message changes, retries, and operator restarts must not extend the current window.
The builder may add minimal persisted status only if existing condition fields cannot represent this safely.

Use bounded API requests within the remaining window.
Do not use existing long waits or nested retry loops that exceed the deadline.
Continue other applicable cleanup operations after an individual failure.
Report unfinished or unattempted operations when the window expires.

The deadline bounds cleanup attempts, not Kubernetes API availability.
If the API cannot accept status writes, the operator records error completion when connectivity returns.
Resume requests also require a functioning API server and admission evaluation.
The design cannot guarantee an externally visible terminal status during a control-plane outage.

## Workload and access scope

Discover resources using both recorded sandbox references and existing managed labels or owner identity.
Resource identity must tie back to managed runs, using run UIDs rather than names alone.
The global sweep includes resources belonging to terminal runs and resources missing from run status.
Preserve the historical outcomes of runs that were already terminal when suspension was accepted.
Already-completed Pods need not be deleted.

For active hub-local sandboxes:

1. Request bare-Pod and claim-backed Pod deletion with zero grace immediately.
2. Remove claims and other run-owned creation sources needed to prevent replacement Pods.
3. Remove temporary sandbox ServiceAccounts and their entries in shared reader bindings.
4. Remove run-specific execution Roles, RoleBindings, ClusterRoles, and ClusterRoleBindings.
5. Remove per-run credential Secrets used by sandbox access.
6. Confirm workload removal and access cleanup through uncached API reads.

A failed operation must not prevent attempts on the remaining inventory.
A successful DELETE response alone does not establish removal.
A terminating object remains pending while it can still support sandbox execution or access.
Use UID preconditions where deletion could otherwise affect a replacement object.
Retain backing-Pod identity when deleting a claim could destroy its discovery path.

Preserve standing identities, source reader bindings, reusable SandboxTemplates, and unrelated objects.
Remove temporary subjects from shared bindings instead of deleting those bindings.
Do not delete input ConfigMaps or Result CRs merely because suspension occurred.
Do not treat an operator namespace as permission to delete unrelated resources.

Confirmation means control-plane resource removal and access revocation.
It does not mean node fencing or independent proof of process death on an unreachable node.
Upstream claim-controller behavior must be exercised, not inferred solely from Pod DELETE requests.

## Controller responsibilities

Keep the existing config controller responsible for global inventory and completion.
Keep the run controller responsible for affected run progress and terminal outcomes.
Reuse existing low-level deletion and access-cleanup helpers without changing ordinary cleanup semantics.
Pass only the concrete capabilities required between existing components.
Do not extract a reusable cancellation framework or design the future cancellation API now.

The config controller records stopping before its sweep.
It continues sweep and confirmation passes while the activation remains suspended.
It reports clean completion only when required resources are absent and affected run outcomes are recorded.
A resource check alone is insufficient: an affected Pending run must also receive its outcome.

The run controller persists `Stopping=True` before permitting further workflow progression.
It evaluates suspension and persisted stop state before approval, revision, and normal phase dispatch.
Sandbox creation and asynchronous result or timeout writes must respect that state too.
Version-checked status writes must reject stale writers, then reread and re-evaluate conflicts.
Late sandbox creation must trigger discovery and deletion even after a run becomes terminal.
Existing terminal-cleanup shortcuts must not suppress suspension cleanup.

## Per-run outcomes

Confirmed cleanup completes `Stopping` and writes `EmergencyStopped=True` in one status update.
Only this confirmed path clears sandbox references.

The user approved the following outcome for unsuccessful cleanup:
Complete `Stopping=False` with reason `TeardownFailed` and record a terminal `Failed` workflow outcome.
Use the phase-relevant condition with reason `EmergencyStopFailed`, not `CancelledByUser`.
Retain sandbox references whose cleanup remains unconfirmed.
Do not write a successful `EmergencyStopped` outcome for an unconfirmed sandbox.
No new run phase is required.

A run that was affected by suspension must never restart after either outcome.
Persisted suspension-failure markers must block revision and late workflow completion after administrative resumption.
The config controller must persist stop acceptance for affected runs rather than rely solely on run-controller scheduling.
An unsaved stop marker keeps `SafeToResume=False`; it does not prevent terminal global error reporting at the deadline.
Existing Result CRs remain available for audit and must not change stop outcomes.
Global error completion need not wait indefinitely for every run-status write.
Any affected run still carrying `Stopping=True` must finalize from its persisted activation deadline on its next reconciliation.
It must do so even if the administrator already resumed or deleted the config.

## Error completion and later cleanup

At the deadline, unresolved cleanup produces `AdminActivatedWithErrors`.
A known permanent error need not consume the window once all other cleanup work has settled.
The operator must still attempt the remaining cleanup before deciding that outcome.

Once recorded, error completion remains terminal for that activation.
Background cleanup while still suspended may remove remaining resources and update diagnostics.
It must not return error completion to stopping; it updates the separate resumption-safety condition instead.
No new cleanup deadline begins because a resource appears late.
Late resources remain subject to cleanup without restarting the associated run.
A late unresolved resource changes `AdminActivated` to `AdminActivatedWithErrors` once the original deadline expires.
Before that deadline, the controller may restore `Stopping` while cleaning the newly discovered resource.
Neither transition resets the activation deadline.
These transitions never extend the stopping window; unresolved execution still blocks resumption through `SafeToResume=False`.
After resumption, stop markers still prevent affected runs from restarting.
Config deletion remains blocked until current safety confirmation permits it, including after terminal error completion.

## Hub-local boundary

This change does not add spoke discovery, remote retries, node fencing, or remote verification.
Existing spoke cleanup requests remain available through current helpers.
Hub-local confirmation must not be described as proof that a spoke workload disappeared.

The user approved reporting unverified spoke cleanup as global completion with errors.
If an affected spoke run cannot supply workload/access confirmation, report it as unverified in error completion.
Do not issue a clean completion claim based solely on an empty hub-local inventory.
Do not add a separate spoke retry window or new remote cleanup subsystem.
The same ten-second activation deadline applies to the administrative stopping state.

Builder clarification: affected runs include non-terminal runs at activation and non-terminal runs discovered during that activation.
Previously terminal runs keep their historical outcomes, but known remaining managed resources still require cleanup or error reporting.
A spoke target alone does not establish that a workload or temporary access exists.
Use existing run state and cleanup evidence; do not add remote inventory discovery in this change.
Known remote workloads or access without confirmation must appear in the error summary.
Known unverified remote execution or access keeps `SafeToResume=False` without adding remote cleanup machinery.

## Resume and deletion admission

Add config UPDATE and DELETE protection through the existing static admission-manifest convention.
Reject `true` to `false` updates and deletion of a suspended config unless its current activation completed safely.
Require `Suspended=True`, reason `AdminActivated` or `AdminActivatedWithErrors`, and current `SafeToResume=True`.
Both conditions must observe the suspended object's current generation.
Reject missing, stale, false, or stopping status, including the interval before the first controller status write.
A terminal condition from an earlier activation must not authorize a new activation's resume or deletion.
Match condition observation to the suspended object's current generation.
Keep unrelated config updates and operator status updates allowed.
Keep config deletion and updates allowed when the system is not suspended.
Keep suspension-status writes restricted to the operator through existing RBAC.

Explicit administrative resumption enables new runs only.
It requires confirmed cleanup and durable stop protection for affected runs before enabling new runs.
Removing the admission binding remains an administrative break-glass action, not an automated fallback.

## Verification required during implementation

- Exercise actual zero-grace Pod DELETE requests in bare and claim modes.
- Confirm ordinary cleanup still omits the suspension-only grace override.
- Keep stopping pending when DELETE succeeds but an active resource remains.
- Confirm removal of temporary access while preserving shared identities and bindings.
- Discover resources missing status references, including resources associated with terminal or deleted runs.
- Exercise claim-backed Pod recreation and discovery after claim deletion.
- Inject permanent errors, transient errors, and a stalled API request within the total deadline.
- Demonstrate error completion remains terminal while unsafe resume and config deletion are rejected.
- Confirm both actions become allowed after background cleanup establishes current `SafeToResume=True`.
- Reject early resume, early deletion, and stale completion from a previous activation.
- Preserve the deadline across controller restarts and rapid activation sequences.
- Prevent late results, approvals, revisions, and Pod creation from restarting affected runs.
- Demonstrate an unsaved run marker causes terminal error completion with `SafeToResume=False`, rather than indefinite `Stopping`.
- Exercise admission and controller behavior together against a Kubernetes API server.
- Report live upstream claim-controller verification separately when that environment is unavailable.

Tests must assert consumer-visible behavior, not helper wiring or source wording.
Use the repository's existing test conventions and an executable runtime smoke.

## Specification alignment and exclusions

This design bounds the stopping outcome, not permission to resume with potentially active old sandboxes.
Error completion applies to unresolved cleanup, but safety confirmation remains mandatory for resume and config deletion.
No node fencing or recovery framework is introduced.
The builder must align parent and operator behavior specifications with that approved change.
Do not mark the full multicluster termination contract complete.

Update applicable system-config, run-lifecycle, sandbox-execution, CRD, and controller documentation during implementation.
Existing console model helpers derive `Failed` and display the proposed `EmergencyStopFailed` diagnostic without cancellation wording.
An executable helper probe confirmed that behavior; this is not visual or operator verification.
No console changes or console feature redesign are planned.
Per-run cancellation fields, per-run Stop triggering, shared-core extraction, and new multicluster behavior remain excluded.

## Builder discretion

Choose exact internal helper boundaries and diagnostic formatting within existing conventions.
Prefer existing fields, labels, watches, and condition helpers over new APIs.
Add only the state necessary to persist activation identity, the deadline, and irreversible run stop intent.
Choose bounded request scheduling without introducing a general task scheduler.
Keep error summaries specific enough to identify unresolved resources without unbounded status growth.
