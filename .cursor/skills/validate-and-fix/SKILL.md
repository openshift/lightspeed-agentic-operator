---
name: validate-and-fix
description: Run the validation pipeline for this repo (go test, go vet, API kube linter) and auto-fix trivial failures like formatting and import issues. Use when the user asks to validate, run tests, check the pipeline, or verify changes are clean.
disable-model-invocation: true
---

# Validate & Auto-Fix

Run the project validation pipeline, auto-fix trivial issues, and re-run until green or a real failure is found.

## Rules

- This repository has **no root Makefile** for tests; use `go test ./...` from the repo root (not operator Makefile conventions from other repos).
- Never modify production logic to fix a test. Only fix test expectations, imports, formatting.
- Never skip or delete a failing test.
- Stop after 3 auto-fix cycles to avoid loops.
- Report real failures clearly; do not attempt speculative fixes.

## Step 1: Run Unit Tests

```bash
go test ./... -count=1 2>&1 | tail -80
```

If all pass, proceed to Step 3 (vet / fmt).
If failures occur, classify each failure (see Step 2).

## Step 2: Classify and Fix Failures

For each failure, determine its type:

**Auto-fixable** (fix immediately, then re-run Step 1):

| Type | Fix |
|------|-----|
| Test expects old constant value | Update assertion to match new value |
| Test uses renamed function | Update function name in test |
| Import error from refactor | Update import path |
| Missing cleanup in test | Add cleanup or use existing cleanup helpers |

**Real failures** (do not auto-fix):

- Logic errors in production code
- Assertion failures reflecting actual behavior regressions
- Reconciliation loop failures
- Context cancellation issues
- Failures in code you did not modify

For real failures: report the test name, file, error message, and stop.

## Step 3: go vet and formatting

```bash
go vet ./... 2>&1 | tail -40
go fmt ./...
```

Fix straightforward vet issues (unused vars in tests, etc.) and re-run.

## Step 4: API module — kube API linter (when API types changed)

If files under `api/v1alpha1/` (or other `api/` packages) were touched, run the custom kube API linter from the repo root:

```bash
golangci-lint custom
cd api && GOWORK=off ../bin/golangci-lint-kube-api-linter run --config ../.golangci-kal.yml ./...
```

See `CLAUDE.md` in the repo for details. Fix reported issues or stop if they need design discussion.

## Step 5: CRD YAML (canonical in lightspeed-operator)

If API types or kubebuilder markers under `api/v1alpha1/` changed, **do not hand-edit** CRD YAML here. Regenerate from the **lightspeed-operator** repo:

```bash
cd ../lightspeed-operator
make manifests-agentic
```

Then commit the updated YAML in lightspeed-operator (and any local copies in this repo if the workflow requires it).

## Step 6: Report

Report exactly:

- `go test ./... -count=1`: X passed / Y failed
  - List any failing tests with brief error summary
- `go vet`: pass/fail
- Kube API linter: pass/fail (if run)
- Auto-fixes applied:
  - File: what was fixed
- Cycles used: N/3

If all green:

```
✅ All validation passed:
  - go test: all packages passing
  - go vet: no issues

Ready to commit or push.
```

Do not include unrelated diagnostics or suggestions.
