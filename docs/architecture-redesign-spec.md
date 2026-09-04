# Architecture Redesign: Batch Execution, Proposal Gateway, and Processing Gating

**Date:** 2026-07-02  
**Status:** Draft  
**Jira:** TBD  
**Authors:** Boris Lublinsky, AI-assisted design

## Problem Statement

The current agentic-operator architecture has structural limitations that constrain scalability, reliability, and operational safety:

1. **Extended reconciliation time** — The operator blocks on HTTP calls to the sandbox for up to 5 minutes per step, tying up reconciler goroutines and starving the work queue.
2. **Brittle operator–sandbox contract** — HTTP 200 for all responses (including errors), no versioning, lockstep deployment required for any response shape change.
3. **etcd pressure and processing gating** — No mechanism prevents a flood of proposals from overwhelming etcd, the operator, or cluster compute resources. Terminal proposals accumulate with no cleanup.
4. **Proposal lifecycle management** — Failed proposals are dead-end states with no recovery path. No TTL-based cleanup.
5. **Configuration sprawl** — Each proposal carries full configuration inline (provider, model, MCP, tools, and timeout). Repeated across proposals, inflates CR size, and makes changing defaults a per-proposal operation.
6. **RBAC accuracy** — Analysis predicts execution permissions; predictions can be wrong, causing 403 failures during execution.
7. **Multi-SDK complexity** — Three divergent provider adapters with inconsistent tool calling, MCP integration, structured output handling, and disk write behavior. N×M maintenance burden grows with every new provider or capability.

These are addressed by 9 design decisions:

| Problem | Design section(s) |
|---------|-------------------|
| Extended reconciliation | 1 — Batch Execution Model |
| Brittle contract | 2 — Contract Versioning |
| etcd pressure / gating | 3 — Proposal Gateway, 6 — Phase Labels, 7 — Processing Gating |
| Lifecycle management | 4 — Lifecycle Management |
| Configuration sprawl | 5 — Configuration Hierarchy |
| RBAC accuracy | 9 — RBAC Accuracy via Single-Pod Analysis |
| Multi-SDK complexity | 8 — Unified Agent Runtime |

## Design Decisions

### 1. Batch Execution Model (replaces HTTP)

