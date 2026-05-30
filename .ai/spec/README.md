# Lightspeed Agentic Operator -- Specifications

These specs define the requirements, behaviors, and architecture for the lightspeed-agentic-operator. They are organized into two layers:

- **[`what/`](what/)** -- Behavioral rules: WHAT the system must do. Technology-neutral, testable assertions. Use these to understand requirements, fix bugs, or rebuild components.
- **[`how/`](how/)** -- Architecture specs: HOW the current implementation is structured. Module boundaries, data flow, design patterns. Use these to navigate, modify, and extend the codebase.

## Scope

These specs cover the **lightspeed-agentic-operator** Go/kubebuilder application only. The sandbox runtime, console plugin, and skills packaging are separate projects with their own specs.

## Audience

AI agents (Claude). Specs optimize for precision, unambiguous rules, and machine-parseable structure.

## Quick Start

| I want to... | Read |
|--------------|------|
| Understand the proposal workflow | `what/proposal-lifecycle.md` |
| Look up a CRD field | `what/crd-api.md` |
| Understand the approval system | `what/approval.md` |
| Understand sandbox pod lifecycle | `what/sandbox-execution.md` |
| Understand the kill switch / system config | `what/system-config.md` |
| Navigate the controller codebase | `how/reconciler.md` |
| Understand the CLI plugin | `how/cli.md` |

## Conventions

- `[PLANNED: OLS-XXXX]` markers indicate existing rules about to change due to open Jira work
- "Planned Changes" sections list new capabilities not yet in code
- CRD field names reference `spec.*` and `status.*` paths
- Internal constants are stated as behavioral rules without numeric values

## Project Context

This operator watches `Proposal` CRs and drives them through a multi-phase workflow (analysis, execution, verification) by calling the sandbox runtime's `POST /v1/agent/run` endpoint. The console plugin provides the human-facing UI. Skills are mounted as OCI image volumes.

Jira tracking: Feature OCPSTRAT-3095, Epic OLS-2894, Kill Switch OLS-3018.
