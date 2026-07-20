# Temporary Audit Log Storage — Agentic Operator

Implementation details for the agentic-operator's role in the templog feature.

## Behavioral Rules

### Configuration

1. The agentic-operator reads Collector connectivity from a well-known ConfigMap (`lightspeed-otel-collector-client`) in the operator namespace, created by the lightspeed-operator.
2. The ConfigMap contains: `collector-endpoint` (OTLP gRPC host:port), `admin-endpoint` (HTTPS admin API), `ca.crt` (TLS CA certificate), `credentials-secret` (optional).
3. Two valid ConfigMap states exist:
    - **Disabled:** empty data / no `collector-endpoint` — OTLP export is off (stdout audit continues). Used by quickstart/e2e without a Collector.
    - **Enabled:** `collector-endpoint` (host:port), `admin-endpoint` (`https://…`), and parseable `ca.crt` PEM are all required. When `credentials-secret` is set, the named Secret in the operator namespace must contain `tls.crt` and `tls.key` (standard TLS Secret keys); those PEMs are loaded into the OTLP and admin HTTPS clients for mTLS. When the key is omitted, trust is CA-only (current lightspeed-operator default).
4. On startup, the operator blocks until the ConfigMap exists (5 minute timeout). Missing ConfigMap or an **invalid enabled** ConfigMap is fatal. An empty/disabled ConfigMap is success.
5. A controller-runtime informer watches the ConfigMap for runtime changes. On a valid change, exporters are rebuilt then the previous providers are shut down. On an **invalid enabled** update, export is disabled (old config is not retained) and an error is logged; reconcile does not retry.

### OTLP Log Emission

6. When the Collector is configured, the agentic-operator emits audit events as OTLP log records to it. When disabled or unconfigured, OTLP emission is a no-op. Whether records are stored depends on the Collector's pipeline configuration (managed by the lightspeed-operator).
7. Structured JSON to stdout always emits unconditionally. Stdout emission is not configurable — it is always on. This is dual emission: stdout + OTLP (when enabled).
8. Each OTLP log record carries:
   - `trace_id` in the log record's trace context (AgenticRun `metadata.uid`, hyphens stripped, 32-char hex)
   - `event` as a log record attribute (the event discriminator, e.g., `audit.agenticrun.received`)
   - The full structured JSON audit event as the log record body
9. When the Collector is not configured (ConfigMap absent at runtime after initial startup), OTLP log emission is a no-op. Stdout continues unaffected.

### OTLP Trace Emission

10. The same ConfigMap-configured OTLP connection is used for trace spans (agenticrun.lifecycle root, phase children). Traces and logs share the same endpoint and TLS configuration.
11. The AgenticRun UID (hyphens stripped) is used as the deterministic trace ID for all spans and log records belonging to a run.
12. Trace context is propagated to sandbox pods via the W3C `traceparent` header on agent HTTP calls.

### AgenticRun Finalizer

13. The `agentic.openshift.io/templog-cleanup` (and RBAC cleanup) finalizers are added the first time the controller reconciles any non-deleting AgenticRun — including already-terminal runs — so TTL or manual delete always runs Collector log cleanup.
14. When an AgenticRun CR is deleted and the finalizer is present:
    a. The operator calls the Collector admin API: `DELETE /api/v1/logs?trace_id=<uid-without-hyphens>` over HTTPS using the CA cert from the ConfigMap.
    b. On success, removes the finalizer — CR deletion proceeds.
    c. On failure, increments a retry counter annotation (`agentic.openshift.io/templog-cleanup-attempts`) and requeues after 30 seconds.
    d. After 3 failed attempts, removes the finalizer regardless — CR deletion proceeds. A warning is logged about orphaned log records.
15. The finalizer depends on the Collector admin API being reachable. If the Collector is permanently down, the finalizer gives up after 3 retries to avoid blocking CR deletion indefinitely.

## Edge Cases

- **Invalid enabled ConfigMap at startup.** Fatal — operator exits.
- **Invalid enabled ConfigMap at runtime.** Export disabled; old config discarded; error logged; no reconcile retry.
- **Collector unavailable during AgenticRun deletion.** The finalizer retries up to 3 times. After exhausting retries, the finalizer is removed and deletion proceeds. Log records become orphaned in Postgres — acceptable trade-off vs blocking deletion forever.
- **No rows to delete.** The Collector admin API returns HTTP 200 with `{"deleted": 0}`. The finalizer succeeds and is removed. No error.
- **ConfigMap deleted at runtime.** OTLP emission becomes no-op. Stdout continues. Admin API calls (finalizer) return nil immediately (no client configured). Finalizer is removed without cleanup.
- **Operator restart mid-cleanup.** The retry counter is stored as an annotation on the CR — survives restart. The operator resumes from the stored attempt count.

## Cross-References

- `what/audit-logging.md` — Audit event catalog, structured JSON format, OTEL span hierarchy
- `what/run-lifecycle.md` — AgenticRun CR lifecycle, phase transitions, finalizers
- `pkg/telemetry/` — Provider implementation (ConfigMap reader, OTLP exporters, admin HTTP client)
- `pkg/configwatch/` — Generic informer-based ConfigMap watcher