**Related Jira:** [OLS-3284](https://redhat.atlassian.net/browse/OLS-3284) (Run-to-completion agent model with file-based I/O), [OLS-3066](https://redhat.atlassian.net/browse/OLS-3066) (Decouple Proposal reconcile latency from sandbox management and agent HTTP), [OLS-3264](https://redhat.atlassian.net/browse/OLS-3264) (Release sandbox pods immediately after step completion)

The sandbox runtime changes from a long-running HTTP server to a batch process that reads input, runs the agent, writes output, and exits.

#### Why batch instead of HTTP

The current architecture has the operator call the sandbox via `POST /v1/agent/run` and block for up to 5 minutes waiting for a response. This means:

- Reconciler goroutines are occupied for the duration of agent execution
- A handful of long-running proposals can starve the entire work queue
- The operator must manage HTTP timeouts, TLS, connection pooling
- Network failures between operator and sandbox cause ambiguous states (did it run or not?)

With a batch model, the operator creates a pod and returns immediately. The Kubernetes informer notifies the operator when the pod completes — a native, event-driven pattern. Reconciliation time drops from minutes to milliseconds.

#### Why ConfigMap for data transfer

The batch pod needs input (task specification) and must deliver output (agent result). Several mechanisms were evaluated:

| Mechanism | Pros | Cons |
|-----------|------|------|
| **ConfigMap in/out** | Simple K8s API. Survives pod deletion. Auditable (`oc get cm`). Operator reads via normal client. Owner ref cascade cleanup. | 1MB size limit. Sandbox needs `update` RBAC for output CM. |
| **emptyDir volume** | Zero RBAC for sandbox. | Does not survive pod deletion — operator must read before cleanup. Requires pod to still exist. |
| **Pod logs** | Zero RBAC. `kubectl logs` friendly. | Fragile parsing. Log rotation risk on long runs. Noisy (agent debug output mixed with result). |
| **Pod terminationMessage** | Zero RBAC. Built-in K8s feature. | 4KB–256KB limit. Too small for analysis results. |
| **Direct Result CR write** | Native K8s object. No intermediate translation step. | **Full coupling to operator CRD types.** Sandbox (Python) must construct valid typed CRs. Any schema change (new field, validation rule, renamed status field) requires sandbox updates — reintroduces lockstep deployment. |

**The decisive argument against direct CR access:** Every time the operator team adds a field to `StepMetrics`, changes a validation marker, or renames a status field, the sandbox Python code must be updated in lockstep. This is the same tight coupling that makes the current HTTP response envelope brittle — just via a different transport. The sandbox should not know about operator CRD types.

**ConfigMap keeps a clean boundary:** The sandbox writes raw JSON (any structure the contract version defines). The operator reads it, interprets it, and creates typed Result CRs. All Kubernetes API semantics (CRDs, owner refs, conditions, phases) stay in the operator where they belong. The sandbox remains a pure execution runtime with one simple contract: "JSON in, JSON out."

The 1MB ConfigMap limit is not a practical concern — agent task specs are typically < 50KB and results are < 100KB.

#### Data flow

```
Operator creates:
  1. Input ConfigMap    ("ls-{step}-{proposal}-input")
  2. Output ConfigMap   ("ls-{step}-{proposal}-output", empty)
  3. Pod with:
     - Input CM mounted read-only at /var/run/agent/input/
     - Output CM name as env var AGENT_OUTPUT_CM
     - Credentials via Secret volume (unchanged)
     - Provider/model via env vars

Pod runs:
  4. Reads /var/run/agent/input/task.json
  5. Runs agent to completion
  6. Writes result JSON to output ConfigMap via K8s API (single atomic PATCH)
  7. Exits 0 (result written to CM — even if agent failed) or non-zero (CM not written — infrastructure failure)

Exit code semantics:
  - Exit 0: operator reads output CM (may contain success or agent-level failure)
  - Exit non-zero: operator treats as infrastructure crash (CM may be empty), applies retry policy

Operator reconciles (triggered by pod completion via informer):
  8. Reads output ConfigMap
  9. Creates typed Result CR
  10. Cleans up (or lets owner refs cascade)
```

#### Operator reconcile pattern

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... load proposal, derive phase ...

    switch phase {
    case Analyzing:
        if no analysis pod exists:
            createInputCM(proposal, "analysis")
            createOutputCM(proposal, "analysis")
            createPod(proposal, "analysis", readOnlyRBAC)
            setCondition(Analyzed, Unknown, "InProgress")
            return // no requeue — informer will trigger on pod completion
        if pod Succeeded:
            result := readOutputCM(proposal, "analysis")
            createAnalysisResult(result)
            setCondition(Analyzed, True)
            cleanupStepResources()
        if pod Failed:
            handleStepFailure()
    // ... similar for Executing, Verifying ...
    }
}
```

Each reconciliation completes in milliseconds. The operator never blocks.

#### Timeout enforcement (three layers)

| Layer | Mechanism | Behavior |
|-------|-----------|----------|
| Sandbox internal | Reads timeout from task.json, sets alarm | Writes `{success: false, summary: "timed out"}` to output CM, exits 0 |
| Kubernetes hard kill | `activeDeadlineSeconds` = timeout + 60s | Kubelet terminates pod. No output written. |
| Operator reaper | Checks step start time on periodic requeue | Deletes zombie pods that survived both above |

#### Resource ownership

All step resources (input CM, output CM, pod) are owned by the Proposal CR with `Controller: true, BlockOwnerDeletion: true`. Proposal deletion cascades cleanup automatically.

#### Naming convention

```
Input CM:   ls-{step}-{proposal-name}-input
Output CM:  ls-{step}-{proposal-name}-output
Pod:        ls-{step}-{proposal-name}
```

### 2. Contract Versioning

With HTTP eliminated, the contract is a JSON document format with explicit versioning.

#### Input contract (task.json)

```json
{
  "version": "v1",
  "query": "Analyze the following alert and propose remediation options...",
  "outputSchema": {
    "type": "object",
    "properties": { ... }
  },
  "context": {
    "targetNamespaces": ["production"],
    "previousAttempts": [],
    "approvedOption": null
  },
  "timeout_ms": 300000,
  "allowed_tools": ["Bash", "Read", "Glob", "Grep"]
}
```

#### Output contract (result.json in output CM)

```json
{
  "version": "v1",
  "success": true,
  "summary": "...",
  "metrics": {
    "latency_ms": 45000,
    "input_tokens": 12000,
    "output_tokens": 3500,
    "model": "claude-sonnet-4-20250514",
    "provider": "anthropic",
    "tool_calls_count": 8
  },
  "result": { ... }
}
```

#### Evolution rules

- Input version drives output format (operator controls which version it sends)
- Sandbox supports version N and N-1 simultaneously
- Upgrade path: sandbox deploys with new version support → operator upgrades, sends new version → sandbox drops old version later
- No lockstep deployment required

#### Parameter categories

| Category | Delivery mechanism | Rationale |
|----------|--------------------|-----------|
| Credentials (API keys, tokens) | Secret volume mount | SDKs read from env/files. Sensitive. |
| Provider identity | Pod env vars (LIGHTSPEED_PROVIDER) | SDK initialization reads env directly |
| Task parameters (query, schema, context, timeout, tools) | task.json via ConfigMap volume | Single versioned document. Operator controls format. |

### 3. Proposal Gateway

**Related Jira:** [OLS-3279](https://redhat.atlassian.net/browse/OLS-3279) (Durable state store for agent step outputs — evaluate alternatives to storing large payloads in CRs), [OLS-3296](https://redhat.atlassian.net/browse/OLS-3296) (Cluster-level defaults for Proposal configuration)

A standalone Go service that buffers proposal submissions in Postgres and promotes them to Kubernetes CRs when capacity exists.

#### Why a gateway is needed

**Problem 1: etcd pressure from proposal accumulation.**

Every Proposal with its associated resources consumes etcd storage:

| Resource | Approx size |
|----------|-------------|
| Proposal CR (spec + status) | 5–10 KB |
| ProposalApproval | 2–3 KB |
| AnalysisResult | 10–50 KB |
| ExecutionResult | 5–20 KB |
| VerificationResult | 5–10 KB |
| Input/Output ConfigMaps (per step) | 20–100 KB |
| **Total per fully-processed proposal** | **~50–200 KB** |

At scale:
- 100 proposals: 5–20 MB (negligible)
- 1,000 proposals: 50–200 MB (noticeable on shared clusters)
- 5,000 proposals: 250 MB – 1 GB (approaching etcd default 2 GB limit)

On shared OpenShift clusters where hundreds of other operators and workloads compete for etcd, even 1,000 active proposals can create pressure. Without a buffer, a burst scenario (mass alert storm generating hundreds of proposals in minutes) can overwhelm etcd before the operator has time to process and clean up.

The gateway solves this by holding the queue in Postgres (which handles millions of rows trivially) and only promoting proposals to Kubernetes when capacity exists. etcd only ever holds `maxActiveProposals` CRs plus recently-terminal proposals within TTL — a bounded, predictable footprint.

**Problem 2: flexible gating without custom admission logic.**

With proposals flowing through the gateway, sophisticated gating becomes a simple SQL query:

| Gating dimension | Implementation |
|------------------|---------------|
| Total active proposals | `SELECT count(*) FROM k8s_proposals WHERE phase NOT IN ('Completed','Failed','Denied')` |
| Per namespace | `WHERE namespace = X` — prevent one noisy tenant from monopolizing |
| Per proposal type / agent | `WHERE agent = Y` — limit expensive agents independently |
| Priority-based scheduling | `ORDER BY priority DESC, created_at ASC` — critical proposals promoted first |
| Time-of-day / maintenance windows | `WHERE NOT in_maintenance_window()` — hold proposals during change freezes |
| Rate limiting | Promote at most N per minute — smooth burst absorption |

These gating policies are trivial to implement as SQL predicates in the promotion loop. No admission webhooks, no custom controllers, no CRD changes. Adding a new gating dimension is adding a WHERE clause.

Without the gateway, each of these would require either a validating webhook (which rejects and loses work), complex reconciler logic (which adds code to the operator), or new CRDs for policy expression.

**Problem 3: clean kill switch.**

When the system is suspended (`AgenticOLSConfig.spec.suspended = true`), the gateway simply stops promoting. No new proposals enter Kubernetes — the queue holds in Postgres. Currently, the kill switch must find and terminate all in-flight proposals individually via per-proposal reconciliation. With the gateway, suspension is immediate and total: the promotion loop checks the suspension flag and skips promotion when it's active. Proposals already in Kubernetes continue to be handled by the operator (emergency stop), but no new work enters. When suspension is lifted, queued proposals resume promotion automatically — no manual re-submission needed.

#### Why Go

The gateway creates Proposal CRs from queued records. The Proposal spec is defined in `api/v1alpha1/proposal_types.go`, and the `api/go.mod` separate module exists specifically for downstream Go consumers of these types.

| Factor | Go | Python |
|--------|-----|--------|
| Creates typed Proposal CRs | Imports `api/v1alpha1/` directly — compile-time schema validation | Constructs raw JSON dicts — silent breakage when CRD changes |
| K8s API interaction | client-go is first-class, typed, production-proven | client-python works but is untyped and slower |
| Schema evolution safety | Compiler fails if Proposal spec changes and gateway is outdated | Discovered at runtime in production |
| Container footprint | ~20 MB static binary | ~300 MB+ (Python runtime + dependencies) |
| Memory per replica | Low (compiled, minimal GC at this load) | Higher (interpreter overhead) |
| Team consistency | Same language, CI, testing patterns as operator | Different toolchain |

The decisive argument: when the Proposal CRD evolves (new fields, renamed status fields, validation changes), the Go compiler immediately surfaces breakage in the gateway at build time. In Python, you discover it when CR creation fails in production.

#### Architecture

```
Console / CLI / Alerts
         │ creates ProposalRequest CR
         ▼
