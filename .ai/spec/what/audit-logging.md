# Audit Logging

Implementation spec for compliance audit logging and lifecycle trace production in the agentic operator. Parent spec: `ols/.ai/spec/what/audit-logging.md` (authoritative for cross-repo event semantics, correlation, and OTel GenAI attributes). Product collection semantics are authoritative in `ols/.ai/spec/what/agentic-data-collection.md`.

## Behavioral Rules

### Per-Phase Traces

1. The operator MUST create a separate OTel trace for each phase of an AgenticRun's lifecycle. Each phase trace gets a fresh, auto-generated OTel trace ID. There is no single root span spanning the AgenticRun lifecycle.

2. Phase trace root spans MUST use the following names, all with span kind `INTERNAL`:
   - `agenticrun.analyze` — analysis phase
   - `agenticrun.human_approval` — approval observation, not the human wait interval
   - `agenticrun.execute` — execution phase
   - `agenticrun.verify` — verification phase
   - `agenticrun.escalate` — escalation phase
   - `agenticrun.terminal` — terminal observation for `Completed`, `Failed`, `Denied`, `Escalated`, or `EmergencyStopped`; [PLANNED: OLS-3569] coverage MUST include `Completed` with reason `NoActionRequired`

3. Every span in every phase trace MUST carry `agenticrun.uid`, `agenticrun.phase`, `agenticrun.name`, and `agenticrun.namespace` as span attributes. `agenticrun.uid` and `agenticrun.phase` MUST satisfy the canonical values and span-level placement defined by the parent data-collection contract; resource attributes are not a fallback.

4. Phase root spans that create a batch sandbox SHOULD carry `gen_ai.request.model` and `gen_ai.provider.name` where the operator knows the model and provider.

### Span Links and Restart

5. Each phase trace's root span MUST include an OTel Span Link to the prior phase's root span when that context is available. The analysis phase has no prior link.

6. On operator restart, the operator MUST resume trace production for an in-progress AgenticRun using the CR's literal `metadata.uid`. A new phase gets a fresh trace ID. [DEFERRED: needs Jira] Persisting the prior phase span context for link continuity remains deferred; loss of that link MUST NOT change UID correlation.

### Lifecycle Event Production

7. [PLANNED: OLS-3569] The operator MUST emit the parent-defined lifecycle event catalog from its local producer hooks: run receipt and completed Result CRs from reconciliation, authenticated approval or denial from the webhook, and terminal state from terminal reconciliation. The parent specs own event names and required fields; this file owns the producer locations and timing.

8. All operator spans MUST use span kind `INTERNAL`. The operator orchestrates Kubernetes workflow resources; the batch sandbox creates the LLM `CLIENT` spans.

### Batch Sandbox Trace Propagation

9. For analysis, execution, verification, and escalation, the operator MUST create the input ConfigMap and then inject telemetry into the generated Pod or SandboxTemplate. When the shared OTLP connection is configured, the container MUST receive:
   - `OTEL_EXPORTER_OTLP_ENDPOINT` from `lightspeed-agentic-configuration.data.otel-collector-endpoint`
   - `OTEL_EXPORTER_OTLP_CERTIFICATE` set to `/etc/certs/otel-collector-ca/service-ca.crt` from the mounted Secret named by `otel-ca-secret`
   - `LIGHTSPEED_AGENTICRUN_UID` with the literal AgenticRun UID
   - `LIGHTSPEED_AGENTICRUN_STEP` with the current phase
   - `TRACEPARENT` from the active phase span

10. The operator MUST NOT construct an HTTP sandbox request or use HTTP for trace-context propagation. If tracing is active after a resumed reconciliation, the operator MUST establish the current phase span before Pod creation. If no valid span context exists because tracing is inactive, it MUST omit `TRACEPARENT`; telemetry failure MUST NOT block the batch ConfigMap/Pod/Result CR workflow.

### Exporter Fan-Out

