# Disconnected Gemma Product E2E (OLS-4226)

An infrastructure/provider variant of the existing `product_e2e` suite, not an
LSEval or sandbox output-quality suite. Behavioral contract:
[`../what/product-e2e-testing.md`](../what/product-e2e-testing.md).

## Entrypoint and connected preparation

`make product-e2e-disconnected` invokes `scripts/e2e-disconnected.sh`:

1. Require `LIGHTSPEED_SERVICE_REF` as exactly 40 hexadecimal characters.
2. Clone `openshift/lightspeed-service` into a temporary directory, verify the
   requested object with `git cat-file -e "${SHA}^{commit}"`, check out that SHA
   detached, and verify HEAD equals the requested SHA. There is no submodule or
   copied provisioning code.
3. Invoke the operator-owned `scripts/e2e-rhoai.sh` orchestration helper against
   that checkout's `tests/rhoai/` assets. Like the provisioning section in the
   service's `tests/scripts/test-lseval-periodic.sh`, this caller sequences the
   individual service scripts/manifests; it does not invoke LSEval or require a
   separate service provisioning entrypoint.
4. The operator caller discovers the Service endpoint/ports/selector and verifies
   the selected model through an authenticated models request. It writes a
   safely shell-quoted, non-secret handoff consumed by the runner. Values:
   `RHOAI_VLLM_BASE_URL`, `RHOAI_VLLM_MODEL`, `RHOAI_VLLM_NAMESPACE`,
   `RHOAI_VLLM_SERVICE_NAME`, `RHOAI_VLLM_SERVICE_PORT`,
   `RHOAI_VLLM_NETWORK_PORT`, `RHOAI_VLLM_POD_SELECTOR_JSON`.
5. Write `VLLM_API_KEY` to a temporary mode-0600 file; export OpenAI provider
   inputs using the handoff URL/model. Disable optional OTEL deployment.
6. Invoke the existing `scripts/e2e-cluster.sh openai` path to deploy/reuse the
   operator and clone `rhobs/troubleshooting-scenarios` while connected.

CI must mirror images before invocation. `SANDBOX_IMAGE` and actual configured
sandbox container/init-container/OCI-volume images must use the OpenShift
internal registry (`image-registry.openshift-image-registry.svc:5000`).
`E2E_SKILL_IMAGE_MAP` optionally names a JSON object mapping original scenario
skill pullspecs to their mirrored internal pullspecs. Discovery remains the
standard `evals/scenarios/*/evals.yaml` core-tag path. No scenario allowlist or
exclusions are accepted.

### Connected provisioning and service asset interface

`e2e-rhoai.sh` validates all required scripts and namespace/operator/GPU/vLLM
manifests before cluster changes. The caller bounds the helper and its child
processes with `E2E_RHOAI_PROVISION_TIMEOUT` (default 120m, positive GNU timeout
duration). This covers unbounded discovery loops in the reused scripts as well
as model readiness. A timed-out stage exits with status 124 and is not retried.
The helper performs:

1. Source `scripts/model-profile.sh`, set `VLLM_MODEL_PROFILE=gemma-4-31b`, and
   call `load_vllm_model_profile`. Missing/incompatible Gemma settings fail;
   the classic Llama profile is never a fallback.
2. Apply the service's NFD/NVIDIA namespace manifests, then invoke
   `scripts/bootstrap.sh` and `scripts/gpu-setup.sh` with the service RHOAI
   directory as their argument.
3. Create the model-serving namespace and HF/vLLM Secrets from private files,
   not credential-bearing command arguments.
4. Download the profile's chat template using the HF token while connected and
   create its ConfigMap with the profile's name/key. Source
   `scripts/fetch-vllm-image.sh`, then invoke `scripts/deploy-vllm.sh` against
   the service's profile-parameterized runtime and inference manifests.
5. If the RawDeployment already exists, restart it after applying credentials
   and wait for rollout so reused Pods receive the current Secret-backed
   environment. Do not force a second download on an initial deployment. Wait
   for InferenceService readiness (`E2E_RHOAI_READY_TIMEOUT`, default 60m), then
   invoke `scripts/get-vllm-pod-info.sh` with a private `ENV_FILE` path.
6. Read the actual Service and selected Pods. Construct the internal `/v1` URL
   with the published **Service port**, not the target port currently used by
   the service Pod-info helper's `KSVC_URL`. Resolve numeric or named Pod target
   ports separately for policy enforcement; ambiguous or invalid ports fail.
7. Use the nonempty Service selector as `matchLabels` in the handoff. Before
   restriction, the Go harness verifies it matches exactly the backing Pods of
   the owning InferenceService.