┌────────────────────┐
│  Kubernetes / etcd │  (ProposalRequest CRs — ephemeral, seconds)
└────────┬───────────┘
         │ gateway watches, picks up immediately
         ▼
┌────────────────────┐
│  Proposal Gateway  │◄──── Postgres (durable queue)
│  (controller)      │
└────────┬───────────┘
         │ creates Proposal CRs when capacity exists
         ▼
┌────────────────────┐
│  Kubernetes / etcd │  (Proposal CRs — bounded, active only)
└────────────────────┘
```

#### Interface: ProposalRequest CR

Consumers submit proposals by creating a lightweight `ProposalRequest` CR. The gateway controller watches these, immediately stores the spec in Postgres, and deletes the CR:

1. Consumer creates `ProposalRequest` CR (~1-2 KB)
2. Gateway controller picks it up (within seconds)
3. Stores in Postgres
4. Deletes the `ProposalRequest` CR from Kubernetes

ProposalRequest CRs are ephemeral — they exist for seconds, not minutes. Under normal operation, etcd holds near-zero of them at any time.

**Why CR interface instead of REST API:**

| Concern | REST API | CR interface |
|---------|----------|-------------|
| Gateway down | Submissions fail immediately | CRs queue in K8s — processed on recovery |
| Postgres down | Submissions fail immediately | CRs queue in K8s — stored to DB on recovery |
| Auth | Custom token validation needed | Standard K8s RBAC on CR creation |
| Client tooling | New REST client in CLI/console | Standard `oc apply` / `kubectl create` |
| API maintenance | REST endpoints to document/version | CRD schema only |
| Monitoring | Custom metrics for HTTP service | Standard controller-runtime metrics |

No new HTTP service, no load balancer, no REST API to maintain. The gateway is a controller — same pattern as the operator itself.

#### Authentication

Standard Kubernetes RBAC. Consumers need `create` permission on `proposalrequests.agentic.openshift.io`. No custom auth implementation.

#### Database schema

```sql
CREATE TABLE proposals (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace   TEXT NOT NULL,
    name        TEXT NOT NULL,
    spec        JSONB NOT NULL,
    priority    INT DEFAULT 0,
    created_at  TIMESTAMP WITH TIME ZONE DEFAULT now(),
    status      TEXT DEFAULT 'queued'
);

