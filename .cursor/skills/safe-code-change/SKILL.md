---
name: safe-code-change
description: After a code change, find affected tests, update them to match new behavior, then guide the user to run validation. Use when the user has made or asked for a code change and wants to make sure nothing is broken.
disable-model-invocation: true
---

# Safe Code Change

After a code change is made, find and fix affected tests before running validation.

## Rules

- The code change is already done. Do not modify production code.
- Only update tests to match the new behavior, not the other way around.
- Do not reformat or lint-fix during test updates. Save that for validation.
- If a test change is ambiguous (unclear what the new expected behavior is), ask the user.

## Step 1: Identify What Changed

```bash
git diff --name-only
git diff --stat
```

List the modified production files (ignore test files, configs, docs).

## Step 2: Find Affected Tests

Search for imports and uses of changed functions/types across all test files:

```bash
# Find test files that import the changed package
rg "github.com/openshift/lightspeed-agentic-operator/<changed_package>" --type go -g '*_test.go'

# Find direct function/type references
rg "<ChangedFunctionOrType>" --type go -g '*_test.go'
```

For controller or CLI changes, also check:
- Shared test helpers or fixtures under the same package
- `examples/` YAMLs only if behavior affects documented shapes (usually not test code)

## Step 3: Analyze Impact on Tests

For each affected test file, check whether the change breaks existing tests:

1. **Signature changes** — function renamed, parameters added/removed/reordered.
2. **Behavior changes** — return value, error messages, side effects differ.
3. **Removed code** — tests for deleted functions/types need removal.
4. **New code** — consider whether new tests are needed (ask user if unclear).
5. **Interface changes** — mock implementations need updating.

## Step 4: Update Tests

Apply minimal fixes to each affected test:

### For Ginkgo Tests (if present):

- Update `Expect()` assertions to match new return values
- Update mock return values in test fixtures
- Add/remove parameters in function calls
- Update error message checks
- Adjust `Eventually()` timeouts if reconciliation logic changed

### For Standard Go Tests (typical in this repo):

- Update table-driven test cases with new expected values
- Update mock implementations
- Add/remove parameters in function calls
- Update error assertions

### Common Fixes:

- **Error constant renamed**: Update all `Expect(err).To(MatchError(ContainSubstring(oldName)))` → `newName`
- **Function signature changed**: Update all call sites in tests
- **Resource structure changed**: Update test fixtures and expected values
- **Owner reference logic changed**: Update assertions that check `OwnerReferences`

## Step 5: Verify Test File Syntax

Before telling the user tests are ready, verify Go syntax:

```bash
go fmt <modified_test_file>
```

If formatting changes the file significantly, there may be syntax errors.

## Step 6: Report

List all test files updated and what was changed in each:

1. File name
2. What was updated (function calls, expectations, mocks, fixtures)
3. Number of changes

Then guide the user to run validation:

```
Tests are updated. Run validation with:
  go test ./... -count=1    # All unit tests (see repo CLAUDE.md for focused paths)
  go vet ./...              # Static checks
  go fmt ./...              # Formatting

If tests fail, review the specific failures and adjust expectations.
```

Do not run the full test suite automatically unless the user asked — they may prefer to run it themselves.
