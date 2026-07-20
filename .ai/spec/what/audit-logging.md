# Audit Logging

Implementation spec for compliance audit logging in the agentic operator. Parent spec: `ols/.ai/spec/what/audit-logging.md` (authoritative for cross-repo requirements, event semantics, correlation contract, and OTel GenAI attribute reference).

## Behavioral Rules

### Per-Phase Traces

1. The operator MUST create a separate OTel trace for each phase of an AgenticRun's lifecycle. Each phase trace gets a fresh, auto-generated OTel trace ID. There is no single root span spanning the AgenticRun lifecycle.

2. Phase trace root spans MUST use the following names, all with span kind `INTERNAL`:
   - `agenticrun.analyze` — analysis phase
   - `agenticrun.human_approval` — approval phase (short-lived: just the approval event, not the wait time)
   - `agenticrun.execute` — execution phase
   - `agenticrun.verify` — verification phase
   - `agenticrun.escalate` — escalation phase
   - `agenticrun.terminal` — terminal phase (Completed, Failed, Denied, Escalated, EmergencyStopped)

3. Every span in every phase trace MUST carry these span attributes:
   - `agenticrun.uid` — the AgenticRun CR's `metadata.uid` with hyphens stripped to produce a 32-char hex string. This is the cross-trace correlation key. `agenticrun.uid` is a span attribute, not the trace ID.
   - `agenticrun.name` — the AgenticRun CR's `metadata.name`.
   - `agenticrun.namespace` — the AgenticRun CR's `metadata.namespace`.

4. Phase root spans that call the sandbox SHOULD carry `gen_ai.request.model` and `gen_ai.provider.name` as span attributes where the operator knows the model/provider being sent to the sandbox.

### Span Links

5. Each phase trace's root span MUST include an OTel Span Link back to the prior phase's root span. This gives trace UIs a "click to see previous phase" affordance. The first phase trace (analysis) has no prior link.

6. On operator restart, the operator MUST be able to resume producing traces for an in-progress AgenticRun. Since each phase gets a fresh trace ID, the operator does not need to reconstruct a prior trace ID. It reads `agenticrun.uid` from the CR's `metadata.uid` for the correlation attribute and creates the next phase trace normally. [DEFERRED: needs Jira] Span Links to prior phases require persisting the prior phase's span context (trace ID + span ID) on the AgenticRun's status or annotations; currently the in-memory `priorPhase` map is lost on restart, breaking span link continuity but not `agenticrun.uid` correlation.

### Retries

7. On retry (verification failure leading to re-execute), the operator MUST create new traces for the retry execution and verification phases. The `retry_index` MUST be a span attribute on each retry trace's root span.

### CR Serialization as Span Events

8. The operator MUST emit the following span events attached to the corresponding phase root spans. Each span event records a CR serialization using a split model:
   - **Key fields as span event attributes** (queryable): `result.name`, `result.uid`, `options.count`, `actions_taken.count`, `checks.count`, `retry_count`, `phase`, `reason`.
   - **Full CR serialization as a span event attribute** (viewable, full fidelity): complete `.spec` + `.status` + select metadata as a single attribute value.

   | Span Event Name | Parent Span | When | Key Attributes |
   |---|---|---|---|
   | `agenticrun.received` | `agenticrun.analyze` | New AgenticRun CR detected (finalizer added) | Full AgenticRun CR serialization |
   | `agenticrun.analysis.completed` | `agenticrun.analyze` | AnalysisResult CR created | `result.name`, `result.uid`, `options.count` + full AnalysisResult CR serialization |
   | `agenticrun.approval.completed` | `agenticrun.human_approval` | AgenticRunApproval PATCH observed by webhook | `approver.uid`, `approver.username`, selected option, full text of selected option |
   | `agenticrun.execution.completed` | `agenticrun.execute` | ExecutionResult CR created | `result.name`, `result.uid`, `actions_taken.count` + full ExecutionResult CR serialization |
   | `agenticrun.verification.completed` | `agenticrun.verify` | VerificationResult CR created, checks passed | `result.name`, `result.uid`, `checks.count` + full VerificationResult CR serialization |
   | `agenticrun.verification.retry` | `agenticrun.verify` | Verification failed, retrying execution+verification | `result.name`, `retry_count`, `checks.count` + full VerificationResult CR serialization |
   | `agenticrun.escalation.completed` | `agenticrun.escalate` | EscalationResult CR created | Full EscalationResult CR serialization |
   | `agenticrun.terminal` | `agenticrun.terminal` | AgenticRun reaches terminal phase | `phase`, `reason` |

9. CR serialization MUST include `.spec` plus `metadata.name`, `metadata.namespace`, `metadata.creationTimestamp`, and `metadata.uid`. Not the full Kubernetes metadata. Result CRs (AnalysisResult, ExecutionResult, VerificationResult, EscalationResult) MUST also include `.status` since the useful data (RemediationOptions, ActionsTaken, Checks, etc.) lives in status.

### Span Kinds

10. All operator spans MUST use span kind `INTERNAL`. The operator performs Kubernetes workflow orchestration; it does not make external LLM API calls. The sandbox makes the LLM calls and creates `CLIENT` spans for those.

### Trace Propagation

11. The operator MUST propagate trace context to the sandbox via W3C `traceparent` header on all `/v1/agent/run` HTTP calls. The trace ID in the header is the auto-generated trace ID for the current phase trace (not the AgenticRun UID).

### Structured Log Format — OTel JSON via Stdout Exporter

