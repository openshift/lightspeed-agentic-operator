# Verification Report: lightspeed-agentic-operator Spec
Verified: 2026-07-23
Spec root: /Users/xavi/street/github.com/AI/ols/lightspeed-agentic-operator/.ai/spec/

## Summary
- 3 broken or inaccurate internal references (+ 1 sub-item)
- 3 internal inconsistencies
- 4 completeness gaps
- 3 cross-repo alignment issues

## Reference Issues

**R1. `crd-api.md` duplicate rule 22.**
Two rules are both numbered 22: "LLMProvider — endpoints" and "ApprovalPolicy — singleton name". The second should be 23, shifting all subsequent rule numbers by 1.

**R2. `system-config.md` stale `[PLANNED: OLS-3295]`.**
Line 90 is marked `[PLANNED: OLS-3295]` but OLS-3295 is marked `[DONE]` in both `run-lifecycle.md` (line 74) and `crd-api.md` (line 101). Additionally, the text reads "Rename `AgenticRun` references to `AgenticRun`" — a no-op sentence, clearly intended to say "Rename `Proposal` references to `AgenticRun`". Should be `[DONE: OLS-3295]`.

**R3. `sandbox-execution.md` stale `[PLANNED: OLS-3295]`.**
Line 99 is marked `[PLANNED: OLS-3295]` about renaming the per-run ServiceAccount, but rule 21 in the same file already uses the new naming (`ls-exec-{run-namespace}-{run-name}`). Should be `[DONE: OLS-3295]`.

**R3a. `run-lifecycle.md` stale `[PLANNED: OLS-3018]`.**
Line 72 marked `[PLANNED: OLS-3018]` saying "EmergencyStopped phase and condition type added to run lifecycle," but EmergencyStopped is already fully specified in the rules body of both `run-lifecycle.md` (rules 2, 4, 9, 10) and `system-config.md`. Should be `[DONE: OLS-3018]`.

## Internal Inconsistencies

**I1. Per-run SA owner reference contradiction.**
`sandbox-execution.md` rule 21: "The per-run SA MUST NOT carry an owner reference — cross-namespace owner refs are unsupported by Kubernetes GC."
`agentic-security.md` (parent spec) rule 12: "The per-run SA MUST carry an owner reference to the AgenticRun CR (Controller: true, BlockOwnerDeletion: true)."
These directly contradict. The operator spec is technically correct (cross-namespace owner refs are silently ignored by K8s GC), so the parent spec is wrong. The operator spec should explicitly note the deviation from the parent and why.

**I2. Templog finalizer mechanism contradiction.**
`audit-logging.md` rule 34: the finalizer "does not depend on the Collector being present — it connects directly to PostgreSQL."
`templog.md` rule 14a: calls "the Collector admin API: `DELETE /api/v1/logs`." Rule 15: "The finalizer depends on the Collector admin API being reachable."
These contradict on whether the finalizer uses direct PostgreSQL or the Collector admin API. `templog.md` is more detailed and likely correct; `audit-logging.md` rule 34 is stale.

**I3. `audit-logging.md` `spec.audit` field not in `crd-api.md`.**
`audit-logging.md` rules 23-24 reference `spec.audit.enabled` on `AgenticOLSConfig` as a field controlling audit emission. `crd-api.md` (authoritative CRD field spec) does not list `spec.audit` anywhere — it only documents `spec.suspended` and `spec.templog`. Either `crd-api.md` is missing the field or `audit-logging.md` is specifying a non-existent field without a `[PLANNED]` marker.

## Completeness Gaps

**G1. No `glossary.md`.**
Terms like "sandbox," "phase," "step," "terminal," "claim," "bare-pod mode" vs "sandbox-claim mode" have specific meanings but no glossary definition.

**G2. No ADR files.**
`decisions/README.md` skeleton exists but no actual decision records. Decisions on per-run SA naming, sandbox mode duality, dual-module structure, and approval singleton pattern would benefit from captured rationale.

**G3. Console plugin behavioral rules missing.**
`controller/console/` deploys a console plugin but no what/ spec defines behavioral rules for this component. Only documented in `how/reconciler.md`. (OLS-3236 plans to remove this, so the gap may be moot.)

**G4. `how/project-structure.md` stale flags.**
Line 31 lists `--agentic-console-image` as a cmd/main.go flag but does not list `--sandbox-mode`, which is active per `what/system-overview.md` and `how/reconciler.md`.

## Cross-Repo Alignment Issues

**X1. Parent `agentic-security.md` rule 12 vs operator `sandbox-execution.md` rule 21 — SA owner reference conflict.**
Parent requires per-run SA owner references; operator prohibits them due to cross-namespace GC limitations. Parent spec should be updated to match the operator's position, or the operator spec should document an exemption.

**X2. Parent `agentic-security.md` rule 13 incomplete terminal phase list.**
Lists SA cleanup terminal phases as "Completed, Denied, Escalated" — omits `EmergencyStopped`, `Failed`, and `NoActionRequired`. The operator correctly includes `EmergencyStopped` in cleanup paths (`sandbox-execution.md` rule 24, `system-config.md` rule 14).

**X3. Parent `agentic-runs.md` stale planned markers.**
OLS-3295 (Proposal rename) still listed as planned in parent, but marked `[DONE]` in operator's `run-lifecycle.md` and `crd-api.md`. OLS-3268 (NoActionRequired) listed as planned in parent but fully specified in operator's DerivePhase algorithm.

## Files Checked

### what/ (8 files)
- system-overview.md, crd-api.md, run-lifecycle.md, approval.md, audit-logging.md, sandbox-execution.md, system-config.md, templog.md

### how/ (4 files)
- project-structure.md, reconciler.md, cli.md, cli-distribution.md

### Other
- README.md, health-report.md, operator-self-review-checklist.md, decisions/README.md (no ADRs)

### Cross-repo
- /Users/xavi/street/github.com/AI/ols/.ai/spec/what/agentic-runs.md
- /Users/xavi/street/github.com/AI/ols/.ai/spec/what/agentic-security.md
