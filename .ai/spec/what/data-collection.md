# Agentic Data Collection

Operator-owned producer requirements for OLS-3569 Agentic data collection. The authoritative end-to-end architecture, collection controls, correlation and event semantics, and downstream ownership are defined by `ols/.ai/spec/what/agentic-data-collection.md`; the governing decision is `ols/.ai/spec/decisions/0042-agentic-data-collection-via-otel.md`.

## Behavioral Rules

### Producer Scope

1. [PLANNED: OLS-3569] The operator MUST contribute Agentic product data through its existing OTLP trace spans and span events. It MUST NOT add a parallel application emission path or write product-data files.
2. [PLANNED: OLS-3569] The operator MUST NOT receive or evaluate collection state. OLS-3569 adds no collection field to `AgenticOLSConfig`.
3. [PLANNED: OLS-3569] The operator MUST use the existing shared OTLP connection published in `lightspeed-agentic-configuration` under `otel-collector-endpoint` and `otel-ca-secret`. It MUST NOT introduce a product-specific endpoint, environment variable, or handoff ConfigMap key. `spec.audit.enabled` remains a compliance-audit control and MUST NOT suppress spans sent through the shared Collector connection.

### Correlation and Propagation

4. [PLANNED: OLS-3569] Every eligible operator span MUST carry `agenticrun.uid` and `agenticrun.phase` as span attributes; a resource-attribute fallback is not permitted. `agenticrun.uid` MUST be the literal `AgenticRun.metadata.uid`. `agenticrun.phase` MUST be `analysis`, `approval`, `execution`, `verification`, `escalation`, or `terminal`. Span events inherit both values from their containing span.
5. [PLANNED: OLS-3569] Each phase MAY use an independent OTel trace ID. The operator MUST use `agenticrun.uid`, not a synthesized trace ID, to correlate phases as required by the parent contract.
6. [PLANNED: OLS-3569] For each analysis, execution, verification, or escalation batch sandbox, the operator MUST inject the active phase context and correlation into the generated Pod or SandboxTemplate as `TRACEPARENT`, `LIGHTSPEED_AGENTICRUN_UID`, and `LIGHTSPEED_AGENTICRUN_STEP`. It MUST inject the shared `otel-collector-endpoint` as `OTEL_EXPORTER_OTLP_ENDPOINT` and mount the Secret named by `otel-ca-secret` under the shared sandbox TLS root for inclusion in the sandbox-generated CA bundle. These values travel with the Pod created after the per-step input ConfigMap; there is no sandbox HTTP propagation path.
7. [PLANNED: OLS-3569] Reconciliation resumed after an operator restart MUST establish the current phase span before creating a replacement batch Pod so the injected `TRACEPARENT` is valid. Telemetry setup or export failure MUST NOT block ConfigMap creation, Pod execution, Result CR observation, or run progression.

### Lifecycle Instrumentation

8. [PLANNED: OLS-3569] The operator MUST create phase root spans at the local operation boundaries defined in `audit-logging.md`: analysis, execution, verification, and escalation span their respective workflow operations; human approval and terminal spans are short-lived observations rather than wait intervals.
9. [PLANNED: OLS-3569] A phase span MUST retain its native OTel start time, end time, and status. The operator MUST NOT replace them with a precomputed business duration or aggregate multiple attempts.
10. [PLANNED: OLS-3569] The run reconciler MUST emit the parent-defined lifecycle events when it receives an `AgenticRun`, observes a completed Result CR, or observes a terminal state. The approval webhook MUST emit the parent-defined approval event with the authenticated identity and applied selection. The operator MUST use the parent event catalog and MUST NOT define a second schema locally.
11. [PLANNED: OLS-3569] Terminal instrumentation MUST cover `Completed`, `Failed`, `Denied`, `Escalated`, and `EmergencyStopped`, including `Completed` with condition reason `NoActionRequired`. Cancellation remains `Failed` with reason `CancelledByUser`.
12. [PLANNED: OLS-3569] Result and terminal events MUST carry the source condition, status, reason, failure, action or check outcome, and timestamp evidence required by the parent event contract. The operator emits source evidence only; downstream interpretation is outside this specification.

## Cross-References

- `ols/.ai/spec/what/agentic-data-collection.md` — authoritative architecture, collection controls, event and correlation semantics, and repository ownership
- `ols/.ai/spec/decisions/0042-agentic-data-collection-via-otel.md` — governing architecture decision
- `lightspeed-operator/.ai/spec/what/agentic-sandbox-profile.md` — `lightspeed-agentic-configuration` handoff
- `audit-logging.md` — operator phase spans, reconcile and webhook emission points, and exporter fan-out
- `run-lifecycle.md` — phase, condition, Result CR, terminal, and `NoActionRequired` semantics
- `sandbox-execution.md` — input ConfigMap, batch Pod or SandboxClaim, and Result CR flow
