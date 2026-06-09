#!/usr/bin/env bash
#
# Quickstart installer for Agentic OLS.
# Deploys the operator and its CRDs onto an OpenShift cluster using
# pre-built Konflux images. No building, no cloning.
#
# Usage:
#   curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/install.sh | bash
#
# Prerequisites:
#   - oc CLI on PATH
#   - Logged into the target OpenShift cluster
#   - cluster-admin privileges

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-operator:main}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-sandbox:main}"
SKILLS_IMAGE="${SKILLS_IMAGE:-quay.io/harpatil/agentic-skills:latest}"
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v0.4.5}"
AGENT_SANDBOX_BASE="https://github.com/kubernetes-sigs/agent-sandbox/releases/download"

GITHUB_RAW="https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main"

CRD_FILES=(
  agentic.openshift.io_agents.yaml
  agentic.openshift.io_analysisresults.yaml
  agentic.openshift.io_approvalpolicies.yaml
  agentic.openshift.io_escalationresults.yaml
  agentic.openshift.io_executionresults.yaml
  agentic.openshift.io_llmproviders.yaml
  agentic.openshift.io_proposalapprovals.yaml
  agentic.openshift.io_proposals.yaml
  agentic.openshift.io_verificationresults.yaml
)

info()  { echo "  ✓ $*"; }
step()  { echo "[${1}] ${2}"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

# --- Step 1: Prerequisites ---------------------------------------------------

step "1/6" "Checking prerequisites..."

command -v oc >/dev/null 2>&1 || fail "oc CLI not found. Install it first."
info "oc CLI found"

oc whoami >/dev/null 2>&1 || fail "Not logged into a cluster. Run: oc login ..."
info "Logged in as $(oc whoami)"

if ! oc auth can-i create clusterrolebindings >/dev/null 2>&1; then
  fail "Current user lacks cluster-admin privileges."
fi
info "cluster-admin privileges confirmed"

# --- Step 2: Agent Sandbox controller -----------------------------------------

step "2/6" "Installing Agent Sandbox controller (${AGENT_SANDBOX_VERSION})..."

if oc get crd sandboxclaims.extensions.agents.x-k8s.io >/dev/null 2>&1 \
  && oc get crd sandboxes.agents.x-k8s.io >/dev/null 2>&1 \
  && oc get crd sandboxtemplates.extensions.agents.x-k8s.io >/dev/null 2>&1; then
  info "Agent Sandbox CRDs already present (skipped)"
else
  oc apply -f "${AGENT_SANDBOX_BASE}/${AGENT_SANDBOX_VERSION}/manifest.yaml"
  oc apply -f "${AGENT_SANDBOX_BASE}/${AGENT_SANDBOX_VERSION}/extensions.yaml"
  info "Agent Sandbox ${AGENT_SANDBOX_VERSION} installed"
fi

# --- Step 3: Agentic Operator CRDs -------------------------------------------

step "3/6" "Installing Agentic Operator CRDs..."

for crd in "${CRD_FILES[@]}"; do
  oc apply -f "${GITHUB_RAW}/config/crd/bases/${crd}"
done
info "${#CRD_FILES[@]} CRDs applied"

# --- Step 4: Namespace + operator deployment ----------------------------------

step "4/6" "Deploying operator to ${NAMESPACE}..."

oc create namespace "${NAMESPACE}" 2>/dev/null && info "Namespace created" || info "Namespace already exists"

oc apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: lightspeed-agentic-operator
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agentic-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: lightspeed-agentic-operator
  namespace: ${NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lightspeed-agentic-operator
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: lightspeed-agentic-operator
  template:
    metadata:
      labels:
        app: lightspeed-agentic-operator
    spec:
      serviceAccountName: lightspeed-agentic-operator
      containers:
      - name: manager
        image: ${OPERATOR_IMAGE}
        args:
        - "--namespace=${NAMESPACE}"
        - "--sandbox-mode=sandbox-claim"
        - "--agentic-sandbox-image=${SANDBOX_IMAGE}"
        ports:
        - name: metrics
          containerPort: 8080
          protocol: TCP
        - name: health
          containerPort: 8081
          protocol: TCP
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
EOF
info "Operator deployment applied"

# --- Step 5: ApprovalPolicy --------------------------------------------------

step "5/6" "Creating ApprovalPolicy..."

oc apply -f - <<'EOF'
apiVersion: agentic.openshift.io/v1alpha1
kind: ApprovalPolicy
metadata:
  name: cluster
spec:
  maxAttempts: 3
  maxConcurrentProposals: 5
  stages:
  - name: Analysis
    approval: Automatic
  - name: Execution
    approval: Manual
EOF
info "ApprovalPolicy created"

# --- Step 6: Wait for operator ------------------------------------------------

step "6/6" "Waiting for operator to become ready..."

if oc rollout status deployment/lightspeed-agentic-operator \
    -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1; then
  info "Operator is running"
else
  echo ""
  echo "  ⚠ Operator did not become ready within 120s."
  echo "    Check logs: oc logs deployment/lightspeed-agentic-operator -n ${NAMESPACE}"
  echo ""
fi

# --- Done ---------------------------------------------------------------------

EXAMPLES_BASE="${GITHUB_RAW}/hack/quickstart/examples"

cat <<DONE

════════════════════════════════════════════════════════════════
  Agentic OLS installed successfully!

  Namespace:      ${NAMESPACE}
  Operator image: ${OPERATOR_IMAGE}
  Sandbox image:  ${SANDBOX_IMAGE}
════════════════════════════════════════════════════════════════

  NEXT: Configure your LLM provider. Pick one:

  ── Vertex AI / Claude ─────────────────────────────────────
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS=/path/to/adc.json
  curl -sLO ${EXAMPLES_BASE}/vertex-anthropic.yaml
  # Edit vertex-anthropic.yaml — set your GCP project ID
  oc apply -f vertex-anthropic.yaml

  ── Vertex AI / Gemini ─────────────────────────────────────
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS=/path/to/adc.json
  curl -sLO ${EXAMPLES_BASE}/vertex-google.yaml
  # Edit vertex-google.yaml — set your GCP project ID
  oc apply -f vertex-google.yaml

  ── OpenAI ─────────────────────────────────────────────────
  oc create secret generic llm-creds-openai -n ${NAMESPACE} \\
    --from-literal=OPENAI_API_KEY=sk-...
  curl -sLO ${EXAMPLES_BASE}/openai.yaml
  oc apply -f openai.yaml

  ── Azure OpenAI ───────────────────────────────────────────
  oc create secret generic llm-creds-azure -n ${NAMESPACE} \\
    --from-literal=AZURE_OPENAI_API_KEY=...
  curl -sLO ${EXAMPLES_BASE}/azure.yaml
  # Edit azure.yaml — set your endpoint and API version
  oc apply -f azure.yaml

  ── AWS Bedrock ────────────────────────────────────────────
  oc create secret generic llm-creds-bedrock -n ${NAMESPACE} \\
    --from-literal=AWS_ACCESS_KEY_ID=... \\
    --from-literal=AWS_SECRET_ACCESS_KEY=...
  curl -sLO ${EXAMPLES_BASE}/bedrock.yaml
  # Edit bedrock.yaml — set your AWS region
  oc apply -f bedrock.yaml

  ── Then submit a proposal ─────────────────────────────────
  oc apply -f - <<'PROPOSAL'
  apiVersion: agentic.openshift.io/v1alpha1
  kind: Proposal
  metadata:
    name: hello-test
    namespace: ${NAMESPACE}
  spec:
    request: "Check the health of this OpenShift cluster and report any issues."
    targetNamespaces:
    - ${NAMESPACE}
    analysis:
      agent: default
  PROPOSAL

  oc get proposals -n ${NAMESPACE} -w

  ── To uninstall ───────────────────────────────────────────
  curl -sL ${GITHUB_RAW}/hack/quickstart/uninstall.sh | bash

DONE