8. Make an authenticated models request from a running, non-terminating inference
   Pod, supplying the caller's current API key through exec stdin from its
   private file. Do not use a potentially stale Pod environment key to validate
   connectivity. Service DNS need not be resolvable on the host runner, and the
   key is not passed in exec arguments. Reject a response that does not
   advertise the profile's exact model ID.

The current service manifests use RawDeployment with namespace `e2e-rhoai-dsc`,
ServingRuntime `vllm-gpu`, InferenceService `vllm-model`, Service
`vllm-model-predictor`, and container `kserve-container`. The pinned revision must
preserve these interfaces or be coordinated with the caller. Service owns the
reused scripts, manifests and model profiles; the operator owns orchestration,
readiness/model confirmation, and handoff production. No service scripts or
manifests are copied into the operator repository.

Connected preparation requires `curl`, `envsubst` and GNU `timeout` (coreutils)
in addition to the standard runner tools. The inference image must provide Python 3 for model confirmation.
CI owns teardown of provisioned operators/GPU/model-serving resources; they are
not treated as temporary restricted-test probe resources.

## Service revision pin ownership and updates

`LIGHTSPEED_SERVICE_REF` is a caller-supplied input, not a hardcoded operator
revision. Local users supply it through the environment. CI MUST store the full
commit SHA in version-controlled disconnected job configuration, not solely in
an opaque CI variable. Once the job exists, its documentation MUST identify the
repository, configuration file and field containing the pin.

Disconnected CI-job maintainers own the pin and coordinate with
`lightspeed-service` maintainers when the provisioning contract changes.
Checkout verification establishes commit identity and reproducibility; it does
not establish compatibility with the operator harness.

### Update procedure

1. Initially select a merged `lightspeed-service` commit containing the Gemma
   profile and compatible individual RHOAI scripts/manifests described above.
   No separate provisioning entrypoint or service-produced handoff is required.
2. Propose pin updates through a reviewed PR when provisioning fixes, required
   model/runtime changes or coordinated contract changes need to be consumed.
   Unrelated service commits do not require a pin update.
3. Run the disconnected product E2E job against the proposed SHA before merging
   the update. Passing checkout validation or cluster-free tests alone is not
   sufficient to establish compatibility.
4. Record the reason for the update and its successful validation in the PR.
   Retain the previous SHA in version-control history so rollback is a reviewed
   revert to the last known-good pin.

The job MUST NOT follow `main`, a moving tag, or resolve the latest service
commit on every run. Automatic pin updates are optional, not required for the
initial CI job. Renovate or a scheduled workflow MAY open pin-update PRs, but
those PRs MUST still require disconnected E2E validation and maintainer review;
automation MUST NOT silently adopt or automatically merge unvalidated revisions.

## Restricted runtime

`prepareDisconnected` in `test/e2e/disconnected_test.go` validates the handoff,
requires `bare-pod` mode, validates the complete configured sandbox PodSpec,
and rewrites/validates every selected scenario's skill image **before any
scenario setup script, provider fixture, or AgenticRun is created**. Public
references fail rather than falling back. Kubelet image pulls are not protected
by Pod NetworkPolicy, making this validation mandatory.

`test/disconnected/` implements test-only policy/probe support:

- Sandbox policy selects presence of `agentic.openshift.io/run`.
- Inference policy uses the handoff LabelSelector without replacing expressions.
  Owner-chain checks prove its matched Pod set is exactly the backing Pods of
  the InferenceService owning the handoff Service. Empty, invalid, overbroad,
  incomplete and host-networked selections fail.
- DNS and API peers are discovered from Services and EndpointSlices, restricted
  to individual `/32` or IPv6 `/128` addresses. DNS allows UDP/TCP 53 and its
  discovered target port (typically 5353); API allows TCP 443 and its discovered
  target port. Metrics ports are not allowed.
- Sandbox egress additionally permits the inference namespace plus exact Pod
  selector on the handoff TCP target port. No broad namespace/IP exceptions.
- Use dedicated test namespaces without existing egress NetworkPolicies;
  policies are additive, so pre-existing egress policies cause setup failure.
- Optional OTEL/MCP/RHOKP handoff endpoints are unsupported in this narrow
  variant. Extending coverage requires explicit destination rules and probes.

Two temporary probe Pods use the mirrored sandbox image (Python 3 required) and
labels selected by the respective policies. An always-failing readiness probe
prevents the inference-labeled probe from entering model-serving endpoints.
Dedicated probe ServiceAccounts receive temporary image-puller RBAC in the
sandbox image namespace. ServiceAccount controller owner references prevent
ReplicaSet adoption even when inference selectors include revision labels.
Both first establish successful, certificate-verified HTTPS connections to a
canary and retain only the IPs proven reachable. After restriction, DNS must
still work and connections to those IPs must time out or be refused; DNS failure
alone cannot satisfy denial. Both probes perform authenticated Kubernetes API
requests. The sandbox probe also performs authenticated `/v1/models` and checks
the exact handoff model. Keys are mounted from a temporary Secret, never passed
in command arguments.

