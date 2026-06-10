# Lightspeed Agentic Operator -- Specifications

Behavioral rules, architecture specs, and implementation guides for the lightspeed-agentic-operator -- a Kubernetes operator that drives AI-assisted `Proposal` workflows through analysis, execution, and verification phases with human approval gates.

## Structure

| Layer | Path | Purpose |
|---|---|---|
| **what/** | `.ai/spec/what/` | Behavioral rules. What the system must do. Implementation-agnostic. |
| **how/** | `.ai/spec/how/` | Codebase navigation. How the code is organized. Implementation-specific. |

## Scope

These specs cover the **lightspeed-agentic-operator** Go/kubebuilder application only. The sandbox runtime, console plugin frontend, and skills packaging are separate projects with their own specs.

## Audience

AI agents. Content is optimized for precision and machine consumption.

## Quick Start

| Task | Start here |
|---|---|
| Understand the system | `what/system-overview.md` |
| Understand the proposal workflow | `what/proposal-lifecycle.md` |
| Look up a CRD field | `what/crd-api.md` |
| Understand the approval system | `what/approval.md` |
| Understand sandbox pod lifecycle | `what/sandbox-execution.md` |
| Navigate the project layout | `how/project-structure.md` |
| Understand the kill switch / system config | `what/system-config.md` |
| Navigate the controller codebase | `how/reconciler.md` |
| Understand the CLI plugin | `how/cli.md` |

## Cross-Reference

| what/ | how/ |
|---|---|
| `what/system-overview.md` | `how/project-structure.md` |
| `what/proposal-lifecycle.md` | `how/reconciler.md` |
| `what/crd-api.md` | `how/reconciler.md`, `how/cli.md` |
| `what/approval.md` | `how/reconciler.md`, `how/cli.md` |
| `what/sandbox-execution.md` | `how/reconciler.md` |

## Conventions

- **Rule numbering:** behavioral rules are numbered sequentially within each what/ file.
- **Planned changes:** unimplemented behavior is marked with `[PLANNED]` or `[PLANNED: OLS-XXXX]` inline next to the rule it affects.
- **Constraints:** component-specific and cross-cutting constraints go in the relevant what/ file's Constraints section, co-located with behavioral rules.
- **Authority:** what/ specs are authoritative for behavior. how/ specs are authoritative for implementation. When they conflict, what/ wins.
- **When to create a new file vs. extend an existing one:** if the new concern has its own lifecycle, configuration surface, and can be understood independently, it gets its own file. If it's a capability added to an existing component, it goes in that component's file.
- CRD field names reference `spec.*` and `status.*` paths.
- Internal constants are stated as behavioral rules without numeric values.

## Project Context

This operator watches `Proposal` CRs and drives them through a multi-phase workflow (analysis, execution, verification) by calling the sandbox runtime's `POST /v1/agent/run` endpoint. The console plugin provides the human-facing UI. Skills are mounted as OCI image volumes.

Jira tracking: Feature OCPSTRAT-3095, Epic OLS-2894, Kill Switch OLS-3018.
