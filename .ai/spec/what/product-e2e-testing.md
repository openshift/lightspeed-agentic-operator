# Product E2E testing

Behavioral specification for **product-level end-to-end tests** that exercise
the full AgenticRun lifecycle against real OpenShift clusters with real LLM
providers. Distinct from `test/e2e/` mock-agent tests (`make test-e2e`) which
validate operator logic in isolation.

## Relationship to other specs

| Spec | Relationship |
|------|-------------|
| [run-lifecycle.md](run-lifecycle.md) | Product-e2e validates the phase transitions defined there |
| [sandbox-execution.md](sandbox-execution.md) | Product-e2e exercises the sandbox wiring end-to-end |
| [approval.md](approval.md) | Tests use automatic approval policies |

## Existing product-e2e (`make product-e2e`)

`scripts/e2e-cluster.sh` deploys the operator, iterates over providers (claude,
gemini, openai), creates real LLM fixtures, and runs `make test-e2e` per
provider. This exercises the mock-agent Go e2e tests with real provider
credentials.

## Troubleshooting scenario tests (OLS-3739)

### Scope

- **In scope:** Phase transition verification for AgenticRun CRs created with
  troubleshooting prompts against clusters with injected broken states. Asserts
  that the run completes the full lifecycle (Pending → Analyzing → Proposed →
  Executing → Verifying → Completed) with real LLM providers.
- **Out of scope:** Sandbox output quality verification (sandbox repo
  responsibility), behavioral correctness of fixes (future work).

### Build tag

Tests are gated behind the `product_e2e` build tag, separate from the `e2e`
tag used by mock-agent tests. They are only invoked via `make product-e2e`,
never via `make test-e2e`.

```go
//go:build product_e2e
```

### Test file

`test/e2e/troubleshooting_test.go` — one parametrized test function covering
all 11 troubleshooting scenarios.

### Test flow per scenario

1. Register `cleanup.sh` via `t.Cleanup` **before** running `setup.sh`, so
   cleanup runs even if setup fails and always before the next scenario starts
2. Run scenario `setup.sh` via `os/exec` to inject broken cluster state
3. Create AgenticRun CR with the scenario's request text, pointing at the real
   provider's Agent/LLMProvider fixtures (from `createRealProviderFixtures`)
4. Observe the phase progression in order (Pending → Analyzing → Proposed →
   Executing → Verifying → Completed) — record phase history via the watch/poll
   helper and assert each intermediate phase was seen, not only the terminal
   `AgenticRunPhaseCompleted`
5. Assert: AnalysisResult, ExecutionResult, VerificationResult CRs exist with
   owner references
6. Assert: no `Failed` conditions on the AgenticRun

### Scenario script access

Troubleshooting scenario scripts are owned by `lightspeed-agentic-sandbox`
under `scenarios/troubleshooting/`. The operator accesses them at CI time by
extracting from the sandbox container image.

`e2e-cluster.sh` changes:
- Before running tests, extract `scenarios/` from the sandbox image to a temp
  directory
- Export `E2E_SCENARIOS_DIR` pointing at the extracted path
- The Go test reads `E2E_SCENARIOS_DIR` to locate setup/cleanup scripts and
  `scenario_metadata.yaml` for scenario parameters

### Scenarios

The 11 scenarios and their metadata are defined in the sandbox repo. See
`lightspeed-agentic-sandbox/.ai/spec/what/e2e-testing.md` for the full
scenario table.

### e2e-cluster.sh extension

After running the existing `make test-e2e` (mock-agent tests) per provider,
the script runs:

```bash
E2E_PROVIDER="$provider" \
  E2E_MODEL="$model" \
  E2E_PROVIDER_KEY_PATH="$key_path" \
  E2E_POLL_TIMEOUT="${E2E_POLL_TIMEOUT:-20m}" \
  E2E_SCENARIOS_DIR="$scenarios_dir" \
  VERTEX_PROJECT_ID="${VERTEX_PROJECT_ID:-}" \
  VERTEX_REGION="${VERTEX_REGION:-global}" \
  TEST_NAMESPACE="$NAMESPACE" \
  go test -tags=product_e2e ./test/e2e/... -count=1 -v -timeout 240m
```

