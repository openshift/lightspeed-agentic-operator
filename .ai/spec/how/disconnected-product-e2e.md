# Disconnected Gemma 4 Product E2E

Implementation contract for OLS-3472. This is an infrastructure/provider variant of the existing real-provider product-e2e runner in `scripts/e2e-cluster.sh`; it is not an LSEval or sandbox output-quality suite.

Cross-references: provider API → `../what/crd-api.md`; sandbox environment mapping → `../what/sandbox-execution.md`; workspace contract → `ols/.ai/spec/what/agentic-disconnected-operation.md`.

## Existing Product-E2E Contract

- `scripts/e2e-cluster.sh` clones `rhobs/troubleshooting-scenarios` before running tests.
- `test/e2e/troubleshooting_test.go` is compiled with the `product_e2e` build tag.
- Scenario discovery reads each `agentic/<scenario>/evals.yaml` from that checkout.
- `E2E_SCENARIO_TAGS` is a comma-separated AND filter over `evals.yaml` tags and defaults to `core`.
- Standard product-e2e owns scenario discovery and lifecycle/result assertions. OLS-3472 MUST reuse these paths and MUST NOT copy scenario names into a disconnected allowlist.

## Planned Command and Phases

[PLANNED: OLS-3472] Add:

```bash
make product-e2e-disconnected
```

The target has two phases and MUST preserve the first failure as its exit status.

### 1. Connected provisioning

1. Run on a GPU-enabled OpenShift cluster with RHOAI/KServe available or installable.
2. Clone `rhobs/troubleshooting-scenarios` through the existing product-e2e path.
3. Require `LIGHTSPEED_SERVICE_REF` to match exactly 40 hexadecimal characters and verify with `git cat-file -e "${LIGHTSPEED_SERVICE_REF}^{commit}"` that it resolves to a commit object. Check out `lightspeed-service` separately at that SHA and verify `git rev-parse HEAD` equals the supplied value. Branches, tags, abbreviated SHAs, and revision expressions are invalid. Do not add a Git submodule or copy its scripts.
4. Invoke `tests/rhoai/scripts/provision-vllm.sh --profile gemma4 --output-env <path>` from the service checkout with secret CI inputs `HUGGING_FACE_HUB_TOKEN` and `VLLM_API_KEY`.
5. Source the safely quoted output file. It provides:
   - `RHOAI_VLLM_BASE_URL` — internal OpenAI API root ending in `/v1`
   - `RHOAI_VLLM_MODEL` — model identifier confirmed by the vLLM models API
   - `RHOAI_VLLM_NAMESPACE`
   - `RHOAI_VLLM_SERVICE_NAME` and `RHOAI_VLLM_SERVICE_PORT`
   - `RHOAI_VLLM_NETWORK_PORT` — backing-Pod destination port
   - `RHOAI_VLLM_POD_SELECTOR_JSON` — compact JSON `metav1.LabelSelector` whose matched set is exactly the selected `InferenceService` backing Pods
6. Mirror the agentic sandbox image and every OCI skill image referenced by the selected core scenarios into a cluster-local registry. Before installing the operator or creating any `AgenticRun`, validate the configured sandbox image and every discovered core scenario's skill image against the internal-registry allowlist, rewrite fixtures to the mirrored pullspecs, and reject any unresolved or external reference. This pre-creation validation is the primary control because kubelet pulls occur before Pod inspection and are not constrained by Pod NetworkPolicy. `PullAlways` is permitted only against the internal registry.

### 2. Restricted execution

The disconnected job MUST use `bare-pod` sandbox mode. In this mode every sandbox Pod carries `agentic.openshift.io/run`; sandbox-claim backing Pods do not carry that label directly and are therefore outside this test variant until they have an equally reliable policy selector.

The harness owns temporary Kubernetes `NetworkPolicy` objects in the agentic operator and vLLM namespaces:

