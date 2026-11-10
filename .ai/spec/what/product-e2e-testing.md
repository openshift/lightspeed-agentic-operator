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

## Model compatibility smoke (OLS-4817)

> **Status:** All rules in this section are planned: [PLANNED: OLS-4817].

A periodic job that proves each supported provider route still works with the
latest models. It configures Agentic OLS through the real `LLMProvider` and
`Agent` CRs, then runs one analysis per `(route, model)` pair. It checks
compatibility only. Model quality, RCA correctness and remediation behavior
belong to evals (epic OLS-3758).

### Model versions file

1. `test/models/model-versions.yaml` MUST be the single source of real-provider
   model IDs for `make product-e2e` and `make product-e2e-smoke`.
   `resolve_model()` in `scripts/e2e-lib.sh` MUST read route primaries from it
   instead of hardcoding them. An explicit `E2E_MODEL` still overrides the
   file for local runs. Harnesses read the file from the checked-out source;
   no run-time fetch is performed.
2. Every model value MUST be an explicit, deployable model ID, never an alias.
   Route-specific identifiers (Bedrock inference-profile IDs, Azure deployment
   names) are stored exactly as the route expects them.
3. The file has two maps. Route keys equal the `E2E_PROVIDER` values accepted
   by the real-provider fixtures:

   ```yaml
   routes:                       # one entry per E2E_PROVIDER fixture
     <route>:
       family: anthropic | google | openai | <other>
       primary: <model-id>       # used by product-e2e and smoke
       smoke: true | false       # optional, default true
   families:
     <family>:
       referenceRoute: <route>   # route that runs this family's extras
       extras: [<model-id>, ...] # smoke-only compatibility models
   ```

   Illustrative content (model IDs change as providers ship):

   ```yaml
   routes:
     anthropic:        { family: anthropic, primary: claude-haiku-5-5 }
     claude:           { family: anthropic, primary: claude-haiku-5-5 }   # Vertex
     bedrock-claude:   { family: anthropic, primary: global.anthropic.claude-haiku-5-5 }
     gemini:           { family: google,    primary: gemini-3.1-flash-lite }  # Vertex
     openai:           { family: openai,    primary: gpt-6-luna }
     azure-openai:     { family: openai,    primary: gpt-6-luna }
     bedrock-deepseek: { family: deepseek,  primary: deepseek.v3.2, smoke: false }
   families:
     anthropic: { referenceRoute: anthropic, extras: [claude-sonnet-5-5, claude-opus-4-6] }
     google:    { referenceRoute: gemini,    extras: [gemini-3.8-flash] }
     openai:    { referenceRoute: openai,    extras: [gpt-6.1-sol, gpt-6.1-terra] }
   ```

4. The file MUST be validated before any cluster work. Validation fails on an
   unknown route key, a route without `primary`, a smoke-enabled route whose
   family has no `families` entry, a `referenceRoute` that is missing, has
   `smoke: false`, or belongs to another family, or an empty model ID.

### Smoke matrix

5. The matrix MUST contain:
   - one `(route, primary)` pair for every route with `smoke` not set to
     `false`;
   - one `(referenceRoute, extra)` pair for every extra of every family.

   Duplicate pairs are collapsed. Every in-scope route is proven with its
   primary model. Every extra model is proven on one route of its family.
6. In-scope routes are direct Anthropic (`anthropic`), Vertex/Anthropic
   (`claude`), Bedrock/Anthropic (`bedrock-claude`), Vertex/Gemini
   (`gemini`), native OpenAI (`openai`) and Azure OpenAI (`azure-openai`).
   `bedrock-deepseek` is a product-e2e route with `smoke: false`. Bedrock
   OpenAI and OpenAI-compatible/vLLM routes are out of scope.
7. The real-provider fixtures MUST support `E2E_PROVIDER=anthropic`: a Secret
   with key `ANTHROPIC_API_KEY` and `LLMProvider.spec.type=Anthropic`. Existing
   fixtures cover the other in-scope routes.

