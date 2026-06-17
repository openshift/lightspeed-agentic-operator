# Agentic Telemetry Data Model

## Overview

The agentic system collects telemetry at multiple levels of detail, designed to be extensible without breaking downstream consumers.

## Telemetry Tiers

| Tier | Scope | Description |
|------|-------|-------------|
| Step metrics | Individual agent call | Cost, latency, token counts, model/provider per step |
| Proposal summary | Entire proposal lifecycle | Aggregated outcome, total cost/duration, approval decisions |
| Transcript | Prompt/response/tool calls | Full LLM interaction trace for debugging and analysis |
| Audit trail | Identity, authorization, decisions | Compliance-grade record of who did what and why |

---

## Step Metrics

Collected from the sandbox agent's response envelope and stored on each Result CR status. The data is available in-cluster but not yet exported to any external telemetry system.

### Schema: `StepMetrics`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `latencyMs` | int64 | Yes | Wall-clock time (ms) the agent spent processing |
| `inputTokens` | int64 | No | Input tokens consumed by the LLM |
| `outputTokens` | int64 | No | Output tokens produced by the LLM |
| `costUsd` | string | No | Estimated cost in USD (decimal string, e.g. "0.05"). Omitted when unknown. |
| `model` | string | No | LLM model used (e.g. "claude-opus-4-6") |
| `provider` | string | No | LLM provider (e.g. "anthropic", "openai") |
| `toolCallsCount` | int32 | No | Number of tool invocations the agent made |

### Storage location

Each Result CR (AnalysisResult, ExecutionResult, VerificationResult, EscalationResult) has:
```yaml
status:
  metrics:
    latencyMs: 4500
    inputTokens: 1200
    outputTokens: 350
    costUsd: "0.03"
    model: claude-opus-4-6
    provider: anthropic
    toolCallsCount: 5
```

---

## Proposal Summary

Aggregated from step metrics + proposal lifecycle events. The proposal summary is a nested structure: proposal-level aggregates at the top, with per-step detail embedded. The per-step detail is derived directly from the Step Metrics tier (read from Result CR `.status.metrics` at terminal phase).

### Proposal-level fields

| Field | Type | Source | Description |
|-------|------|--------|-------------|
| `proposalName` | string | Proposal CR | Unique identifier |
| `namespace` | string | Proposal CR | Proposal namespace |
| `finalPhase` | string | DerivePhase() | Terminal phase (Completed, Failed, Denied, Escalated) |
| `totalLatencyMs` | int64 | Sum of steps | End-to-end processing time |
| `totalCostUsd` | string | Sum of steps | Total LLM cost |
| `totalInputTokens` | int64 | Sum of steps | Total input tokens across all steps |
| `totalOutputTokens` | int64 | Sum of steps | Total output tokens across all steps |
| `approvalDecision` | enum | ProposalApproval | Approved, Denied, TimedOut |
| `targetNamespaces` | []string | Proposal spec | Cluster namespaces targeted |
| `createdAt` | timestamp | Proposal CR | When the proposal was created |
| `completedAt` | timestamp | Terminal condition | When it reached terminal state |

### Per-step fields (nested under `steps[]`)

Each entry corresponds to a workflow step that ran. Metrics fields come directly from Result CR `.status.metrics`.

| Field | Type | Source | Description |
|-------|------|--------|-------------|
| `name` | string | Step type | "analysis", "execution", "verification", "escalation" |
| `outcome` | enum | Result CR condition | Succeeded, Failed |
| `model` | string | StepMetrics | LLM model used for this step |
| `provider` | string | StepMetrics | LLM provider used for this step |
| `latencyMs` | int64 | StepMetrics | Wall-clock time for this step |
| `inputTokens` | int64 | StepMetrics | Input tokens for this step |
| `outputTokens` | int64 | StepMetrics | Output tokens for this step |
| `costUsd` | string | StepMetrics | Cost for this step |
| `toolCallsCount` | int32 | StepMetrics | Tool invocations for this step |
| `failureReason` | string | Result CR status | Why this step failed (if applicable) |
| `retryCount` | int32 | Execution status | Retries (execution only) |

### Example JSON

```json
{
  "proposalName": "fix-oom-staging",
  "namespace": "openshift-lightspeed",
  "finalPhase": "Completed",
  "totalLatencyMs": 12500,
  "totalCostUsd": "0.12",
  "totalInputTokens": 3500,
  "totalOutputTokens": 900,
  "approvalDecision": "Approved",
  "targetNamespaces": ["staging"],
  "createdAt": "2026-06-16T10:00:00Z",
  "completedAt": "2026-06-16T10:02:30Z",
  "steps": [
    {
      "name": "analysis",
      "outcome": "Succeeded",
      "model": "claude-opus-4-6",
      "provider": "anthropic",
      "latencyMs": 4500,
      "inputTokens": 1200,
      "outputTokens": 350,
      "costUsd": "0.03",
      "toolCallsCount": 5
    },
    {
      "name": "execution",
      "outcome": "Succeeded",
      "model": "claude-opus-4-6",
      "provider": "anthropic",
      "latencyMs": 6000,
      "inputTokens": 1500,
      "outputTokens": 400,
      "costUsd": "0.05",
      "toolCallsCount": 8,
      "retryCount": 0
    },
    {
      "name": "verification",
      "outcome": "Succeeded",
      "model": "claude-opus-4-6",
      "provider": "anthropic",
      "latencyMs": 2000,
      "inputTokens": 800,
      "outputTokens": 150,
      "costUsd": "0.04",
      "toolCallsCount": 3
    }
  ]
}
```

