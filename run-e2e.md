# End-to-End Testing

## Multicluster product E2E (real provider)

`make mc-product-e2e` runs one real-provider AgenticRun against a registered spoke. It
accepts a separate hub and spoke or a self-referencing spoke on one cluster. A
self-spoke run checks the workflow, but not connectivity between two clusters.
This is not a Prow/Konflux job or the full hosted multi-spoke T2 suite. The test
does not install operators, register spokes, set policy, or create provider
credentials.

| Jira check | Assertion in the single run |
|---|---|
| spoke identity and RBAC | ephemeral per-step SAs, reader bindings, execution Role/RoleBinding and scoped authorization |
| hub sandbox targeting spoke | run-owned kubeconfig Secret mounted in hub Pod, valid token and exact proof read via spoke client |
| complete lifecycle | non-skipped Analysis, Execution and Verification results, Passed checks, Automatic approval policy |
| cleanup | operator releases per-step access, test deletes only its own run and proof namespace with UID checks |
| token safety | actual sandbox bearer token is valid and expires no later than 24h after issue |

### Prerequisites

- hub and spoke kubeconfigs, either for separate OpenShift clusters or the same
  cluster in self-spoke mode; the test actor needs permissions to create/get/delete
  hub AgenticRuns and an owned spoke namespace, read observed results, RBAC,
  Pods and Secrets, and issue spoke SubjectAccessReviews (preflight checks
  required permissions); hub SpokeCluster list permission is needed for
  automatic discovery, or supply `MC_SPOKE_NAME` if only get is allowed
- running agentic operator and lightspeed-hub on the hub, their CRDs, active
  `openshift-lightspeed` namespace, `HubConfig/cluster`, and one hub Deployment
  labeled `app.kubernetes.io/name=lightspeed-hub` that is ready; the agentic
  operator must run sandbox Pods in `openshift-lightspeed`, including when it
  runs locally via `make run` instead of a Deployment
- a registered, ready `SpokeCluster` with Connected, Provisioned,
  AdaptersReady and Ready conditions True, its hub
  `openshift-lightspeed/spoke-kubeconfig-<name>` Secret pointing at the supplied
  spoke cluster with permission to read its `kube-system` namespace identity,
  and active `openshift-lightspeed-managed` namespace with the hub-provisioned
  reader bindings there
- hub ConfigMap `openshift-lightspeed/lightspeed-agentic-configuration` with
  bare-pod mode and a real non-mock sandbox image; `Agent/default` with a model
  and a reachable real `LLMProvider` plus referenced credentials Secret
- `ApprovalPolicy/cluster` with Automatic Analysis, Execution and Verification;
  real provider proposals execute automatically in the test-owned namespace

### Run

From this checkout, pass only explicit kubeconfigs, with no fallback to the
current `oc` context or `~/.kube/config`. In self-spoke mode, both variables
can point at the same file. The test discovers a ready registered SpokeCluster
whose standing kubeconfig reaches the supplied spoke cluster. If more than one
registration matches, set `MC_SPOKE_NAME` to choose one; it must still match the
supplied kubeconfig. A ready registration whose credentials cannot be checked
also fails closed; selecting a name limits probing to that registration.
Preflight fails before creating fixtures on a mismatch.

```bash
export MC_HUB_KUBECONFIG=/path/to/hub.kubeconfig
export MC_SPOKE_KUBECONFIG=/path/to/spoke.kubeconfig
make mc-product-e2e-preflight
make mc-product-e2e
```

Preflight performs reads and transient authorization reviews only; it does
not install or repair prerequisites. The full target reruns preflight, creates
one uniquely named hub AgenticRun and spoke namespace, and emits named PASS/FAIL
subtests for proof, each stage's identity/token, and artifact release. Its
request describes a missing proof ConfigMap, not a hard-coded shell command.
The exact labeled value must be present in the owned target namespace when
read via the spoke client and absent from the hub **operator namespace**. On a
self-spoke cluster, the target namespace also lives on the hub; do not treat a
self-spoke pass as evidence that the proof is absent from the hub cluster.