### Scenario

8. The job MUST select its scenario from `rhobs/troubleshooting-scenarios`
   with `E2E_SCENARIO_TAGS=smoke`, through the standard discovery path. The
   `smoke` tag is initially applied only to `crashlooping_pod_alert`. That
   scenario needs real investigation: it is alert-driven, mounts the
   `investigate-alert` OCI skill, and the root cause is only visible in the
   container logs. Changing the scenario means moving the tag; it needs no
   operator change.
9. Discovery MUST match exactly one scenario. Zero or several matches fail the
   job before any AgenticRun is created.
10. Scenario `setup.sh` runs once before the matrix. `cleanup.sh` is
    registered before setup and runs once after the matrix, including after
    failures.

### Execution and pass criteria

11. Matrix entries MUST run sequentially, after one operator deployment. Each
    entry creates its own route Secret, `LLMProvider` and `Agent` with the
    entry's model. It then creates one AgenticRun from the scenario's
    `openshift_agentic_run_spec`, including its skills and target namespaces.
    The entry's run, agent, provider and Secret are deleted before the next
    entry starts.
12. The run is analysis-only. Smoke fixtures MUST NOT auto-approve execution,
    so the run stops at `Proposed`. The scenario's
    `expected_openshift_agentic_run_status` is ignored. An entry passes when,
    within `E2E_SMOKE_TIMEOUT` (default 15m), the run reaches `Proposed` with
    `Analyzed=True` and an owned AnalysisResult exists. Analysis content, RCA
    correctness and tool-call counts are not asserted.
13. A failing entry MUST NOT stop the remaining entries. The job exits
    non-zero if any entry fails. No entry is retried automatically.
14. Credentials for every route in the matrix MUST be present before scenario
    setup. A missing credential fails the job; the route is never silently
    skipped.

### Results and notification

15. Each failed entry MUST be classified from AgenticRun conditions, sandbox
    termination messages and sandbox logs:
    - `model_unavailable`: the provider reports an unknown, retired or
      undeployed model;
    - `credentials`: an authentication or authorization failure;
    - `timeout`: the pass criteria were not met within `E2E_SMOKE_TIMEOUT`;
    - `product`: any other failure.
16. Per-entry diagnostics go under `$ARTIFACT_DIR/smoke/<route>/<model>/`:
    AgenticRun YAML, Result CRs, sandbox status and logs, events, and OTEL
    artifacts when enabled. They are collected before the entry's resources
    are deleted. The job also writes `smoke-summary.json` and
    `smoke-summary.md`: a route × model table with result, failure class and
    duration, plus the operator commit and the model-versions file content.
    Secret contents and known credentials MUST NOT appear in artifacts.
17. The job runs on Konflux as an optional (non-gating)
    `IntegrationTestScenario` named `agentic-model-smoke`. It uses the same
    nightly snapshot trigger as `agentic-product-e2e`.
18. When any entry fails, a final pipeline step MUST send a notification
    containing the summary table and a link to the PipelineRun. It reuses or
    mirrors the existing FBC-test failure-notification mechanism. A passing
    job sends no notification.

### Constraints and exclusions

- Credentials for all six in-scope routes must be available to the Konflux
  tenant pipeline.
- The scenario's skill image is referenced by a mutable tag in
  `troubleshooting-scenarios`. A broken skill push can fail every entry with
  class `product`, unrelated to the models.
- Classic OLS smoke (OLS-4819) and automated model-version updates
  (OLS-4818) are separate work. Consumers outside this repository read the
  file from this repository.
- Out of scope: execution and verification phases, LLM judges, RCA
  correctness, Bedrock OpenAI, OpenAI-compatible/vLLM and Gemma routes.

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
make product-e2e-smoke # [PLANNED: OLS-4817] analysis-only smoke per (route, model) from test/models/model-versions.yaml
make test-product-e2e-unit # cluster-free product harness tests
```