- Sandbox policy selects Pods where `agentic.openshift.io/run` exists.
- vLLM policy unmarshals `RHOAI_VLLM_POD_SELECTOR_JSON` directly into its Pod selector in `RHOAI_VLLM_NAMESPACE`; invalid JSON or an invalid LabelSelector fails setup.
- Both deny all egress except cluster DNS and Kubernetes API access. The harness resolves the `openshift-dns/dns-default` Service ClusterIP and EndpointSlice addresses and allows only those `/32` peers on UDP and TCP 53. It resolves the `default/kubernetes` Service ClusterIP and EndpointSlice addresses and allows only those `/32` peers on TCP 443 and the published endpoint target port. Including both Service and endpoint addresses supports network plugins that enforce policy before or after service translation.
- The sandbox policy additionally allows the vLLM peer using a namespace selector for `RHOAI_VLLM_NAMESPACE`, the unmarshalled `RHOAI_VLLM_POD_SELECTOR_JSON`, and TCP `RHOAI_VLLM_NETWORK_PORT`.
- Any additional internal destination required by a core scenario must be explicit in policy setup. CIDR exceptions wider than a discovered single-address `/32`, cluster-wide namespace selectors, and internet CIDRs are forbidden.

Before product-e2e starts:

1. Verify the vLLM policy selector matches every current inference Pod.
2. Start policy-equivalent probe Pods selected by each policy. A known HTTPS egress canary must succeed before policy installation and fail afterward by connection denial or timeout; DNS failure is not sufficient.
3. From the restricted sandbox probe, make an authenticated Kubernetes API request and authenticated `GET ${RHOAI_VLLM_BASE_URL}/models`; both must succeed and the model response must include `RHOAI_VLLM_MODEL`.
4. During the suite, fail if any created bare sandbox Pod lacks `agentic.openshift.io/run`, because that Pod would be outside the tested boundary.
5. As defense in depth after pre-creation validation, inspect every created sandbox Pod and fail if its container image or OCI image-volume pullspec is outside the cluster-local registry allowlist. Image-pull failures or evidence of an external registry request fail the suite; the harness must not retry with a public pullspec.

After preflight, invoke the existing `product_e2e`-tagged tests with `E2E_SCENARIO_TAGS=core`. Do not invoke the mock-agent `e2e` suite.

## Provider Fixture

[PLANNED: OLS-3472] Add a RHOAI/vLLM fixture mode to the existing real-provider setup:

1. Create a Secret in the operator namespace with key `OPENAI_API_KEY`, populated from secret CI input `VLLM_API_KEY`.
2. Create `LLMProvider.spec.type=OpenAI` with `spec.openAI.url=RHOAI_VLLM_BASE_URL` and the Secret reference.
3. Create the selected `Agent` with `spec.model=RHOAI_VLLM_MODEL`.
4. Reuse the existing automatic approval, AgenticRun creation, phase, condition, and result-CR assertions.

The key and Secret contents MUST NOT be written to the provisioning output file, command traces, logs, or artifacts.

## Failure Diagnostics and Cleanup

Provisioning failures SHOULD collect GPU capacity, RHOAI/KServe readiness, `ServingRuntime`/`InferenceService` status, Kubernetes events, and vLLM Pod logs. Restricted-runtime failures SHOULD additionally collect NetworkPolicies and selector results, AgenticRun phase/conditions, result CRs, sandbox Pod status/logs, termination messages, and existing OTEL product-e2e artifacts.

Artifact collection MUST happen before destructive cleanup and redact credentials. Cleanup is best-effort and removes probe resources and temporary NetworkPolicies without replacing the original test result. A failure after restriction MUST NOT trigger a retry with external access restored.

## Out of Scope

- LSEval, LLM judges, response-quality thresholds, or new scenario assertions
- A Gemma-specific provider CRD or sandbox adapter
- `sandbox-claim` mode coverage
- A disconnected-only scenario list
- Production bundle changes already owned by the OCP 5.x release flow