### Open questions

- Where is the summary computed? Operator at terminal phase? An exporter job?
- Where is it stored? Proposal status (ephemeral if TTL deletes it), or pushed to external system?
- What's the export target? Dataverse? Prometheus? Both?

---

## Transcript

Per-step detail including prompt content, tool call traces, and LLM reasoning. This is the **agent's responsibility** — the operator does not collect or store transcript data. The agent logs this information to its own stdout (available via pod logs / cluster log aggregator).

### Data captured by the agent

| Field | Type | Description | Notes |
|-------|------|-------------|-------|
| `promptInput` | string | Full prompt sent to LLM | Already in Proposal spec (request text) |
| `llmOutput` | string | Raw LLM response text | Already stored in Result CRs (options, actions, checks) |
| `toolCalls` | []ToolCall | Each tool invocation (name, args, result, duration) | Agent logs only |
| `reasoningTrace` | string | Chain-of-thought / scratchpad (if available) | Agent logs only |
| `inferenceId` | string | Provider-assigned inference ID | Agent logs only |
| `modelVersion` | string | Exact model version/checkpoint | Agent logs only |

### Per tool call

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Tool name (e.g. "kubectl_get") |
| `arguments` | string | Arguments passed |
| `result` | string | Tool output (may contain cluster state) |
| `durationMs` | int64 | Time spent in tool |
| `success` | bool | Whether tool succeeded |

### Notes

- The agent already logs transcripts to stdout during execution
- Available via `oc logs` on the sandbox pod (or cluster log aggregator)
- Too large for CRDs (prompts + tool outputs can be 100KB+)
- Must be opt-in for export due to sensitivity

---

## Audit Trail

Required for PCI DSS 10.2, NIST 800-53 AU-10 compliance.

Audit Trail extends the Proposal Summary with identity, authorization, and compliance fields. It is not a separate data structure — it adds columns to the same record.

### Additional proposal-level fields (beyond Proposal Summary)

| Field | Type | Description |
|-------|------|-------------|
| `escalationTrigger` | string | Why human review was required |
| `reviewerIdentity` | string | Who approved/denied execution |
| `reviewTimestamp` | timestamp | When human intervention occurred |

### Additional per-step fields (beyond Proposal Summary `steps[]`)

| Field | Type | Description |
|-------|------|-------------|
| `agentIdentity` | string | ServiceAccount the pod ran as |
| `agentName` | string | Agent CR name used for this step |
| `authorizationScope` | []string | RBAC permissions granted (execution step only) |

> Note: Fields for failed proposal recovery (restart/retry) will be added when OLS-3278 is implemented.

### Behavioral telemetry

Behavioral metrics (action velocity, permission escalation rate, cross-boundary calls, exception rate) are not collected directly — they are computed from the raw telemetry above. Design of derived metrics belongs in the dashboard/alerting layer, not the data model.

---

## Data Classification

| Data category | Sensitivity | Default treatment | Notes |
|---------------|-------------|-------------------|-------|
| Token counts, latency, cost | **Non-sensitive** | Ship as-is | Operational metrics |
| Model/provider names | **Non-sensitive** | Ship as-is | Infrastructure metadata |
| Proposal outcome, retry count | **Non-sensitive** | Ship as-is | Workflow state |
| Target namespaces | **Low** | Ship as-is | Cluster topology hint |
| Approval decision + stage | **Low** | Ship as-is | Workflow decision |
| Failure reason (condition message) | **Medium** | Redact cluster-specific details | May contain resource names |
| Proposal request text | **Medium** | Opt-in only | May contain PII or context |
| Reviewer/approver identity | **Medium** | Hash for external export | Employee information |
| Tool outputs (pod logs, oc output) | **High** | Do not export without explicit opt-in | Contains cluster state, possibly secrets |
| Prompt content | **High** | Do not export | May contain injected secrets or context |
| LLM reasoning trace | **High** | Do not export | May reflect sensitive logic |

---

## Extensibility Design

1. **Additive fields only** — new fields added as optional; never remove or rename existing ones
2. **Tiers are independent** — Step Metrics works without Proposal Summary; Proposal Summary works without Transcript
3. **Per-step metrics on Result CRs** — the atomic unit; proposal summaries are computed from these
4. **Export format is decoupled from storage** — CRs store structured data; exporters transform to target format (Dataverse, Prometheus, etc.)
5. **Sensitivity tiers gate export** — non-sensitive ships by default; medium requires opt-in; high requires explicit configuration

---

## Open Questions (Requiring BU Input)

1. Which Proposal Summary fields does product actually need for "make the system better" analytics?
2. What is the Dataverse table structure? Can we reuse the existing OLS transcript schema or need a new one?
3. What's the export cadence — real-time (webhook/stream) or batch (periodic job)?
4. Is there a retention policy — how long should telemetry be kept?
5. For compliance (Audit Trail) — what regulations apply to this product specifically?
