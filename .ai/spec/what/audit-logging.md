# Audit Logging

Implementation spec for compliance audit logging in the agentic operator. Parent spec: `ols/.ai/spec/what/audit-logging.md` (authoritative for cross-repo requirements, event semantics, and correlation contract).

## Behavioral Rules

### Operator Audit Events

1. The operator MUST emit the following structured JSON audit events to stdout at each phase transition during AgenticRun reconciliation. Each event carries `trace_id` (AgenticRun's `metadata.uid` with hyphens stripped to 32 hex chars).

| Event | When | Payload |
|---|---|---|
| `audit.agenticrun.received` | New AgenticRun CR detected (finalizer added) | AgenticRun `.spec` + select metadata |
| `audit.analysis.completed` | AnalysisResult CR created | AnalysisResult serialization (all RemediationOptions) |
| `audit.approval.received` | AgenticRunApproval PATCH observed by webhook | Approver `uid`/`username` (webhook-injected), selected option, full text of selected option |
| `audit.execution.completed` | ExecutionResult CR created | ExecutionResult serialization (all ActionsTaken) |
| `audit.verification.completed` | VerificationResult CR created, checks passed | VerificationResult serialization |
| `audit.verification.retry` | Verification failed, retrying execution+verification | VerificationResult serialization, retry count |
| `audit.escalation.completed` | EscalationResult CR created | EscalationResult serialization |
| `audit.agenticrun.terminal` | AgenticRun reaches terminal phase (Completed, Failed, Denied, Escalated, EmergencyStopped) | Final phase, terminal reason |

2. CR serialization MUST include `.spec` plus `metadata.name`, `metadata.namespace`, `metadata.creationTimestamp`, and `metadata.uid`. Not the full Kubernetes metadata. Result CRs (AnalysisResult, ExecutionResult, VerificationResult, EscalationResult) MUST also include `.status` since the useful data (RemediationOptions, ActionsTaken, Checks, etc.) lives in status.

3. All reconcile-emitted audit events MUST be emitted from the reconciliation loop where the operator already has the AgenticRun object in scope. The `trace_id` is read from the AgenticRun's `metadata.uid`. (`audit.approval.received` is webhook-emitted as defined below.) Terminal phase handling (§1 terminal event + §4 lifecycle span cleanup) MUST run before the suspension guard so that EmergencyStopped runs receive audit cleanup even while the system is suspended.

### OTEL Spans

4. The operator MUST create a root span `agenticrun.lifecycle` when it first detects a new AgenticRun CR. The OTEL trace ID MUST be the AgenticRun's `metadata.uid` with hyphens stripped.

5. On operator restart, the operator MUST read the AgenticRun's `metadata.uid` from the CR and resume the trace by constructing a SpanContext with the same trace ID.

6. Child spans MUST be created for each phase: `agenticrun.analyze`, `agenticrun.human_approval`, `agenticrun.execute`, `agenticrun.verify`, `agenticrun.escalate`, `agenticrun.terminal`.

7. `agenticrun.human_approval` starts when the operator begins waiting for approval and ends when the AgenticRunApproval PATCH is observed. Duration = human decision time.

8. On retry (verification failure → re-execute), new `agenticrun.execute` and `agenticrun.verify` child spans are created under the same root. The retry index MUST be a span attribute.

### Trace Propagation

9. The operator MUST propagate trace context to the sandbox via W3C `traceparent` header on all `/v1/agent/run` HTTP calls. The trace ID in the header is the AgenticRun's `metadata.uid` (hyphens stripped).

### Mutating Admission Webhook

10. The operator MUST host a MutatingAdmissionWebhook for `PATCH` operations on `agenticrunapprovals.agentic.openshift.io/v1alpha1`.

11. The webhook MUST read `request.userInfo.username` and `request.userInfo.uid` from the AdmissionReview and write them into `spec.approver.uid`, `spec.approver.username`, and `spec.approver.timestamp` (server-side `time.Now()`) on the CR, overwriting any client-submitted values.

12. The webhook MUST emit the `audit.approval.received` log event with user identity and `trace_id` (AgenticRun's `metadata.uid`, read from the CR's owner reference UID field).

13. The webhook MUST be fail-closed — if the webhook is unavailable, the API server rejects the PATCH.

14. The webhook runs in the same controller-manager process — same binary, same logger, same OTEL tracer.

### CRD Changes

15. The AgenticRunApproval CRD MUST add `spec.approver` with fields:
    - `uid` (string) — from `userInfo.uid`, webhook-authoritative
    - `username` (string) — from `userInfo.username`, webhook-authoritative
    - `timestamp` (string, RFC3339) — server-side `time.Now()`, webhook-authoritative

### Configuration

16. The operator reads audit config from the `AgenticOLSConfig` CR at `spec.audit`. Logging and tracing are independent controls.

17. `spec.audit.logging` controls structured JSON audit events to stdout. Defaults to `true` — when the CR is absent or the field is not set, audit logging is enabled. Set to `false` to disable structured audit log output.

18. `spec.audit.otel.endpoint` controls OTEL trace export. When set, the operator configures an OTLP exporter pointed at that endpoint. When empty or absent, a no-op exporter is used. Independent of the `logging` flag — tracing works regardless of whether logging is on or off.

19. The operator MUST pass the OTEL tracing endpoint to the sandbox via environment variable or config mount so the sandbox can configure its own exporter.

### OTLP Log Emission (Templog)

20. When the OTLP log endpoint environment variable is set (wired by the lightspeed-operator when `spec.templog` is enabled), the operator MUST also emit all audit events as OTLP log records to that endpoint. This is in addition to stdout — dual emission.

21. Each OTLP log record MUST carry: `trace_id` in the log record's trace context (AgenticRun `metadata.uid`, hyphens stripped), `event` as a log record attribute, and the full structured JSON audit event as the log record body.

22. The OTLP log endpoint is independent of `spec.audit.otel.endpoint` (tracing). Both can be active simultaneously.

23. When the OTLP log endpoint is absent, no OTLP log records are emitted. No error, no warning — graceful degradation.

### Templog Finalizer

24. When a new AgenticRun CR is created and templog is enabled (read from an environment variable set by the lightspeed-operator), the operator MUST add the finalizer `agentic.openshift.io/templog-cleanup` to the AgenticRun.

25. On AgenticRun deletion, if the `agentic.openshift.io/templog-cleanup` finalizer is present, the operator MUST connect to PostgreSQL and execute `DELETE FROM templogs.logs WHERE trace_id = $1`. On success, remove the finalizer. On failure, block deletion and requeue with exponential backoff.

26. The finalizer does not depend on the Collector being present — it connects directly to PostgreSQL. See `templog.md` for edge cases.

### Structured JSON Format

20. All audit events MUST be single JSON lines to stdout with at minimum: `timestamp`, `level`, `event`, `trace_id`. The existing logr+zap setup already produces structured JSON — audit events use the same logger.

## Cross-References

- `run-lifecycle.md` — phase transitions where audit events are emitted
- `approval.md` — approval flow and AgenticRunApproval CR
- `sandbox-execution.md` — sandbox HTTP calls where trace context is propagated
- `crd-api.md` — CRD definitions (AgenticRunApproval needs `spec.approver` addition)
- `templog.md` — Temporary audit log storage: OTLP log emission, finalizer, Postgres cleanup

## Planned Changes

- [PLANNED: OLS-3295] Rename audit event prefixes from `audit.proposal.*` to `audit.agenticrun.*` and OTEL span prefixes from `proposal.*` to `agenticrun.*`.
