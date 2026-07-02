# Temporary Audit Log Storage — Agentic Operator

Implementation details for the agentic-operator's role in the templog feature. See parent spec `what/templog.md` for requirements and architecture.

## Behavioral Rules

### OTLP Log Emission

1. When the OTLP log endpoint environment variable is set (wired by the lightspeed-operator), the agentic-operator emits audit events as OTLP log records to that endpoint.
2. Structured JSON to stdout always emits when audit is enabled, regardless of whether the OTLP log endpoint is set. This is dual emission — stdout is never replaced.
3. The OTLP log endpoint is independent of `spec.audit.otel.endpoint` (tracing). The operator can emit to both simultaneously: OTLP logs to the Collector, OTLP spans to the tracing endpoint.
4. Each OTLP log record carries:
   - `trace_id` in the log record's trace context (AgenticRun `metadata.uid`, hyphens stripped, 32-char hex)
   - `event` as a log record attribute (the event discriminator, e.g., `audit.agenticrun.received`)
   - The full structured JSON audit event as the log record body
5. When the OTLP log endpoint is absent, no OTLP log records are emitted. No error, no warning — same graceful degradation as the tracing no-op exporter.

### AgenticRun Finalizer

6. When a new AgenticRun CR is created and templog is enabled (agentic-operator reads this from an environment variable set by the lightspeed-operator), the operator adds the finalizer `agentic.openshift.io/templog-cleanup` to the AgenticRun.
7. When a AgenticRun CR is deleted and the `agentic.openshift.io/templog-cleanup` finalizer is present:
   a. The operator connects to PostgreSQL using the credentials from the shared secret.
   b. Executes `DELETE FROM templogs.logs WHERE trace_id = $1` where `$1` is the AgenticRun `metadata.uid` with hyphens stripped.
   c. On success, removes the finalizer — CR deletion proceeds.
   d. On failure (Postgres unreachable, query error), the finalizer blocks deletion. The reconciler requeues with standard controller-runtime exponential backoff.
8. The finalizer does not depend on the Collector being present. It connects directly to PostgreSQL. This handles the case where `spec.templog` was disabled after logs were written — the finalizer still fires and cleans up.

### Postgres Connectivity

9. The agentic-operator reads PostgreSQL connection details from the same credentials secret managed by the lightspeed-operator.
10. The connection to PostgreSQL uses TLS via service-ca certificates (same as all other Postgres clients in the system).
11. The Postgres connection is used only for the finalizer cleanup query. The agentic-operator does not write to PostgreSQL — that is the Collector's responsibility.

## Edge Cases

- **Templog disabled after AgenticRun creation.** The finalizer was already added. On deletion, the finalizer fires and deletes rows from Postgres. The Collector being absent does not affect this — the operator connects directly to Postgres.
- **Postgres unavailable during AgenticRun deletion.** The finalizer blocks. The AgenticRun CR cannot be deleted until cleanup succeeds. This is correct for a compliance-adjacent feature.
- **No rows to delete.** The `DELETE` query returns 0 rows affected. The finalizer succeeds and is removed. No error.
- **Templog enabled mid-lifecycle.** Runs created before templog was enabled do not have the finalizer. Their audit events were not stored in Postgres (the Collector was not deployed). No cleanup needed.

## Cross-References

- Parent spec: `what/templog.md`
- `what/audit-logging.md` — Audit event catalog, structured JSON format, OTEL span hierarchy
- `what/run-lifecycle.md` — AgenticRun CR lifecycle, phase transitions, finalizers
