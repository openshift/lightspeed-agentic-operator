# Behavioral Specifications (what/)

These specs define WHAT the operator must do -- testable behavioral rules, configuration surface, constraints, and planned changes. They are technology-neutral where possible and survive a complete rewrite.

## Spec Index

| Spec | Description |
|------|-------------|
| [proposal-lifecycle.md](proposal-lifecycle.md) | Proposal phases, condition-driven state machine, retry logic, revision, escalation |
| [crd-api.md](crd-api.md) | All CRD types and field semantics: Proposal, Agent, LLMProvider, ApprovalPolicy, result CRs |
| [approval.md](approval.md) | Human-in-the-loop approval system: policy modes, stage gates, deny semantics |
| [sandbox-execution.md](sandbox-execution.md) | Sandbox pod lifecycle, RBAC scoping, agent communication, skills mounting |

## Relationship to how/ Specs

These `what/` specs define the behavioral contract. The [`how/` specs](../how/README.md) describe the current Go implementation. Read `what/` to understand requirements, read `how/` to understand the codebase structure.
