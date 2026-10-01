# E2E Testing with Local Checkout Repositories

Run end-to-end tests on an OpenShift cluster using locally built images for the operator, sandbox, and console.

## Prerequisites

- `oc` CLI logged into an OpenShift cluster with cluster-admin
- `podman` (or `docker`) on PATH
- Local checkouts of the repositories you want to test
- An LLM API key (for real agent tests) or the mock agent image (for automated e2e)

## Setup: Build and Push Local Images

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

## Option A: Full Deployment with Quickstart

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

## Option B: Operator Runs Locally (Faster Iteration)

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

## Configure LLM Provider

Pick one provider and configure it:

### OpenAI
```bash
oc create secret generic llm-creds-openai -n openshift-lightspeed \
  --from-literal=OPENAI_API_KEY=sk-...
oc apply -f hack/quickstart/examples/openai.yaml
```

### Anthropic (via Vertex AI)
```bash
oc create secret generic llm-creds-vertex -n openshift-lightspeed \
  --from-file=GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa-key.json
oc apply -f hack/quickstart/examples/vertex-anthropic.yaml
```

## Submit a Test AgenticRun

```bash
oc apply -f hack/quickstart/examples/deploy-test-workload.yaml
```

## Watch the Lifecycle

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

### Expected Phase Timeline

```
Pending → Analyzing (sandbox pod runs analysis agent)
       → Proposed  (analysis done, awaiting execution approval)
       → Executing (sandbox pod runs execution agent, after approval)
       → Verifying (sandbox pod runs verification agent, if configured)
       → Completed
```

## Automated E2E Tests (Mock Agent, No Real LLM)

For CI or automated testing, use the pre-built mock agent image instead of a real LLM:

```bash
# Run operator with mock agent
SANDBOX_IMAGE=quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent \
make run &

# Run the e2e test suite
make test-e2e
```

## Cleanup

```bash
# Delete the test run
oc delete agenticrun deploy-test-workload -n openshift-lightspeed

# Full teardown
bash hack/quickstart/uninstall.sh
# Or: make undeploy
```

## Environment Variables Reference

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

## Multicluster E2E (developer preview)

For Developer Preview, `make mc-product-e2e` runs a live-cluster integration
test with a real LLM provider. More thorough testing strategies, including
mocked E2E tests and CI integration, are planned for Technology Preview (TP)
and General Availability (GA).

A configured multicluster deployment with at least one registered spoke is
required. Run preflight to check the remaining prerequisites before running
the test:

```bash
export MC_HUB_KUBECONFIG=/path/to/hub.kubeconfig
export MC_SPOKE_KUBECONFIG=/path/to/spoke.kubeconfig
make mc-product-e2e-preflight
make mc-product-e2e
```

The test verifies Analysis, Execution and Verification against the spoke,
checks a spoke-side proof, and cleans up its own run and namespace. If preflight
finds multiple matching spokes, set `MC_SPOKE_NAME` to select one. Self-spoke
runs can use the same kubeconfig for both variables, but do not validate
connectivity between separate clusters. If cleanup fails, inspect the reported
resources rather than deleting shared prerequisites.