CREATE INDEX idx_proposals_queue ON proposals (status, priority DESC, created_at ASC)
    WHERE status = 'queued';
```

#### Promotion loop

```
every 1s:
    activeCount = count Proposal CRs where phase NOT IN (Completed, Failed, Denied)
    if activeCount < maxConcurrent:
        slotsAvailable = maxConcurrent - activeCount
        candidates = SELECT ... FROM proposals
                     WHERE status = 'queued'
                     ORDER BY priority DESC, created_at ASC
                     LIMIT slotsAvailable
                     FOR UPDATE SKIP LOCKED

        for each candidate:
            create Proposal CR from candidate.spec
            DELETE FROM proposals WHERE id = candidate.id
```

`FOR UPDATE SKIP LOCKED` ensures safe promotion even if the gateway is briefly processing overlapping timer ticks.

#### Ownership transitions

| Stage | Lives in | Deleted from |
|-------|----------|-------------|
| Queued | Postgres | — |
| Promoted | Kubernetes CR | Postgres (on successful CR creation) |
| Terminal + TTL expired | Nowhere | Kubernetes (TTL cleanup) |

No duplication. Single owner at each stage.

#### High availability

Single replica, standard Kubernetes Deployment restart. If the gateway pod crashes:
- ProposalRequest CRs accumulate harmlessly in K8s during restart (~10-30s)
- Queued proposals in Postgres wait for promotion (no urgency — proposals take minutes to process)
- No data loss, no submissions rejected

Same model as the operator itself. In a system where end-to-end proposal processing takes minutes, 10-30s of gateway downtime is imperceptible.

#### Console integration

ProposalRequest CRs are ephemeral (deleted within seconds) — the console cannot rely on watching them for queue visibility. The source of truth for queued proposals is the gateway's Postgres database.

The gateway exposes a minimal read-only REST endpoint for the console:

- `GET /v1/queue` → list of queued proposals (from Postgres)

Console combines:
1. `GET /v1/queue` → queued proposals (from gateway)
2. K8s API watch on `Proposal` CRs → active + terminal proposals

Unified view: Queued → Analyzing → Proposed → Executing → Verifying → Completed

### 4. Lifecycle Management

**Related Jira:** [OLS-3278](https://redhat.atlassian.net/browse/OLS-3278) (Proposal lifecycle management: TTL-based cleanup, manual cleanup, and failed proposal recovery)

#### TTL-based cleanup

Terminal proposals (Completed, Failed, Denied) are deleted after a configurable TTL.

Configuration in `ApprovalPolicy`:

```yaml
spec:
  retention:
    completedTTL: 24h
    failedTTL: 7d
    deniedTTL: 48h
```

Implementation: periodic `RunnableFunc` in the operator lists terminal proposals, deletes those past their TTL. Owner ref cascade handles Result CRs, ConfigMaps, and Approvals.

#### No recovery mechanism needed

Failed proposals don't need a "restart" feature. The user (or alert pipeline) re-submits via the gateway. The gateway queues it. A new proposal is created. Simple, stateless, no state machine complexity.

### 5. Configuration Hierarchy (Defaults in AgenticOLSConfig)

**Related Jira:** [OLS-3296](https://redhat.atlassian.net/browse/OLS-3296) (Cluster-level defaults for Proposal configuration)

#### The problem

Currently, each Proposal carries full configuration inline: LLM provider, model, MCP servers, skills, tools, and timeout. This makes proposals unnecessarily large (~5-10 KB of repeated configuration per proposal) and creates maintenance overhead — changing a default (e.g., switching model) requires updating every proposal template.

#### Solution: cluster-level defaults with proposal-level overrides

`AgenticOLSConfig` (the existing cluster-scoped singleton) becomes the home for default configuration. Proposals only specify **overrides** — fields they want different from the cluster default:

```yaml
# AgenticOLSConfig (cluster defaults)
apiVersion: agentic.openshift.io/v1alpha1
kind: AgenticOLSConfig
metadata:
  name: cluster
spec:
  defaults:
    provider: anthropic
    model: claude-sonnet-4-20250514
    timeout: 300s
    mcpServers:
      - name: openshift
        url: http://openshift-mcp-server.openshift-lightspeed.svc:8080/mcp
    tools:
      allowedTools: ["Bash", "Read", "Glob", "Grep"]
```

```yaml
# Proposal (only overrides)
apiVersion: agentic.openshift.io/v1alpha1
kind: Proposal
spec:
  request: "Investigate high memory usage on pod X"
  overrides:
    model: claude-opus-4-20250514  # more expensive model for complex analysis
    timeout: 600s                   # longer timeout for this specific case