A Pod watch checks every `ls-*` sandbox's stable run label, container/init/
ephemeral-container and image-volume pullspecs, host networking and image-pull
errors. Normal watch disconnects resume from the last resourceVersion; unrecoverable
errors/expired history fail rather than silently losing boundary coverage.
Watcher failures cancel a shared suite context: preflight, scenario setup and
phase polling stop, and the test goroutine aborts remaining scenarios. Boundary
violations and image-pull failures use the same fail-fast path. Diagnostics and
registered cleanup retain independent contexts; intentional watcher shutdown
during cleanup is not a failure.
The standard product suite then owns scenario execution and result assertions.

`E2E_SUITE_TIMEOUT` defaults to 12h for this variant. The test rejects a deadline
shorter than all selected per-scenario deadlines plus cleanup/preflight overhead.

## Diagnostics and cleanup

Per-run CRs/results and watch-observed sandbox status are archived before their
cleanup. Before boundary cleanup, collect policies, selector checks, events,
ServingRuntime/InferenceService status and selected Pod logs. The shell fallback
also collects redacted inference logs when provisioning/readiness fails before
successful Pod discovery. This variant bypasses the connected runner's raw
artifact collector/log watcher; test output,
sandbox logs and fallback diagnostics are credential-redacted. No Secret objects
are archived.

Cleanup is registered before temporary resources are created. Normal Go cleanup
collects diagnostics, then invokes the same authoritative shell cleanup used for
hard Go timeouts and INT/TERM; it never deletes policies directly after a cleanup
failure. Every harness-created resource carries a unique invocation label,
including provider fixtures and AgenticRuns. Actual sandbox Pods carry the run
UID label, not the invocation label.

The outer runner exports a private `E2E_DISCONNECTED_RUNS_FILE` JSON-lines journal.
Each newly created run's namespace/name/UID is durably recorded before registering
its per-scenario deletion. Shell cleanup combines that journal with live
invocation-labelled runs, and persists live-discovered identities before any
deletion. This also covers interruption between run creation and Go journal
recording, so recovery can still find sandbox Pods after their run CR has
disappeared. It snapshots live run conditions, result objects, sandbox
status/logs and run identities before deletion, including after hard timeouts
that bypass Go cleanup. Evidence is credential-redacted; recovery attempts write
under separate `fallback/retry.*` directories instead of overwriting the first
pre-deletion snapshot. Artifact-write/redaction failures and failed required
workflow API fetches stop cleanup before destructive commands. Optional unavailable
logs (for example a container that never started) are retained as redacted error
text without blocking teardown.

Cleanup first marks owned runs deleting to prevent sandbox recreation, deletes
and waits for run-UID-selected Pods in their recorded namespaces, waits for run
finalizers, and checks again for residual sandbox Pods. Only then does it remove
policies/probes/temporary credentials and verify no invocation-owned resources
remain. A failed stop/wait/residual check preserves the remaining policies for
recovery and turns an otherwise successful run into failure.

The outer runner also exports a private `E2E_DISCONNECTED_CLEANUP_MARKER` shared
with Go and the product runner. The authoritative script writes it only after
all cleanup checks succeed. Later invocations skip cleanup when that marker
exists, preserving the evidence. Failed or interrupted cleanup remains eligible
for fallback, and existing test/cleanup exit-code handling is preserved.
Before starting resource cleanup, the product runner atomically writes non-secret
operator/RBAC ownership flags to a private `E2E_DISCONNECTED_OPERATOR_STATE` file
instead of undeploying the disconnected operator itself. Early publication keeps
ownership available if the inner cleanup is interrupted. The outer runner performs standard operator cleanup only after
resource cleanup succeeds, including after recovery; on failure it leaves the
operator and CRDs available to process finalizers and support further recovery.

The connected provisioning helper and product runner use separate process groups
so cancellation stops their children before cleanup. SIGKILL cannot be trapped.
The shell removes temporary checkouts/key files. Service/GPU provisioning resources
remain owned by service/CI. No failure triggers an unrestricted retry.

## Verification

- `make test`: handoff/image/policy/selector/endpoint/probe-baseline/artifact tests.
- `make test-product-e2e-unit`: OpenAI fixture and shell entrypoint tests without
  a cluster (pin validation, provisioning contract, handoff, exit status, watcher
  reconnection/failure and cancellation with retained cleanup).
- Live coverage requires a GPU OpenShift cluster, a pinned service revision with
  compatible Gemma/RHOAI assets, mirrored images, and CI-provided model credentials.