11. [PLANNED: OLS-3569] Product data collection MUST reuse the operator spans and span events above rather than add another application emission. Shared event, correlation, fidelity, and collection-boundary semantics are defined only by the parent data-collection contract and the local producer additions in `data-collection.md`.

12. The operator MUST fan each single span/event emission to the independently configured destinations:
    - **Compliance stdout exporter** — serializes spans as OTLP JSON while compliance audit is enabled.
    - **Configured compliance OTLP exporter** — sends traces to `spec.audit.otel.endpoint` while compliance audit is enabled and an endpoint exists.
    - **Shared Collector OTLP exporter** — sends traces through the existing handoff connection, independent of `spec.audit.enabled`.

13. Exporter fan-out creates destination copies of one emission, not separate application emissions. Each datum MUST be recorded exactly once as an OTel span or span event.

14. Application loggers MUST emit only developer-debugging messages and MUST NOT duplicate span or span-event data. The operator MUST NOT emit a second custom JSON audit event through the application logger.

### Reconcile Loop Emission

15. Reconcile-produced lifecycle events MUST be emitted where the operator has the AgenticRun or completed Result CR in scope. Terminal span/event handling MUST run before the suspension guard so `EmergencyStopped` runs receive terminal telemetry while the system is suspended.

16. The `agenticrun.human_approval` trace is short-lived and records only the observed decision. The operator MUST NOT keep a span open during the human wait.

### Mutating Admission Webhook

17. The operator MUST host a MutatingAdmissionWebhook for `PATCH` operations on `agenticrunapprovals.agentic.openshift.io/v1alpha1`.

18. The webhook MUST read `request.userInfo.username` and `request.userInfo.uid` from the AdmissionReview and write them into `spec.approver.uid`, `spec.approver.username`, and `spec.approver.timestamp` using server time, overwriting client-submitted values.

19. The webhook MUST emit the parent-defined approval event on the `agenticrun.human_approval` root span with authenticated identity and the decision and selection actually applied. It MUST read `agenticrun.uid` from the AgenticRun owner reference.

20. The webhook MUST fail closed: if it is unavailable, the API server rejects the PATCH.

21. The webhook MUST run in the controller-manager process and use the same OTel TracerProvider and exporters as reconciliation.

### CRD and Configuration

22. The AgenticRunApproval CRD MUST define `spec.approver.uid`, `spec.approver.username`, and RFC3339 `spec.approver.timestamp` as webhook-authoritative strings.

23. [PLANNED -- spec.audit field not yet in AgenticOLSConfig CRD; see crd-api.md] The operator reads compliance audit configuration from `AgenticOLSConfig.spec.audit`. `spec.audit.enabled` defaults to `true` and gates the compliance stdout and configured compliance OTLP destinations. [PLANNED: OLS-3569] It MUST NOT gate instrumentation or trace export through the shared Collector connection. Templog behavior remains defined only in `templog.md`.

24. When compliance audit is enabled, the stdout exporter emits OTLP JSON; when disabled, it emits no compliance output. The configured compliance OTLP endpoint is `AgenticOLSConfig.spec.audit.otel.endpoint`.

25. [PLANNED: OLS-3569] The shared Collector connection MUST be read from `lightspeed-agentic-configuration` keys `otel-collector-endpoint` and `otel-ca-secret`. Runtime ConfigMap or CA rotation MUST rebuild that connection using the existing configuration-watch mechanics. The operator MUST NOT read a collection gate or create a second product endpoint/key.

## Cross-References

- `run-lifecycle.md` — phase transitions and terminal outcomes where producer hooks run
- `approval.md` — approval flow and AgenticRunApproval CR
- `sandbox-execution.md` — input ConfigMap, batch Pod or SandboxClaim, and Result CR flow
- `crd-api.md` — AgenticRunApproval and `spec.approver`
- `templog.md` — canonical local OTLP log-bridge and finalizer behavior
- `data-collection.md` — OLS-3569 operator producer requirements
- `ols/.ai/spec/what/audit-logging.md` — authoritative shared audit event and correlation semantics
- `ols/.ai/spec/what/agentic-data-collection.md` — authoritative product collection contract
