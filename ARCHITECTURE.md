# Architecture

The lightspeed-agentic-operator is a Kubernetes operator built with controller-runtime that orchestrates AI-assisted change proposals on OpenShift clusters. It watches `Proposal` custom resources and drives each through a multi-phase workflow -- analysis, execution, and verification -- with configurable human approval gates between phases. Each phase invokes an LLM-backed agent running inside an ephemeral sandbox pod.

## System Components

```mermaid
graph TB
    subgraph "Operator Binary"
        MC[cmd/main.go]
        PC[Proposal Controller]
        CC[Console Plugin Deployer]
        MC --> PC
        MC --> CC
    end

    subgraph "CLI Binary"
        CLI[oc-agentic]
    end

    subgraph "Kubernetes API Server"
        P[Proposal CR]
        PA[ProposalApproval CR]
        AG[Agent CR]
        LP[LLMProvider CR]
        AP[ApprovalPolicy CR]
        RES[Result CRs]
        SC[SandboxClaim]
    end

    subgraph "Sandbox Runtime"
        SB[Sandbox Pod]
        AGENT[Agent HTTP Server]
        SB --> AGENT
    end

    subgraph "OpenShift Console"
        CP[ConsolePlugin CR]
        UI[Agentic Console UI]
    end

    PC -->|watches/reconciles| P
    PC -->|creates/reads| PA
    PC -->|reads| AG
    PC -->|reads| LP
    PC -->|reads| AP
    PC -->|creates| RES
    PC -->|creates/deletes| SC
    PC -->|POST /v1/agent/run| AGENT
    CC -->|deploys| CP
    CLI -->|CRUD| P
    CLI -->|patch| PA
    CLI -->|watch/logs| P
```

## Proposal Data Flow

A `Proposal` moves through phases driven by conditions on its status. The phase is derived (never stored) from the condition set using `DerivePhase()`.

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Analyzing: analysis gate opens
    Analyzing --> Proposed: analysis succeeds
    Analyzing --> Failed: analysis fails
    Proposed --> Executing: execution approval
    Proposed --> Denied: user denies
    Executing --> Verifying: execution succeeds
    Executing --> Failed: execution fails
    Verifying --> Completed: verification passes
    Verifying --> Executing: verification fails (retry)
    Verifying --> Escalating: retries exhausted
    Verifying --> Failed: verification fails (no retry)
    Escalating --> Escalated: escalation completes
    Escalating --> Failed: escalation fails
```

Each step (analysis, execution, verification, escalation) follows the same pattern:

1. Check approval gate (automatic or manual via `ProposalApproval`)
2. Ensure a derived `SandboxTemplate` with LLM credentials and tools
3. Create a `SandboxClaim` to provision an ephemeral sandbox pod
4. Wait for sandbox readiness (`Ready=True` condition)
5. `POST /v1/agent/run` with step-specific query and output schema
6. Record the result in a typed Result CR (`AnalysisResult`, `ExecutionResult`, etc.)
7. Update proposal conditions and status

## Module Organization

The codebase splits into two Go modules:

- **Root module** (`go.mod`) -- the operator binary, CLI binary, controller logic, and all runtime dependencies. Uses `replace => ./api` for local development.
- **API module** (`api/go.mod`) -- CRD type definitions only. Published separately so downstream projects (console, CLI, other operators) can import types without pulling in controller-runtime or other operator dependencies.

```mermaid
graph LR
    subgraph "Root Module"
        CMD[cmd/main.go]
        CTRL[controller/]
        CLIP[cli/]
    end

    subgraph "API Module"
        API[api/v1alpha1/]
    end

    CMD --> CTRL
    CMD --> API
    CTRL --> API
    CLIP --> API
    EXT[External consumers] --> API
```

## Deployment Topology

The operator runs as a single-replica Deployment in a designated namespace (e.g., `openshift-lightspeed`). Sandbox pods also run in this namespace, not in tenant namespaces. Proposals are namespace-scoped and created in workload namespaces.

```mermaid
graph TB
    subgraph "Operator Namespace"
        OP[Operator Pod]
        SB1[Sandbox Pod - analysis]
        SB2[Sandbox Pod - execution]
    end

    subgraph "Workload Namespace A"
        P1[Proposal]
        PA1[ProposalApproval]
        AR1[AnalysisResult]
        ROLE1[Execution Role]
    end

    subgraph "Cluster-Scoped"
        AG1[Agent: default]
        LP1[LLMProvider: anthropic]
        APOL[ApprovalPolicy: cluster]
    end

    OP -->|reconciles| P1
    OP -->|creates in operator ns| SB1
    OP -->|creates in operator ns| SB2
    OP -->|creates in workload ns| ROLE1
    OP -->|reads| AG1
    OP -->|reads| LP1
    OP -->|reads| APOL
```

## Key Architectural Decisions

**Condition-driven state machine.** Proposal phase is derived from `status.conditions` via a pure function (`DerivePhase`), not stored as a separate field. This ensures the phase label is always consistent with the actual condition state and can be recomputed by any consumer (controller, CLI, console) without drift.

**Sandbox isolation.** Each workflow step runs in an ephemeral sandbox pod provisioned through the Sandbox API (`SandboxClaim` / `SandboxTemplate`). The operator creates derived templates by cloning a base template and patching in LLM credentials, tools, and step configuration. Templates are named by content hash for deduplication.

**Dual approval model.** `ApprovalPolicy` (cluster singleton) defines default automatic/manual gates. `ProposalApproval` (per-proposal) carries user decisions, option selection, and agent overrides. The combined gate function checks both: a step is approved if the policy says `Automatic` OR the approval has a non-denied entry.

**Create-only idempotency.** Child resources (ProposalApproval, Result CRs, RBAC objects) use `Create` + handle `AlreadyExists` rather than `Get`-then-`Create`, avoiding read-modify-write race conditions.

**Separate API module.** The `api/` directory is its own Go module so downstream projects can depend on CRD types without importing controller-runtime or the full operator dependency tree.
