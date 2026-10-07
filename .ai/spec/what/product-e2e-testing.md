# Product E2E testing

Behavioral specification for product-level tests exercising the AgenticRun
lifecycle on real OpenShift clusters with real LLM providers. Distinct from
`make test-e2e` mock-agent tests, which validate operator logic in isolation.

## Relationship to other specs

| Spec | Relationship |
|------|-------------|
| [run-lifecycle.md](run-lifecycle.md) | Defines the workflow phases and terminal outcomes |
| [sandbox-execution.md](sandbox-execution.md) | Defines sandbox wiring and stable run labels |
| [approval.md](approval.md) | Product tests use automatic approval |
| [../how/disconnected-product-e2e.md](../how/disconnected-product-e2e.md) | Restricted-network implementation and provisioning contract |

## Standard product E2E (`make product-e2e`)

`scripts/e2e-cluster.sh` deploys/reuses the operator, clones
`rhobs/troubleshooting-scenarios`, and runs the `product_e2e`-tagged suite per
selected real provider. It does not invoke the mock-agent suite.

Scenario discovery reads `evals/scenarios/*/evals.yaml`. Membership is defined
by scenario tags, not a fixed scenario count. `E2E_SCENARIO_TAGS` defaults to
`core` and is a comma-separated AND filter. Standard connected runs may use
`E2E_SKIP_SCENARIOS`; the disconnected variant must not.

### Scenario flow (OLS-3739)

1. Register scenario cleanup before setup so it also runs after setup failure.
2. Run `setup.sh` to inject broken state.
3. Create an AgenticRun with the scenario request, target namespaces and skills,
   using the real-provider fixtures and automatic approval policy.
4. Wait for the scenario's expected terminal phase (default `Completed`).
5. For completed runs, assert AnalysisResult, ExecutionResult and
   VerificationResult existence/owner references and no failed workflow
   conditions. Other expected terminal outcomes require an AnalysisResult.
6. Archive available diagnostics before deleting the run, then execute cleanup
   before the next scenario.

Tests are sequential to avoid state interference. Result quality, LLM judges
and behavioral correctness of fixes remain out of scope.

### Inputs

- `E2E_PROVIDER`, `E2E_MODEL`, provider credentials: real-provider fixtures.
- `E2E_OPENAI_URL`: optional custom OpenAI-compatible API root, passed to
  `LLMProvider.spec.openAI.url`; unset preserves the provider default.
- `E2E_SCENARIOS_DIR`: checkout containing `evals/scenarios`.
- `E2E_SCENARIO_TIMEOUT`: per-scenario deadline, default 20m.
- `E2E_SUITE_TIMEOUT`: runner's Go test deadline, default 120m for connected
  runs; configure above the selected scenario count times its deadline plus
  setup/cleanup overhead.
- `ARTIFACT_DIR`: provider logs, run records and sandbox diagnostics.

## Disconnected Gemma product E2E (OLS-4226)

1. `make product-e2e-disconnected` MUST provide a connected provisioning phase
   followed by restricted runtime execution. It MUST clone `lightspeed-service`
   at a required, verified full commit SHA. The operator caller MUST orchestrate
   that checkout's individual RHOAI scripts/manifests and select its existing
   `gemma-4-31b` profile, following the service calling-script pattern. Reused
   provisioning assets MUST NOT be copied here; no separate service provisioning
   entrypoint is required. The operator MUST discover the internal Service
   endpoint/ports/selector and confirm the exact model with an authenticated
   models request, producing the handoff for restricted execution.
2. Gemma MUST reuse the existing OpenAI provider, with its exact model ID and
   cluster-internal `/v1` URL returned by provisioning. No new provider type.
3. The suite MUST reuse standard `product_e2e` discovery and assertions with
   `E2E_SCENARIO_TAGS=core`, without a disconnected scenario allowlist or skips.
4. The variant MUST use bare-pod sandbox mode. All configured sandbox images
   and every selected scenario's skill image MUST be validated/replaced with
   cluster-local pullspecs before scenario setup, fixtures and AgenticRuns are
   created. Public or unresolved references MUST fail; kubelet pulls are not
   constrained by Pod NetworkPolicy.
5. Temporary egress policies MUST select all sandbox run Pods and the exact
   actual inference backing-Pod set. They MUST deny external egress and allow
   only discovered single-address DNS/API peers and sandbox-to-vLLM traffic.
   Pre-existing additive egress policies MUST cause setup failure.
6. Before tests, policy-selected probes MUST prove working DNS, authenticated
   Kubernetes API access and authenticated vLLM `/v1/models` access with the
   expected model. A certificate-verified HTTPS canary MUST work before policy
   installation and its proven-reachable IPs MUST be denied afterward from both
   runtime policy sets. DNS failure alone MUST NOT count as egress denial.
7. During tests, sandbox Pod inspection MUST fail on missing stable run labels,
   external container/skill pullspecs, host networking, image-pull failures or
   lost inspection coverage. Normal watch reconnects MUST retain the last
   resourceVersion; expired event history MUST fail. Unrecoverable watch
   failures or boundary violations MUST cancel active scenario work and abort
   remaining scenarios, while preserving diagnostics and registered cleanup.
   Intentional watcher shutdown during cleanup MUST NOT fail the suite.
8. Failures after restriction MUST remain failures. The harness MUST NOT
   restore external access and retry. Cleanup MUST preserve the original exit
   status and remove only harness-created policies/probes/temporary credentials.
9. Diagnostics MUST be collected before destructive cleanup and redact known
   CI credentials. Include per-run conditions/results, sandbox status/logs,
   inference status/logs, events, policies and selector evidence. Do not archive
   Secret contents.
10. The disconnected suite timeout MUST exceed all selected per-scenario
    deadlines plus cleanup/preflight overhead; the default is 12h. Too-short
    deadlines MUST fail clearly before scenario execution.

### Constraints and exclusions

- Requires a GPU OpenShift test cluster with enforceable NetworkPolicy and
  internal-registry image access, plus connected model preparation.
- CI owns image mirroring, the cluster/job and provisioning-resource teardown.
  Service owns reusable GPU/model-serving scripts, manifests and model profiles;
  the operator owns their orchestration and the restricted-test handoff.
- Probe images must provide Python 3. This variant permits DNS/API/vLLM only;
  optional OTEL/MCP/RHOKP dependencies require future explicit policy extensions.
- Out of scope: LSEval, LLM judges, new Gemma adapters, sandbox-claim coverage,
  disconnected-only scenario membership and classic-operator bundle changes.

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
  real spoke reaches `Completed`, and that the ephemeral token is RBAC-scoped:
  read access remains available across namespaces, but execution modifications
  are denied outside namespaces named by approved namespace-scoped RBAC rules.
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
make product-e2e-disconnected # provision Gemma, then restricted core product e2e
make test-product-e2e-unit # cluster-free product harness tests
```