```

#### How it flows through the system

1. **ProposalRequest** carries only the request + optional overrides (tiny CR)
2. **Gateway** stores the minimal spec in Postgres
3. **Operator** merges defaults from `AgenticOLSConfig` + overrides from Proposal at reconcile time
4. **task.json** (ConfigMap) contains the fully-resolved configuration — sandbox sees a complete spec regardless of where values came from

The sandbox never knows about the hierarchy — it receives a fully-resolved task.json. The merging logic lives entirely in the operator.

#### Benefits

- **Smaller proposals** — most proposals carry zero configuration (just the request)
- **Less etcd pressure** — smaller CRs = less storage per proposal
- **Centralized management** — change the default model cluster-wide by editing one CR
- **Simpler ProposalRequest CRD** — fewer fields, lower barrier for alert adapters and CLI
- **Auditable** — `AgenticOLSConfig` shows the effective defaults; proposal shows only intentional overrides

### 6. Phase Labels for Efficient Queries

The operator promotes the derived phase to a label on every Proposal CR:

```yaml
labels:
  agentic.openshift.io/phase: Analyzing
```

**Rationale:** Phase is currently derived from conditions client-side via `DerivePhase()`. Any component that needs to query proposals by state (gateway, TTL cleanup, console, CLI) must list ALL proposals and filter in-memory. This is O(n) on every query and doesn't scale.

With a phase label, Kubernetes API server handles filtering via label selectors — efficient server-side queries regardless of total proposal count.

**Who benefits:**

| Consumer | Query |
|----------|-------|
| Gateway promotion loop | `labelSelector=agentic.openshift.io/phase notin (Completed,Failed,Denied)` — count active |
| TTL cleanup | `labelSelector=agentic.openshift.io/phase in (Completed,Failed,Denied)` — find expired |
| Console / CLI | `oc get proposals -l agentic.openshift.io/phase=Executing` — filter by state |
| Operator informers | Can scope watches to specific phases if needed |

**Implementation:** The reconciler updates the label on every phase transition, in the same status patch that updates conditions. One additional field — negligible cost. Conditions remain the authoritative source of truth; the label is a projection for query efficiency.

### 7. Processing Gating

**Related Jira:** [OLS-3066](https://redhat.atlassian.net/browse/OLS-3066) (Decouple Proposal reconcile latency), [OLS-3296](https://redhat.atlassian.net/browse/OLS-3296) (Cluster-level defaults for Proposal configuration)

Two layers of protection:

| Layer | Protects | Mechanism |
|-------|----------|-----------|
| Gateway promotion rate | etcd, operator memory, API server | Only promotes when active CR count < `maxConcurrent` |
| Pod creation gate | Cluster compute, node resources | Reconciler checks active pod count before creating new pods |

Configuration in `ApprovalPolicy`:

```yaml
spec:
  concurrency:
    maxActiveProposals: 50
    maxActivePodsPerNamespace: 5
