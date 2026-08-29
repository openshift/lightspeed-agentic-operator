# LightSpeed Audit Logging and Tracing

This document outlines Lightspeed audit logging and tracing.

## Configuration

### Operator Configuration

The operator reads audit and tracing configuration from the `AgenticOLSConfig` CRD:

```yaml
apiVersion: agentic.openshift.io/v1alpha1
kind: AgenticOLSConfig
metadata:
  name: cluster
  namespace: openshift-lightspeed
spec:
  audit:
    # Enable/disable JSON audit logs to stdout (default: Enabled)
    logging: Enabled  # or Disabled
    
    # OpenTelemetry tracing export
    otel:
      # OTLP gRPC endpoint (e.g., "otel-collector.observability.svc:4317")
      # When empty, no traces are exported (no-op tracer)
      endpoint: "dev-collector.otel-observability.svc.cluster.local:4317"
      # TLS mode: Secure (default) or Insecure
      tlsMode: Insecure
```

**Binary behavior** ([cmd/main.go](https://github.com/openshift/lightspeed-agentic-operator/blob/main/cmd/main.go)):
- Reads `AgenticOLSConfig` named `cluster` in the operator namespace at startup
- If CR not found or audit config empty, defaults to: logging enabled, tracing disabled
- Initializes OTEL TracerProvider with OTLP gRPC exporter if endpoint configured
- Creates `ProductionAuditLogger` (JSON logs) or `NoOpAuditLogger` based on logging setting

### Sandbox Configuration

The sandbox uses **environment variables** for configuration (no CRD):

| Variable | Default | Description |
|----------|---------|-------------|
| `LIGHTSPEED_AUDIT_ENABLED` | `false` | Enable JSON audit logs (`"true"` to enable) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `""` | OTLP endpoint for traces (empty = disabled) |

**Binary behavior** ([app.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/app.py), [tracing.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/tracing.py)):
- `LIGHTSPEED_AUDIT_ENABLED=true` enables `AuditLogger._emit()` JSON output
- `OTEL_EXPORTER_OTLP_ENDPOINT` configures TracerProvider with OTLP gRPC exporter
- TLS auto-detected: `https://` prefix = TLS, otherwise insecure
- Standard Python logging always enabled (`logging.basicConfig`)

**Note**: The sandbox uses custom tracer initialization and does **not** respect standard OTEL SDK env vars like `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_SERVICE_NAME`, `OTEL_TRACES_EXPORTER`, or `OTEL_EXPORTER_OTLP_PROTOCOL`. Only `OTEL_EXPORTER_OTLP_ENDPOINT` is read.

**Example sandbox pod env**:
```yaml
env:
- name: LIGHTSPEED_AUDIT_ENABLED
  value: "true"
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: "http://dev-collector.otel-observability.svc.cluster.local:4317"
```

## Improvements

### Audit logs and span events difference 

#### Operator

The operator ([audit.go](https://github.com/openshift/lightspeed-agentic-operator/blob/main/controller/proposal/audit.go)) emits both JSON audit logs and OTEL span events for each lifecycle event. They contain different data:

| Signal | Content | Purpose |
|--------|---------|---------|
| JSON logs (`emitStructuredLog`) | Full CR payloads (metadata, spec, status) | Compliance audit - complete record |
| Span events (`addSpanEvent`) | Summarized attributes (truncated, limited counts) | Observability - lightweight tracing |

Example for `audit.analysis.completed`:
- **Log**: Full `analysisResult` CR with all options, diagnoses, proposals
- **Span event**: `proposal.name`, `result.name/uid`, `options.count`, first 3 options only (title, risk)

#### Sandbox

The sandbox has **three separate output mechanisms** that emit overlapping but different data:

| Mechanism | File | Format | Purpose |
|-----------|------|--------|---------|
| Standard logging | [logging.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/logging.py) | `logger.info()` text | Developer debugging |
| JSON audit logs | [audit.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/audit.py) `_emit()` | `print(json.dumps(...))` | Compliance audit |
| OTEL spans | [audit.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/audit.py) | `tracer.start_span()` | Distributed tracing |

**Standard logging** (`EventLogger` in logging.py):
```
INFO lightspeed_agentic: [provider:analysis] tool_use: exec_command({"cmd":"kubectl get pods"...})
INFO lightspeed_agentic: [provider:analysis] tool_result: Chunk ID: abc123...
INFO lightspeed_agentic: [provider:analysis] result: cost=$0.0150, tokens=1500
```

**JSON audit logs** (`AuditLogger._emit()` in audit.py):
```json
{"timestamp": "...", "level": "audit", "event": "audit.agent.tool.call", "trace_id": "...", "phase": "analysis", "tool_name": "exec_command", "tool_input": "..."}
```

**OTEL spans** (in audit.py):
- `tool.{name}` span with attributes: `tool.name`, `tool.input`, `tool.output`

| Event | Standard Log | JSON Audit | OTEL Span |
|-------|--------------|------------|-----------|
| Agent started | None | `audit.agent.started` | None |
| Tool call | `tool_use: name(input)` (500 chars) | `audit.agent.tool.call` (full input) | Creates `tool.{name}` span (300 chars) |
| Tool result | `tool_result: output` (1000 chars) | `audit.agent.tool.result` (full output) | Sets `tool.output` attr (500 chars), ends span |
| Thinking | `thinking: ...` (2000 chars) | `audit.agent.thinking` (full text) | None |
| Text output | None | `audit.agent.text` (full text) | None |
| Result | `result: cost=$X, tokens=Y` | `audit.agent.completed` | None |

**Key differences**:
- **Spans only cover tool execution** — no spans for thinking, text, started, completed events
- **Audit logs have full payloads** — spans truncate (input: 300 chars, output: 500 chars)
- **Spans capture duration** — audit logs only have point-in-time timestamps
- **Spans have hierarchy** — parent-child relationships; audit logs are flat with just `trace_id`

**Logging framework**: Python standard `logging` module ([app.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/app.py#L21)):
```python
logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
```

### Align spans with OTEL semantic conventions

The sandbox and operator emit spans with custom attributes. This section documents the current instrumentation and proposes alignment with OTEL semantic conventions.

#### Current sandbox spans

**`agent.run` span** (in `routes/query.py`):
| Current Attribute | Current Value |
|-------------------|---------------|
| span name | `agent.run` |
| `model` | model name |
| `provider` | provider name |

**`tool.{name}` span** (in `audit.py`):
| Current Attribute | Current Value |
|-------------------|---------------|
| span name | `tool.{tool_name}` (e.g., `tool.exec_command`) |
| `tool.name` | tool name |
| `tool.input` | input JSON (truncated 300 chars) |
| `tool.output` | output (truncated 500 chars) |

#### Current operator spans

The operator (`controller/proposal/audit.go`) creates workflow/lifecycle spans. These are **not GenAI spans** — they track the Kubernetes proposal lifecycle, not LLM inference.

**`proposal.lifecycle` span** (root span, from proposal UID):
| Attribute | Value |
|-----------|-------|
| `proposal.name` | proposal name |
| `proposal.namespace` | namespace |
| `proposal.uid` | UID |

**`proposal.analyze` / `proposal.execute` / `proposal.verify` / `proposal.escalate` spans**:
| Attribute | Value |
|-----------|-------|
| `proposal.name` | proposal name |
| `proposal.namespace` | namespace |
| `retry_index` | (on execute/verify, if retrying) |

**`proposal.human_approval` span** (measures human decision time):
| Attribute | Value |
|-----------|-------|
| `proposal.name` | proposal name |
| `proposal.namespace` | namespace |
| `approver.uid` | approver UID (on end) |
| `approver.username` | approver username (on end) |
| `approval.decision` | decision (on end) |

**`proposal.terminal` span**:
| Attribute | Value |
|-----------|-------|
| `proposal.name` | proposal name |
| `proposal.namespace` | namespace |
| `phase` | terminal phase |
| `reason` | terminal reason |

**Span events** (attached to phase spans, contain result CR attributes):

| Event | CR Type | Attributes |
|-------|---------|------------|
| `audit.analysis.completed` | AnalysisResult | `result.name`, `result.uid`, `options.count`, `option.{i}.title`, `option.{i}.risk` |
| `audit.execution.completed` | ExecutionResult | `result.name`, `result.uid`, `actions_taken.count`, `failure_reason`, `action.{i}.type`, `action.{i}.description` |
| `audit.verification.completed` | VerificationResult | `result.name`, `result.uid`, `summary`, `checks.count`, `check.{i}.name`, `check.{i}.result` |
| `audit.verification.retry` | VerificationResult | `result.name`, `summary`, `retry_count`, `checks.count` |
| `audit.escalation.completed` | EscalationResult | (logged, attributes TBD) |

Note: Span events truncate/limit data (e.g., first 3 options, first 5 actions/checks) while JSON audit logs contain full CR payloads.

#### Proposed changes for sandbox (per GenAI semconv)

**For inference spans** (`agent.run` → should be inference span):
| Current | GenAI Semconv |
|---------|---------------|
| span name: `agent.run` | `{gen_ai.operation.name} {gen_ai.request.model}` (e.g., `chat claude-sonnet-4-20250514`) |
| `model` | `gen_ai.request.model` |
| `provider` | `gen_ai.provider.name` |
| (missing) | `gen_ai.operation.name` = `"chat"` (Required) |

**For tool execution spans**:
| Current | GenAI Semconv |
|---------|---------------|
| span name: `tool.{name}` | `execute_tool {gen_ai.tool.name}` |
| `tool.name` | `gen_ai.tool.name` (Required) |
| `tool.input` | `gen_ai.tool.call.arguments` (Opt-In) |
| `tool.output` | `gen_ai.tool.call.result` (Opt-In) |
| (missing) | `gen_ai.operation.name` = `"execute_tool"` (Required) |
| (missing) | `gen_ai.tool.call.id` (Recommended) |
| (missing) | `gen_ai.tool.type` = `"function"` (Recommended)

#### Proposed changes for operator

Operator spans track Kubernetes workflow lifecycle, not GenAI inference. Custom `proposal.*` attributes are appropriate since Proposal is a custom resource.

Per [Kubernetes semantic conventions](https://opentelemetry.io/docs/specs/semconv/resource/k8s/):

| Current | K8s Semconv                                                                      |
|---------|----------------------------------------------------------------------------------|
| `proposal.namespace` | `k8s.namespace.name` (stable, already collected)                                 |
| `proposal.name` | No standard for CR names; `k8s.proposal.name` exists for built-in resources only |
| `proposal.uid` | No standard for CR UIDs; `k8s.proposal.uid` exists for built-in resources only   |

### Tool spans not exported from sandbox

**Problem**: Tool spans (`tool.kubectl`, `tool.exec_command`, etc.) created in `AuditLogger` were not appearing in Tempo. Only `agent.run` spans were visible, losing visibility into individual tool calls.

**Root cause**: In `audit.py`, `start_span()` was called without an explicit `context=` parameter. In Python async code, the OTEL context is not automatically propagated when spans are created inside async generators. The `AuditLogger` was created before entering the `agent.run` span block, so tool spans were orphaned.

**Fix** (in `lightspeed-agentic-sandbox`) - PR https://github.com/openshift/lightspeed-agentic-sandbox/pull/97:

1. **`src/lightspeed_agentic/audit.py`**: Added parent context storage and explicit context passing:
   ```python
   from opentelemetry.context import Context
   from opentelemetry.trace import StatusCode

   class AuditLogger:
       def __init__(self, ...):
           self._parent_context: Context | None = None

       def set_parent_context(self, ctx: Context) -> None:
           self._parent_context = ctx

       def process_event(self, event: ProviderEvent) -> None:
           case "tool_call":
               self._tool_span = self._tracer.start_span(
                   f"tool.{self._last_tool_name}",
                   context=self._parent_context,  # Explicit parent
                   attributes={"tool.name": ..., "tool.input": ...},
               )
           case "tool_result":
               self._tool_span.set_attribute("tool.output", event.output[:500])
               self._tool_span.set_status(StatusCode.OK)
               self._tool_span.end()
   ```

2. **`src/lightspeed_agentic/routes/query.py`**: Pass context after entering span:
   ```python
   from opentelemetry import context as otel_context

   with tracer.start_as_current_span("agent.run", context=trace_ctx, ...):
       audit_logger.set_parent_context(otel_context.get_current())
       result = provider.query(...)
   ```

**Result**: Correct span hierarchy:
```
proposal.lifecycle
└── proposal.analyze
    └── agent.run
        └── tool.exec_command  (now properly nested)
```

**Test Image**: `ghcr.io/pavolloffay/lightspeed-agentic-sandbox:tracing-fix`

### Use standard OTEL SDK initialization in sandbox

**Problem**: The sandbox uses custom tracer initialization in [tracing.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/tracing.py) that only reads `OTEL_EXPORTER_OTLP_ENDPOINT`. Standard OTEL SDK environment variables are ignored:

| Ignored Variable | Purpose |
|------------------|---------|
| `OTEL_SERVICE_NAME` | Override service name (hardcoded to `lightspeed-agentic-sandbox`) |
| `OTEL_EXPORTER_OTLP_INSECURE` | Explicit TLS control |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | Protocol selection (grpc/http) |
| `OTEL_TRACES_EXPORTER` | Exporter selection (otlp/console/none) |
| `OTEL_EXPORTER_OTLP_HEADERS` | Custom headers (auth tokens) |
| `OTEL_RESOURCE_ATTRIBUTES` | Additional resource attributes |

**Fix** (in `lightspeed-agentic-sandbox`) - PR https://github.com/openshift/lightspeed-agentic-sandbox/pull/98:

Refactored `tracing.py` to respect all standard OTEL_* environment variables:
- `OTEL_SERVICE_NAME`: Override service name
- `OTEL_EXPORTER_OTLP_INSECURE`: Explicit TLS control
- `OTEL_EXPORTER_OTLP_PROTOCOL`: Protocol selection (grpc/http)
- `OTEL_EXPORTER_OTLP_HEADERS`: Custom headers (auth tokens)
- `OTEL_RESOURCE_ATTRIBUTES`: Additional resource attributes
- `OTEL_TRACES_EXPORTER`: Exporter selection (otlp/console/none)

**Test Image**: `ghcr.io/pavolloffay/lightspeed-agentic-sandbox:otel-sdk-autoconfig`

**Benefits**:
- All standard OTEL env vars work (`OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_HEADERS`, etc.)
- Consistent with OTEL Python conventions
- Easier to configure in different environments (dev/staging/prod)