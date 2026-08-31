# Spec health report

Last evaluated: 2026-08-31
Trigger: post-milestone (console removal OLS-3350, TokenUsage OLS-3993) — staleness + accuracy check
Layout: software (.ai/spec/)

## Stale

1. **Console plugin removal shipped, still marked `[PLANNED: OLS-3236]`.** `controller/console/` is deleted (commit f88e0e1 "OLS-3350: Remove console plugin deployment from agentic-operator"). `cmd/main.go` no longer parses `--agentic-console-image`, registers no `EnsureAgenticConsole` RunnableFunc, and its scheme registers only `clientgoscheme` + `agenticv1alpha1` (no `consolev1` / `openshiftv1`). Affected:
   - `what/system-overview.md` rules 5a, 6, 12 (marked `[PLANNED: OLS-3236]` — now DONE), Constraints line "requires OpenShift APIs for console plugin deployment" (no longer true), Planned Changes OLS-3236 row.
   - `how/project-structure.md` module map row `controller/console/`; entry-point bullets listing `--agentic-console-image`, "Registers console plugin", "Registers `EnsureAgenticConsole` as a `RunnableFunc`".
   - `how/reconciler.md` entire "Module map: `controller/console/`" section; scheme note claiming `consolev1` + `openshiftv1` registration.

2. **`controller/sandbox/` deleted, still in module map.** `how/project-structure.md` lists `controller/sandbox/` ("Legacy bootstrap helpers"); the directory no longer exists (SA bootstrap is inline in `cmd/main.go`).

3. **TokenUsage shipped, still marked `[PLANNED: OLS-3661]`.** `TokenUsage` (both `inputTokens`/`outputTokens` `+required`, `MinProperties=1`) exists in `api/v1alpha1` on all four Result types and `AgenticRunStatus`; aggregation is implemented in `controller/agenticrun/results.go` (shipped as OLS-3993, building on the OLS-3661 spec). All `[PLANNED: OLS-3661]` markers are stale:
   - `what/crd-api.md` rules 6c, 6d, 6e, 31–34, config-surface lines, Planned Changes OLS-3661 row.
   - `what/sandbox-execution.md` rule 43.1 and Planned Changes OLS-3661 row.

4. **`how/reconciler.md` internal contradiction — `SandboxAgentCaller` fields.** DI wiring line lists `{Sandbox, K8sClient, ClientFactory, Namespace, Audit}`, but the actual struct is `{Sandbox, K8sClient, Namespace, Audit}` and the same doc later states `ClientFactory` was removed under OLS-3066. `ClientFactory` no longer exists anywhere in the code.

5. **Moved function — `buildInputConfigMap`.** `how/reconciler.md` attributes `buildInputConfigMap` to `sandbox_manager.go`; it now lives in `controller/agenticrun/input_configmap.go`.

6. **Stale cross-reference — `pkg/telemetry/`.** `what/templog.md` cross-references `pkg/telemetry/` for the provider implementation; telemetry/OTLP code lives in `pkg/configuration/otel_provider.go` and audit code in `controller/agenticrun/audit.go`. No `pkg/telemetry/` package exists.

## Missing

1. **`how/reconciler.md` module map omits shipped files.** `controller/agenticrun/audit.go` (AuditLogger, ProductionAuditLogger, LogEmitter), `approval_webhook.go` (AgenticRunApprovalMutator — the audit MutatingAdmissionWebhook of `audit-logging.md` rules 17–21), and `input_configmap.go` (buildInputConfigMap, buildResultTemplate) have no module-map entries, despite their behavior being specified in `what/audit-logging.md` and `what/sandbox-execution.md`.

2. **`how/project-structure.md` omits `controller/agenticolsconfig/`** (the AgenticOLSConfig status reconciler), which is documented in `how/reconciler.md`. Also omits `cli/system/` and `cmd/check-isa-level/`. (Minor; agenticolsconfig added for parity with reconciler.md.)

## Structural concerns

None new. `how/reconciler.md` remains large but cohesive now that the console section is removed.

## Findability issues

The audit-logging / webhook implementation files were not discoverable from the how/ module maps (see Missing #1). Addressed by adding entries.

## Accurate (verified current — no change)

- `[PLANNED: OLS-3743]` layered-timeout markers: code still has `Agent.spec.timeouts.chatSeconds`, no `escalationSeconds`, and no `LIGHTSPEED_AGENT_TIMEOUT_SECONDS`/`LIGHTSPEED_AGENT_MAX_TURNS` env wiring — still planned. Left unchanged.
- `[PLANNED: OLS-3491]` per-step `instructions`: no `Instructions` field on `AgenticRunStep` in `api/v1alpha1` — still planned. Left unchanged.
- `[PLANNED -- spec.audit]` in `what/audit-logging.md` rules 23–24: no `Audit` field on `AgenticOLSConfig` — still planned. Left unchanged.
- `[PLANNED: OLS-3594]` disableDefaultMCP / MCP auto-injection: not in API — still planned.
- Kill switch (`AgenticOLSConfig`, VAP), templog finalizer, per-step SAs, reader multi-binding, execution outcome override: all match code.
</content>