### Cleanup and limitations

The operator releases per-step SA/RBAC/Pod/Secret resources after each step.
The test checks their absence, then uses normal finalizers and UID-checked
cleanup for its own run and namespace even if an assertion fails. It does not
force-remove finalizers or delete global prerequisites. If cleanup cannot be
confirmed, inspect the labeled resources before retrying, do not assume a
failed run left zero resources or run a bulk teardown.

```bash
KUBECONFIG="$MC_HUB_KUBECONFIG" oc get agenticruns -n openshift-lightspeed
KUBECONFIG="$MC_SPOKE_KUBECONFIG" oc get namespaces
KUBECONFIG="$MC_HUB_KUBECONFIG" oc get agenticruns -n openshift-lightspeed -l agentic.openshift.io/mc-e2e
KUBECONFIG="$MC_SPOKE_KUBECONFIG" oc get namespaces -l agentic.openshift.io/mc-e2e
```

Do not run the separate `make test-e2e` suite on an already prepared hub: its
fixture setup changes global resources. Ordinary `make test` does not run
either E2E suite. Real LLM results vary, and the operator must be running;
without it, the bounded live test will time out rather than a Deployment
preflight check failing.

---

## Legacy local checkout workflow

Run end-to-end tests on an OpenShift cluster using locally built images for the operator, sandbox, and console.

### Prerequisites

- `oc` CLI logged into an OpenShift cluster with cluster-admin
- `podman` (or `docker`) on PATH
- Local checkouts of the repositories you want to test
- An LLM API key (for real agent tests) or the mock agent image (for automated e2e)

### Setup: Build and Push Local Images

```bash
export KUBECONFIG=/path/to/kubeconfig

# 1. Create namespace
oc create namespace openshift-lightspeed 2>/dev/null || true

# 2. Expose the internal registry and get a push token
oc patch configs.imageregistry.operator.openshift.io/cluster \
  --type=merge -p '{"spec":{"defaultRoute":true}}'

# Wait for the route
REGISTRY=$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}')
echo "Registry: $REGISTRY"

# Create a service account for pushing images
oc create sa image-pusher -n openshift-lightspeed 2>/dev/null || true
oc adm policy add-role-to-user system:image-builder -z image-pusher -n openshift-lightspeed 2>/dev/null || true
TOKEN=$(oc create token image-pusher -n openshift-lightspeed --duration=1h)
podman login -u image-pusher -p "$TOKEN" "$REGISTRY" --tls-verify=false

# 3. Build + push SANDBOX image
cd /path/to/lightspeed-agentic-sandbox
podman build -t $REGISTRY/openshift-lightspeed/agentic-sandbox:latest .
podman push $REGISTRY/openshift-lightspeed/agentic-sandbox:latest --tls-verify=false

# 4. Build + push OPERATOR image
cd /path/to/lightspeed-agentic-operator
podman build -t $REGISTRY/openshift-lightspeed/agentic-operator:latest .
podman push $REGISTRY/openshift-lightspeed/agentic-operator:latest --tls-verify=false

# 5. Build + push CONSOLE image (optional, skip with CONSOLE_IMAGE="")
cd /path/to/lightspeed-agentic-console
podman build -t $REGISTRY/openshift-lightspeed/agentic-console:latest .
podman push $REGISTRY/openshift-lightspeed/agentic-console:latest --tls-verify=false
```

### Option A: Full Deployment with Quickstart

Deploy the operator, console, and webhook in-cluster using the quickstart script with local images:

```bash
cd /path/to/lightspeed-agentic-operator
INTERNAL=image-registry.openshift-image-registry.svc:5000/openshift-lightspeed

OPERATOR_IMAGE=$INTERNAL/agentic-operator:latest \
SANDBOX_IMAGE=$INTERNAL/agentic-sandbox:latest \
CONSOLE_IMAGE=$INTERNAL/agentic-console:latest \
IMAGE_PULL_POLICY=Always \
bash hack/quickstart/install.sh
```