```

etcd is never flooded because the gateway holds the buffer in Postgres. The operator only ever sees `maxActiveProposals` CRs (plus terminal within TTL).

### 8. Unified Agent Runtime (Target SDK Strategy)

With execution eliminated as an LLM touch point (script replay) and verification potentially simplified, the remaining LLM usage (analysis + verification) benefits from a unified agent runtime rather than per-vendor SDK adapters.

#### The problem with multi-SDK

The current sandbox maintains three separate provider adapters (Claude/Anthropic, Gemini/ADK, OpenAI), each with:
- Divergent tool calling mechanics
- Different structured output handling (Claude `output_format`, Gemini `response_schema`, OpenAI strict mode)
- Provider-specific MCP integration (or lack thereof)
- Inconsistent skills loading
- Different RAG integration paths
- Different working directory and disk write locations per SDK
- Monkey-patches and workarounds for SDK limitations

This is N×M complexity (N providers × M features) that grows with every new provider or capability. SDK version churn adds ongoing maintenance overhead.

#### Target direction

A single framework-owned agent runtime that treats LLM providers as completion endpoints. Rather than delegating agent behavior (tool loops, structured output, retries) to vendor SDKs, the runtime handles these concerns consistently at its own layer:

| Concern | Per-SDK approach (current) | Unified runtime (target) |
|---------|---------------------------|--------------------------|
| Tool calling | Delegated to each SDK's agent loop | Framework-owned loop: call LLM → parse tool calls → execute → feed back |
| Structured output | SDK-specific mechanisms | Framework parses and validates JSON output uniformly |
| MCP integration | Each SDK handles differently (or doesn't) | Framework owns MCP client, consistent across providers |
| RAG / context | Per-provider retrieval mechanisms | Framework owns retrieval, injects context before LLM call |
| Skills | Symlinks, ADK loading, capabilities — all different | Framework loads skills uniformly, presents to LLM as tool definitions |
| Retries / error handling | SDK-specific | Framework-owned retry logic |

Provider integration reduces to: HTTP client configuration (auth mechanism, endpoint URL, model name, request/response format mapping). Everything else lives in the framework.

#### Benefits

- **No SDK version churn** — no dependency on vendor release cycles for core agent behavior
- **Consistent parity** — features work the same regardless of backend provider
- **RAG/MCP/skills integrated once** — not N times per provider
- **Simpler sandbox image** — one runtime, not three adapters with conditional imports
- **Easier new provider onboarding** — add auth config + completion API mapping, not a full adapter

#### Relationship to batch model

The batch architecture reinforces this direction:
- Execution is script replay — no LLM runtime needed at all
- Analysis and verification are the only LLM touch points — a unified runtime covers both
- The runtime's only responsibility is: given a prompt + tools + schema, produce structured JSON output
- Provider-specific concerns are isolated to "how to call the completion API" — a thin HTTP layer

### 9. RBAC Accuracy via Single-Pod Analysis

**Related Jira:** [OLS-3283](https://redhat.atlassian.net/browse/OLS-3283) (Standardize retry policies for transient failures across operator steps), [OLS-3294](https://redhat.atlassian.net/browse/OLS-3294) (MCP server connectivity and tool extensibility)

Analysis produces concrete execution plans with discovered (not predicted) RBAC requirements.

#### The problem

Today, analysis and execution are separate LLM invocations. During analysis, the agent proposes remediation options and the operator derives RBAC for execution from those proposals. However, LLMs are non-deterministic — the analysis agent is *predicting* what permissions the execution agent will need, and those predictions can be wrong:

- Analysis says "I'll patch the Deployment" → execution discovers it also needs to read Secrets to check configuration → 403 Forbidden
- Analysis lists 3 resources to modify → execution finds a 4th dependency at runtime → 403 Forbidden
- Analysis proposes one approach → execution agent reasons differently in a new session, takes a slightly different path → insufficient RBAC

The root cause: RBAC is derived from a prediction about future behavior, not from observed behavior. When the user approves an analysis option, they are implicitly approving RBAC that may be inaccurate.

#### Solution: two-phase analysis with script replay execution

Analysis becomes a two-phase process within a single pod. The LLM plans freely, then the sandbox mechanically discovers permissions by attempting the planned actions:

#### How RBAC discovery works: two-phase analysis with empirical error collection

Server-side dry-run (`kubectl apply --dry-run=server`) requires the same RBAC verbs as real writes — it just doesn't persist the result. This creates a circular dependency: we can't give analysis execution-level RBAC because we don't know what execution needs until analysis discovers it.

LLM-based reasoning about permissions ("the agent figures out what RBAC it needs") is unreliable — LLMs are non-deterministic and may miss subresources, custom resources, or MCP tool internals.

**Critical design constraint: LLM agents adapt to errors.** If the agent encounters a 403 during planning, it will reason about the failure and change course — potentially skipping subsequent actions or trying alternatives. This means we cannot simply "let the agent run and collect 403s" because the agent's behavior would diverge from the actual execution plan.

**Solution: separate the LLM session from the permission discovery.**

Analysis runs in two distinct phases within a single pod:

**Phase A — LLM-driven planning (agent active):**
1. Agent diagnoses the problem (inspect current state, logs, events)
2. Agent proposes remediation options
3. For each option, agent produces a **structured action script** — a concrete list of commands/tool calls to execute
4. Agent session ends. Output: the script.

**Phase B — Deterministic permission discovery (no LLM):**
5. The sandbox takes the action script from Phase A
6. Mechanically attempts each action against the cluster — no LLM involved
7. Every write operation fails with `403 Forbidden`
8. Collects all 403 errors without stopping
9. Parses each 403 into structured permissions

The agent never sees the 403 errors — it planned freely without interference. Permission discovery is mechanical and complete — no LLM branching or adaptation.

**Execution is script replay — no LLM.**

This two-phase design also determines the execution model: execution is not an autonomous LLM agent. It mechanically replays the approved action script. This gives:

- **Perfect RBAC accuracy** — the script IS the execution, and 403 discovery ran against that exact script
- **Full auditability** — user approves exact actions, execution does exactly that
- **Determinism** — execution is repeatable, no LLM non-determinism
- **Lower cost** — no LLM tokens during execution
- **Simpler execution sandbox** — script runner, not an LLM agent

The verification step (which IS LLM-driven) covers the adaptivity gap: if something unexpected happened during execution, verification catches it and the user can re-analyze.

| Step | LLM involved? | Purpose |
|------|--------------|---------|
| Analysis (Phase A) | Yes | Diagnose, reason, produce structured action script |
| Analysis (Phase B) | No | Mechanically run script to discover permissions via 403 collection |
| Execution | **No** | Replay the approved script |
| Verification | Yes | Check if execution achieved the desired outcome |

**Implementation: the sandbox runs in error-collection mode during Phase B.**

```python
permission_errors = []
for action in action_script:
    result = execute(action)
    if result.status == 403:
        permission_errors.append(parse_forbidden_error(result))
        continue  # don't stop — collect ALL 403s
    # Non-403 errors (404, 409, etc.) are not permission problems — ignore for RBAC