The provider configuration MUST be exported for the `go test` invocation so it
is available during `createRealProviderFixtures`; this mirrors the per-provider
env block already set in `scripts/e2e-cluster.sh`.

The suite timeout MUST exceed `11 * E2E_POLL_TIMEOUT` plus per-scenario
setup/cleanup overhead — 11 scenarios run sequentially, each polling up to
`E2E_POLL_TIMEOUT` (default 20m ⇒ up to 220m of polling). Keep `-timeout` above
that whenever `E2E_POLL_TIMEOUT` changes.

With environment:
- `E2E_PROVIDER`, `E2E_MODEL`, `E2E_PROVIDER_KEY_PATH` — from existing flow
- `E2E_SCENARIOS_DIR` — extracted scenario scripts path
- `E2E_POLL_TIMEOUT` — default 20m per scenario (longer than mock-agent tests)

### Assertions

Phase transition:
- AgenticRun passes through each phase in order (Pending → Analyzing → Proposed
  → Executing → Verifying → Completed), verified against recorded phase history
- AgenticRun reaches `Completed` phase (derived from conditions)
- `Analyzed=True`, `Executed=True`, `Verified=True` conditions present
- AnalysisResult CR exists with owner reference to AgenticRun
- ExecutionResult CR exists with owner reference to AgenticRun
- VerificationResult CR exists with owner reference to AgenticRun
- No `False`-status conditions with failure reasons

No assertions on result content quality — that is the sandbox repo's
responsibility via LLM judge.

### Constraints

- Requires a live OpenShift cluster with the operator deployed
- Requires real LLM provider credentials
- Scenario setup/cleanup scripts must be idempotent
- Tests run sequentially (one scenario at a time) to avoid cluster state
  interference

### Future work

- [PLANNED] Behavioral correctness assertions: verify ExecutionResult actions
  match expected fix patterns per scenario
- [PLANNED] Failure scenario tests: verify graceful handling when the LLM
  cannot diagnose the problem (phase reaches Failed or Escalated)

## Multicluster e2e (agentic-operator share)

The operator participates in the cross-repo **multicluster test suite** that
validates hub-managed fleet operations. The suite's tier definitions, ownership
split, and shared kubeconfig contract live in the parent spec
(`ols/.ai/spec/what/multicluster-testing.md`); the primary owner and its
mechanics are in `lightspeed-hub/.ai/spec/what/multicluster-testing.md`. This
section records the operator's share.

### Scope

- **In scope:** `spec.targetCluster` reconcile, ephemeral SA creation on a
  *separate* spoke apiserver (24h bound token via the TokenRequest API),
  cross-cluster cleanup (finalizer removes spoke-side resources; resources carry
  `hub.openshift.io/spoke-cluster` and `hub.openshift.io/agentic-run` labels; the
  periodic stale-SA sweep runs), and sandbox wiring against a real spoke.
- **T1** asserts a full AgenticRun lifecycle with the **mock agent** against a
  real spoke reaches `Completed`, and that the ephemeral token is RBAC-scoped
  (succeeds inside `targetNamespaces`, denied outside).
- **T2** reuses the phase-transition assertions above (Pending → Analyzing →
  Proposed → Executing → Verifying → Completed) against a real hosted spoke with
  a real provider.
- **Out of scope:** same boundary as the troubleshooting scenarios above —
  sandbox output quality and behavioral correctness of fixes.

### Build tag

Distinct from this repo's `e2e` / `product_e2e` tags. T1 files:

```go
//go:build mc_e2e
```

T2 files:

```go
//go:build mc_product_e2e
```

### Operator risk paths (CI gating)

T1 MUST run per-PR and block merge when a PR touches: `targetCluster` reconcile,
ephemeral-SA and cross-cluster-cleanup code, sandbox wiring, or the `mc_e2e`
tests. It MAY be skipped otherwise (Prow `run_if_changed`; regex lives in
`openshift/release`).

## Commands

```bash
make test          # unit tests (no cluster, no credentials)
make test-e2e      # mock-agent e2e (cluster + operator, no real LLM)
make product-e2e   # full product e2e including troubleshooting scenarios
```
