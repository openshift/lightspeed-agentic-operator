# Architecture Specifications (how/)

These specs describe HOW the operator is structured -- module boundaries, data flow, design patterns, key abstractions, and implementation decisions. They are grounded in the current Go/kubebuilder codebase and should be updated when the code changes.

## Spec Index

| Spec | Description |
|------|-------------|
| [reconciler.md](reconciler.md) | Controller package: reconciler, handler dispatch, sandbox management, agent communication, templates |
| [cli.md](cli.md) | oc-agentic CLI plugin: command tree, K8s API calls, watch streaming, output formatting |

## Relationship to what/ Specs

The [`what/` specs](../what/README.md) define behavioral contracts (technology-neutral). These `how/` specs describe the implementation that fulfills those contracts. When the two diverge, the `what/` spec is the source of truth for correct behavior.