```

Only 403 Forbidden errors are collected. Other failures (404 Not Found, 409 Conflict) are not permission-related and are ignored.

**Why this works for any tool type:**

| Tool | How 403s appear |
|------|-----------------|
| kubectl commands | K8s API returns structured 403 with verb, resource, API group |
| MCP tools (backed by K8s API) | Underlying API calls fail with the same structured 403 |
| Raw HTTP calls to K8s API | Same 403 response format |

It doesn't matter whether the agent uses bash, kubectl, MCP servers, or direct API calls. If it touches the Kubernetes API, a 403 tells you exactly what permission was missing.

**The approval experience:**

User sees at approval time:
- The execution script (exact actions)
- The permissions needed (empirically discovered, not estimated)
- These are facts, not predictions

**Edge case: order-dependent scripts.**

If action 1 creates a resource and action 2 modifies it, action 1 fails with 403 (no create permission), so the resource never exists. Action 2 then fails with 404 (resource not found) rather than 403. The 404 is ignored — we only collect 403s. This means action 2's permission might not be discovered during analysis.

Mitigation: if execution hits a new 403 at runtime (rare — only in cascading-dependency scripts), the step fails with a clear permission error. The operator can expand RBAC and retry. For most remediation scripts (patch X, restart Y, scale Z), actions are independent and all 403s are discovered in one pass.

**Comparison with alternatives:**

| Approach | Reliability | Tool-agnostic | Complexity |
|----------|-------------|---------------|------------|
| LLM reasons about RBAC | Low (non-deterministic) | No (needs command parsing) | Low |
| MCP permission declarations | High (but manual config) | No (MCP spec doesn't support it) | Medium |
| Pre-configured agent roles | High (but coarse) | Yes | Low |
| **Empirical 403 collection** | **High (direct observation)** | **Yes (any K8s API call)** | **Medium** |

The result: RBAC in the analysis output is *empirically discovered* — the same script that will run during execution was already attempted during analysis, and the API told us exactly what it needs.

#### Analysis output (per option)

```json
{
  "title": "Update image to v1.3",
  "diagnosis": "Deployment foo is running image v1.2 with known CVE-2026-1234",
  "actions": [
    "kubectl set image deployment/foo container=registry/foo:v1.3 -n production",
    "kubectl rollout status deployment/foo -n production"
  ],
  "requiredPermissions": [
    {"apiGroup": "apps", "resource": "deployments", "verbs": ["get", "patch"]},
    {"apiGroup": "", "resource": "pods", "verbs": ["get", "list"]}
  ],
  "dryRunValidated": true,
  "risk": "low"
}
```

#### Approval experience

When the user approves an option, they are approving based on facts, not predictions:

- **Concrete actions** — exact commands that will be executed (validated via dry-run)
- **Exact RBAC** — permissions discovered by the agent during dry-run, not estimated
- **Risk level** — informed by actual dry-run validation

User approves one option → operator creates execution RBAC from `requiredPermissions` → execution pod runs with accurate, user-approved permissions. RBAC mismatches are eliminated in the happy path because the same agent session that planned the actions also discovered the permissions.

#### No solution is 100% — recovery from RBAC failures

Even with empirical 403 collection, RBAC accuracy cannot be guaranteed in all cases:

- Order-dependent scripts where cascading failures hide permissions (action 2 depends on action 1's output)
- Admission webhooks (OPA, pod security) that reject operations for non-RBAC reasons
- Time-dependent state changes between analysis and execution (resource modified by another actor)
- MCP tools with internal logic that branches differently based on runtime state

The system must allow users to **analyze failed execution runs, adjust parameters (including RBAC), and re-run**:

1. **Failed execution produces a detailed error report** — the Result CR captures the exact 403 or admission error, the action that triggered it, and the permissions that were granted vs needed.
2. **User can modify RBAC and retry** — via CLI (`oc agentic proposal retry --expand-rbac`) or console, the user adds missing permissions and re-triggers execution without re-running analysis.
3. **Re-submission via gateway** — alternatively, the user re-submits the proposal through the gateway. A new analysis runs with updated context (`previousAttempts` includes the failure reason), allowing the agent to account for the permission gap.

This is not a failure of the design — it's an acknowledgment that automated systems operating in dynamic environments will occasionally encounter conditions that weren't observable during planning. The architecture must make these failures **visible, diagnosable, and recoverable** rather than trying to prevent them entirely.

#### Option constraints

Users can only select from options produced by analysis. No free-form alternatives. This guarantees RBAC accuracy — the dry-run that discovered the permissions is the same plan that will execute.

#### Pod count (happy path)

| Pod | LLM? | Access | Purpose |
|-----|------|--------|---------|
| Analysis | Yes | Read-only | Diagnose + plan + discover RBAC (two-phase) |
| Execution | **No** | Read-write (user-approved RBAC) | Replay approved action script |
| Verification | Yes | Read-only | Validate execution results |

Three pods maximum per proposal lifecycle. Execution pod is a simple script runner — no LLM provider needed, lower cost, deterministic behavior.

## System Topology (Target State)

```
┌─────────────────────────────────────────────────────────────────────┐
│                          User Interface                              │
│  Console (watches CRs)  │  CLI (oc agentic)  │  Alerts adapter      │
└─────────────────────────┬───────────────────────────────────────────┘
                          │ creates ProposalRequest CRs
                          ▼
┌─────────────────────────────────────────────────────────────────────┐
│  Proposal Gateway (controller)                                      │
│  - Watches ProposalRequest CRs                                      │
│  - Stores in Postgres, deletes CR immediately                       │
│  - Promotes to Proposal CRs on timer when capacity available        │
│  - Auth: standard K8s RBAC                                          │
└─────────────────────────┬───────────────────────────────────────────┘
                          │ creates Proposal CRs
                          ▼