12. The operator MUST configure two exporters on its TracerProvider:
    - **Stdout exporter** — serializes spans as OTLP JSON to stdout. Always active (audit is unconditionally enabled). This is the compliance record. The stdout exporter MUST NOT truncate span attributes or event attributes.
    - **OTLP exporter** — sends spans to the in-cluster Collector via OTLP gRPC. Configured from the `lightspeed-otel-collector-client` ConfigMap (managed by lightspeed-operator). Uses a no-op exporter when the ConfigMap is not yet available.

13. The single-emission rule MUST be followed: each audit-significant datum is recorded exactly once, as an OTel span or span event. The stdout and OTLP exporters are two destinations for the same emission, not two separate emission paths. Application-level loggers (Go `logr`) MUST emit only developer-debugging messages and MUST NOT re-emit data that appears in spans or span events.

14. The operator MUST NOT emit custom structured JSON audit events to stdout via the application logger. All audit data flows through OTel spans and span events. The stdout exporter produces the structured JSON output (OTLP JSON format) automatically.

### Reconcile Loop Emission

15. All span events listed in section 8 MUST be emitted from the reconciliation loop where the operator already has the AgenticRun object in scope. The `agenticrun.uid` is read from the AgenticRun's `metadata.uid`. (`agenticrun.approval.completed` is webhook-emitted as defined below.) Terminal phase handling (terminal span + span event) MUST run before the suspension guard so that EmergencyStopped runs receive audit cleanup even while the system is suspended.

### Human Approval Trace

16. The `agenticrun.human_approval` trace is short-lived: it records just the approval event, not the wait time. Human decision-time duration is derived from timestamps between the `agenticrun.analysis.completed` event (on the analysis trace) and the `agenticrun.approval.completed` event (on the approval trace).

### Mutating Admission Webhook

17. The operator MUST host a MutatingAdmissionWebhook for `PATCH` operations on `agenticrunapprovals.agentic.openshift.io/v1alpha1`.

18. The webhook MUST read `request.userInfo.username` and `request.userInfo.uid` from the AdmissionReview and write them into `spec.approver.uid`, `spec.approver.username`, and `spec.approver.timestamp` (server-side `time.Now()`) on the CR, overwriting any client-submitted values.

19. The webhook MUST emit the `agenticrun.approval.completed` span event with user identity attributes (`approver.uid`, `approver.username`) on the `agenticrun.human_approval` phase trace's root span. The `agenticrun.uid` is read from the CR's owner reference UID field.

20. The webhook MUST be fail-closed — if the webhook is unavailable, the API server rejects the PATCH.

21. The webhook runs in the same controller-manager process — same binary, same OTel TracerProvider, same exporters.

### CRD Changes

22. The AgenticRunApproval CRD MUST add `spec.approver` with fields:
    - `uid` (string) — from `userInfo.uid`, webhook-authoritative
    - `username` (string) — from `userInfo.username`, webhook-authoritative
    - `timestamp` (string, RFC3339) — server-side `time.Now()`, webhook-authoritative

### Configuration

23. The operator reads audit config from the `AgenticOLSConfig` CR at `spec.audit`.

24. `spec.audit.enabled` controls whether audit emission is active. Defaults to `true` — when the CR is absent or the field is not set, audit is enabled. Set to `false` to disable all audit emission (both stdout and OTLP exporters).

25. When audit is enabled, the stdout exporter always emits OTLP JSON to stdout. This is what any log aggregator (Loki, Splunk, Fluentd, etc.) reads from container logs.

26. The OTLP exporter endpoint is sourced from the `lightspeed-otel-collector-client` ConfigMap (field `collector-endpoint`). The operator blocks at startup until this ConfigMap exists (5 min timeout, fatal on expiry). Runtime changes to the ConfigMap reconfigure the exporter without restart. The OTLP exporter is additive — it provides distributed tracing and log persistence alongside the stdout compliance record.

27. The operator MUST pass the OTEL endpoint to the sandbox via environment variable or config mount so the sandbox can configure its own exporters.

### OTLP Log Emission (Templog)

28. When the OTLP log endpoint environment variable is set (wired by the lightspeed-operator when `spec.templog` is enabled), the operator MUST also emit audit span data as OTLP log records to that endpoint. This is in addition to the stdout and OTLP trace exporters.

29. Each OTLP log record MUST carry: `trace_id` in the log record's trace context (the current phase trace's auto-generated trace ID), `agenticrun.uid` as a log record attribute (for cross-trace correlation), and the span event data as the log record body.

30. OTLP logs and traces share the same Collector endpoint (from ConfigMap). Both are always active when the Collector is configured.

31. When the OTLP log endpoint is absent, no OTLP log records are emitted. No error, no warning — graceful degradation.

### Templog Finalizer

32. When a new AgenticRun CR is created and templog is enabled (read from an environment variable set by the lightspeed-operator), the operator MUST add the finalizer `agentic.openshift.io/templog-cleanup` to the AgenticRun.

33. On AgenticRun deletion, if the `agentic.openshift.io/templog-cleanup` finalizer is present, the operator MUST connect to PostgreSQL and execute `DELETE FROM templogs.logs WHERE trace_id = $1`. On success, remove the finalizer. On failure, block deletion and requeue with exponential backoff.

34. The finalizer does not depend on the Collector being present — it connects directly to PostgreSQL. See `templog.md` for edge cases.

## Cross-References

- `run-lifecycle.md` — phase transitions where span events are emitted
- `approval.md` — approval flow and AgenticRunApproval CR
- `sandbox-execution.md` — sandbox HTTP calls where trace context is propagated
- `crd-api.md` — CRD definitions (AgenticRunApproval needs `spec.approver` addition)
- `templog.md` — Temporary audit log storage: OTLP log emission, finalizer, Postgres cleanup
- `ols/.ai/spec/what/audit-logging.md` — parent spec (cross-repo requirements, event semantics, correlation contract, OTel GenAI attribute reference)
