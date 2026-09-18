# Temporary Audit Log Storage — Agentic Operator

Implementation details for the agentic-operator's role in the templog feature.

## Behavioral Rules

### Configuration

1. [PLANNED: OLS-3685] The agentic-operator reads Collector connectivity from the `lightspeed-agentic-configuration` ConfigMap in the operator namespace, created by the lightspeed-operator.
2. The ConfigMap supplies `otel-collector-endpoint`, `otel-admin-endpoint`, and `otel-ca-secret`. The named Secret contains the public Collector CA under `otel-ca.crt`; the operator uses it to trust OTLP and admin HTTPS connections.
3. The agentic-operator does not receive a templog or product-collection enablement value. Whether received logs are stored and received traces are staged depends on the Collector pipelines independently configured by the lightspeed-operator.
4. Missing or invalid Collector connectivity disables OTLP export and records a bounded configuration error; it MUST NOT prevent the controller manager from starting or block reconciliation. Stdout compliance output continues when enabled, and the configuration watcher enables OTLP after valid handoff resources appear.
5. A controller-runtime informer watches the ConfigMap and referenced CA Secret for runtime changes. On a valid change, exporters are rebuilt before previous providers are shut down. On an invalid update, export is disabled and an error is logged; reconciliation does not retain stale connectivity.

### OTLP Log Emission

6. When the Collector is configured and compliance audit is enabled, the agentic-operator emits audit events as OTLP log records to it. When audit is disabled or connectivity is unavailable, OTLP log emission is a no-op. Whether records are stored depends on the Collector's pipeline configuration managed by the lightspeed-operator; product trace export remains independent.
7. Structured compliance JSON emits to stdout only when compliance audit is enabled. When enabled with Collector connectivity, this is dual emission: stdout plus OTLP logs.
8. Each OTLP log record carries:
   - `agenticrun.uid` as a log record attribute (AgenticRun `metadata.uid`, raw UUID with hyphens — collector normalizes to 32-char hex on INSERT)
   - `agenticrun.phase` as a log record attribute (the current audit phase: `analysis`, `approval`, `execution`, `verification`, `escalation`, `terminal`)
   - `event` as a log record attribute (the event discriminator, e.g., `audit.agenticrun.received`)
   - The full structured JSON audit event as the log record body
9. When Collector connectivity is unavailable, OTLP log emission is a no-op. Compliance stdout behavior remains governed by the audit setting.

### OTLP Trace Emission

10. The same ConfigMap-configured OTLP connection is used for trace spans (per-phase root spans). Traces and logs share the same endpoint and TLS configuration.
11. The `agenticrun.uid` log attribute stores the raw Kubernetes `metadata.uid` (with hyphens). The collector's `postgresexporter` normalizes it (strips hyphens) when writing to the `agentic_run_id` column. The OTel log record's native `TraceID` field carries the per-phase trace ID and is not used for templog column mapping.
12. Trace context is propagated to sandbox pods via the W3C `TRACEPARENT` environment variable set from the operator phase span at pod creation.

### CLI Retrieval

13. `oc agentic run logs NAME` reads live sandbox pod logs by default. With `--stored`, it reads the persisted audit records from the Collector admin API after the sandbox pod has been deleted.
14. Stored-log retrieval uses the AgenticRun `metadata.uid`, requests JSON pages from the Collector, and renders the result in the Collector's text format. An explicit `--step` sends `phase=analysis`, `phase=execution`, or `phase=verification`; without `--step`, the phase parameter is omitted. By default, the command uses the Kubernetes API server's Service proxy to reach the `lightspeed-otel-collector` Service over HTTPS in the AgenticRun/operator namespace; this requires `get` on `services/proxy` there. `--admin-endpoint` MAY override the Service proxy for an externally reachable endpoint. The inherited `--insecure-skip-tls-verify` flag MAY be used only with an explicit endpoint when certificate verification is intentionally bypassed.
15. When `--step` is omitted with `--stored`, the command retrieves all persisted phases in Collector record order. An explicit `--step` filters to analysis, execution, or verification. `--follow` is valid only for live pod logs and MUST be rejected with `--stored`.
16. Stored-log retrieval requests the maximum page size and follows the Collector's `after` cursor until `has_more=false`, so the command returns all records. The command MUST reject a non-advancing cursor to avoid an infinite loop. Rendered stored logs MUST include a `===== <phase> =====` header for each phase; the header is also shown for an explicit single-phase filter.

### AgenticRun Finalizer

17. The `agentic.openshift.io/templog-cleanup` (and RBAC cleanup) finalizers are added the first time the controller reconciles any non-deleting AgenticRun — including already-terminal runs — so TTL or manual delete always runs Collector log cleanup. Both finalizers are processed in a **single reconcile pass** on deletion: RBAC cleanup first (via `ReleaseSandboxes`), then templog cleanup.
18. When an AgenticRun CR is deleted and the finalizer is present:
    a. The operator calls the Collector admin API: `DELETE /api/v1/logs?agentic_run_id=<uid>` over HTTPS using the CA cert from the ConfigMap. The raw Kubernetes UID (with hyphens) is passed; the collector normalizes internally.
    b. On success, removes the finalizer — CR deletion proceeds.
    c. On failure, increments a retry counter annotation (`agentic.openshift.io/templog-cleanup-attempts`) and requeues after 30 seconds.
    d. After 3 failed attempts, removes the finalizer regardless — CR deletion proceeds. A warning is logged about orphaned log records.
19. The finalizer depends on the Collector admin API being reachable. If the Collector is permanently down, the finalizer gives up after 3 retries to avoid blocking CR deletion indefinitely.

## Edge Cases

- **Invalid connectivity at startup or runtime.** OTLP export is disabled, stale configuration is not retained, and a bounded error is logged. The operator continues reconciling and the watcher enables export after valid handoff resources appear.
- **Collector unavailable during AgenticRun deletion.** The finalizer retries up to 3 times. After exhausting retries, the finalizer is removed and deletion proceeds. Log records become orphaned in Postgres — acceptable trade-off vs blocking deletion forever.
- **No rows to delete.** The Collector admin API returns HTTP 200 with `{"deleted": 0}`. The finalizer succeeds and is removed. No error.
- **ConfigMap deleted at runtime.** OTLP emission becomes a no-op. Compliance stdout remains governed by the audit setting. A cleanup attempt without an admin client is a normal failed attempt under rule 14; it does not bypass retry accounting or remove the finalizer early.
- **Operator restart mid-cleanup.** The retry counter is stored as an annotation on the CR — survives restart. The operator resumes from the stored attempt count.

## Cross-References

- `what/audit-logging.md` — Audit event catalog, structured JSON format, OTEL span hierarchy
- `what/run-lifecycle.md` — AgenticRun CR lifecycle, phase transitions, finalizers
- `pkg/telemetry/` — Provider implementation (ConfigMap reader, OTLP exporters, admin HTTP client)
- `pkg/configwatch/` — Generic informer-based ConfigMap watcher