┌─────────────────────────────────────────────────────────────────────┐
│  Operator (1 replica)                                               │
│  - Watches Proposal CRs + owned Pods                                │
│  - Creates batch pods (ConfigMap in/out)                             │
│  - Processes results on pod completion (informer-driven)             │
│  - TTL cleanup of terminal proposals                                 │
│  - Never blocks on external calls                                    │
└─────────────────────────┬───────────────────────────────────────────┘
                          │ creates pods + ConfigMaps
                          ▼
┌─────────────────────────────────────────────────────────────────────┐
│  Sandbox Pod (batch, ephemeral)                                     │
│  - Reads task.json from input ConfigMap                              │
│  - Runs agent (single LLM session with tools)                       │
│  - Writes result.json to output ConfigMap                            │
│  - Exits                                                             │
└─────────────────────────────────────────────────────────────────────┘
```

## Migration Path

TBD — to be determined based on release timeline. Options:

1. **Incremental:** Operator supports both modes (HTTP + batch) behind a flag during transition period.
2. **Big bang:** Single release cuts over. Requires coordinated sandbox image + operator deployment.
3. **Gateway optional initially:** Operator still accepts direct CR creation. Gateway is additive, not mandatory on day 1.

## Jira Traceability

### Directly addressed by this design

| Jira | Title | Spec section |
|------|-------|-------------|
| [OLS-3284](https://redhat.atlassian.net/browse/OLS-3284) | Run-to-completion agent model with file-based I/O | §1 Batch Execution |
| [OLS-3066](https://redhat.atlassian.net/browse/OLS-3066) | Decouple Proposal reconcile latency from sandbox management and agent HTTP | §1 Batch Execution, §5 Processing Gating |
| [OLS-3264](https://redhat.atlassian.net/browse/OLS-3264) | Release sandbox pods immediately after step completion | §1 Batch Execution (pods are ephemeral by design) |
| [OLS-3279](https://redhat.atlassian.net/browse/OLS-3279) | Durable state store for agent step outputs | §3 Proposal Gateway (Postgres as durable store) |
| [OLS-3278](https://redhat.atlassian.net/browse/OLS-3278) | Proposal lifecycle management: TTL-based cleanup and failed recovery | §4 Lifecycle Management |
| [OLS-3296](https://redhat.atlassian.net/browse/OLS-3296) | Cluster-level defaults for Proposal configuration | §3, §5 (gateway gating config in ApprovalPolicy) |
| [OLS-3283](https://redhat.atlassian.net/browse/OLS-3283) | Standardize retry policies for transient failures | §6 RBAC recovery (retry on 403 failure) |
| [OLS-3018](https://redhat.atlassian.net/browse/OLS-3018) | Kill Switch for Agentic Capabilities | §3 Gateway kill switch (Problem 3) |
| [OLS-3294](https://redhat.atlassian.net/browse/OLS-3294) | MCP server connectivity and tool extensibility | §6 RBAC (MCP tools and permission discovery) |

### Additionally enabled or simplified by this design

| Jira | Title | How this design helps |
|------|-------|----------------------|
| [OLS-3236](https://redhat.atlassian.net/browse/OLS-3236) | Deploy agentic-alerts-adapter as a pod | Alerts adapter becomes a gateway client — creates ProposalRequest CRs instead of Proposal CRs directly |
| [OLS-2986](https://redhat.atlassian.net/browse/OLS-2986) | Agentic Data Collection & Observability | Metrics flow naturally through output ConfigMap (sandbox writes metrics in result.json). Gateway can log submission/promotion events to Postgres for analytics. |
| [OLS-3300](https://redhat.atlassian.net/browse/OLS-3300) | Adversarial testing suite | Batch model simplifies adversarial testing — test harness can inspect input/output ConfigMaps directly, verify pod behavior without HTTP mocking |
| [OLS-3259](https://redhat.atlassian.net/browse/OLS-3259) | AG-UI protocol for agentic chat | Gateway's queue read endpoint could serve as the backend for AG-UI streaming (queue state, promotion/completion events) |
| [OLS-3443](https://redhat.atlassian.net/browse/OLS-3443) | MCP server connectivity for agentic sandbox | Batch model doesn't change MCP connectivity — MCP servers still run as sidecars or network services reachable from the pod |

### Not addressed (out of scope)

| Jira | Title | Why |
|------|-------|-----|
| [OLS-3347](https://redhat.atlassian.net/browse/OLS-3347) | Agentic operand deployment via lightspeed-operator | OLM packaging — orthogonal to runtime architecture |
| [OLS-3187](https://redhat.atlassian.net/browse/OLS-3187) | OLM bundle composition and CI/CD | Build/release concern |
| [OLS-3032](https://redhat.atlassian.net/browse/OLS-3032) | Deploy Agentic Chat as Dedicated Pod | Chat UI — separate workload |
| [OLS-3063](https://redhat.atlassian.net/browse/OLS-3063) | OpenStack Lightspeed Integration | Cross-platform — builds on this architecture but doesn't change it |

## Open Items

- [ ] ProposalRequest CRD schema definition
- [ ] task.json v1 full schema definition (JSON Schema)
- [ ] result.json v1 full schema definition
- [ ] Verification step specifics — does it need cluster access or just result inspection?
- [ ] Gateway deployment model (same namespace as operator? separate?)
- [ ] Postgres provisioning — operator-managed or external dependency?
- [ ] Metrics/monitoring for gateway (queue depth, promotion latency, rejection rate)
- [ ] Migration strategy selection and timeline