To skip console deployment, set `CONSOLE_IMAGE=""`.

### Option B: Operator Runs Locally (Faster Iteration)

Skip building/pushing the operator image. The operator runs on your workstation and connects to the cluster via KUBECONFIG:

```bash
cd /path/to/lightspeed-agentic-operator
INTERNAL=image-registry.openshift-image-registry.svc:5000/openshift-lightspeed

# Install CRDs
make install

# Run the operator locally
SANDBOX_IMAGE=$INTERNAL/agentic-sandbox:latest \
IMAGE_PULL_POLICY=Always \
make run
```

### Configure LLM Provider

Pick one provider and configure it:

#### OpenAI
```bash
oc create secret generic llm-creds-openai -n openshift-lightspeed \
  --from-literal=OPENAI_API_KEY=sk-...
oc apply -f hack/quickstart/examples/openai.yaml
```

#### Anthropic (via Vertex AI)
```bash
oc create secret generic llm-creds-vertex -n openshift-lightspeed \
  --from-file=GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa-key.json
oc apply -f hack/quickstart/examples/vertex-anthropic.yaml
```

### Submit a Test AgenticRun

```bash
oc apply -f hack/quickstart/examples/deploy-test-workload.yaml
```

### Watch the Lifecycle

Open separate terminals:

```bash
# Terminal 1: Watch phase transitions
oc get agenticruns -n openshift-lightspeed -w

# Terminal 2: Watch sandbox pods
oc get pods -n openshift-lightspeed -w -l agentic.openshift.io/run

# Terminal 3: Interact
# Wait for phase "Proposed", then approve execution:
oc agentic run approve deploy-test-workload --stage=execution --option=0

# Stream sandbox logs:
oc agentic run logs deploy-test-workload -f

# Check detailed status:
oc agentic run get deploy-test-workload
```

#### Expected Phase Timeline

```
Pending → Analyzing (sandbox pod runs analysis agent)
       → Proposed  (analysis done, awaiting execution approval)
       → Executing (sandbox pod runs execution agent, after approval)
       → Verifying (sandbox pod runs verification agent, if configured)
       → Completed
```

### Automated E2E Tests (Mock Agent, No Real LLM)

For CI or automated testing, use the pre-built mock agent image instead of a real LLM:

```bash
# Run operator with mock agent
SANDBOX_IMAGE=quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent \
make run &

# Run the e2e test suite
make test-e2e
```

### Cleanup

```bash
# Delete the test run
oc delete agenticrun deploy-test-workload -n openshift-lightspeed

# Full teardown
bash hack/quickstart/uninstall.sh
# Or: make undeploy
```

### Environment Variables Reference

| Variable | Default | Description |
|---|---|---|
| `KUBECONFIG` | `~/.kube/config` | Cluster access |
| `NAMESPACE` | `openshift-lightspeed` | Target namespace |
| `OPERATOR_IMAGE` | Konflux `:main` | Operator container image |
| `SANDBOX_IMAGE` | Konflux `:main` | Agent sandbox container image |
| `CONSOLE_IMAGE` | Konflux `:main` | Console plugin image (set `""` to skip) |
| `SANDBOX_MODE` | `bare-pod` | `bare-pod` or `sandbox-claim` |
| `IMAGE_PULL_POLICY` | *(K8s default)* | `Always`, `IfNotPresent`, or `Never` |
| `TEST_NAMESPACE` | `openshift-lightspeed` | Namespace for e2e test CRs |
| `E2E_POLL_TIMEOUT` | `10m` | How long e2e tests wait for phase transitions |
| `E2E_PROVIDER` | *(empty = mock)* | `claude`, `gemini`, or `openai` for real LLM |
| `E2E_MODEL` | - | Required with `E2E_PROVIDER` |
| `E2E_PROVIDER_KEY_PATH` | - | Credentials file, required with `E2E_PROVIDER` |
