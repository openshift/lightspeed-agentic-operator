# Spec health report

Last evaluated: 2026-05-30
Trigger: post-milestone: spec-first:init alignment
Layout: software (.ai/spec/)

## Stale

1. **how/reconciler.md line 61** — Console integration note says `EnsureAgenticConsole` is "not registered in `cmd/main.go` in this repo snapshot; another binary or future setup is expected to call it." This is stale: `controller/setup.go` registers it as a `manager.RunnableFunc` called from `cmd/main.go` via `controller.Setup()`. The note should be updated to reflect the current wiring.

2. **what/crd-api.md rule 18** — States "Agent — `status.conditions`: Observed readiness; `Ready` condition documents whether referenced provider resources are accessible (see operator reconcile behavior)." No Agent reconciler exists in the codebase; the operator only reconciles `Proposal` CRs. Rule 18 should be marked `[PLANNED]` or reworded to clarify this is aspirational rather than implemented behavior.

## Missing

1. **Console plugin behavioral rules** — `controller/console/` deploys a console plugin (Deployment, Service, ConfigMap, ConsolePlugin CR, Console activation), but no what/ file defines behavioral rules for this component. It is only documented in how/reconciler.md as implementation detail. Consider adding a `what/console-plugin.md` if the console deployment has rules worth specifying (idempotency, image absence handling, activation semantics).

2. **how/reconciler.md `cmd/main.go` section** — References outdated flag set. The spec lists `template-name` as a flag; the actual code has `--agentic-console-image` and `--agentic-sandbox-image` instead. The `SandboxAgentCaller` constructor description also mentions `BaseTemplateName` which is no longer a constructor parameter in the current code.

## Structural concerns

1. **how/reconciler.md size** — At 180 lines, this file covers both `controller/proposal/` (large) and `controller/console/` (small). This is acceptable given the console section is only ~10 lines, but if the console component grows it should be split into `how/console.md`.

2. **how/reconciler.md entry point section** — The `cmd/main.go` description (lines 9-15) partially duplicates the new `how/project-structure.md` entry points section. The reconciler.md section should reference project-structure.md for the main binary and focus only on the controller setup flow.

## Findability issues

None. The cross-reference table in README.md provides clear mapping between what/ and how/ files. The quick-start table covers all common entry points.

## No issues

- All 9 spec files have real content (no empty templates or placeholders).
- All `controller/proposal/` source files listed in how/reconciler.md module map exist on disk.
- All `cli/proposal/` source files listed in how/cli.md module map exist on disk.
- All template files (`*.tmpl`) listed in how/reconciler.md exist.
- All CRD types in `api/v1alpha1/*_types.go` are covered by what/crd-api.md.
- Behavioral rules are numbered sequentially in all what/ files.
- `[PLANNED: OLS-XXXX]` markers are used consistently across all what/ files.
- Constraints sections present in all what/ files.
- `CLAUDE.md` has spec pointer.
- `ARCHITECTURE.md` exists at project root.
- Layer READMEs removed; content absorbed into main README.
