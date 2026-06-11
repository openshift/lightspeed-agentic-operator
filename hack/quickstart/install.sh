#!/usr/bin/env bash
#
# Quickstart installer for Agentic OLS.
# Deploys the operator and its CRDs onto an OpenShift cluster using
# pre-built Konflux images. No building, no cloning.
#
# Usage:
#   bash <(curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/install.sh)
#
# Prerequisites:
#   - oc CLI on PATH
#   - Logged into the target OpenShift cluster
#   - cluster-admin privileges
#
# Note: The console plugin requires OpenShift 4.21+.
#       Set CONSOLE_IMAGE="" to skip console deployment on older clusters.

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-operator:main}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-sandbox:main}"
CONSOLE_IMAGE="${CONSOLE_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-console:main}"
SANDBOX_MODE="${SANDBOX_MODE:-bare-pod}"
IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-}"

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

step "1/5" "Checking prerequisites..."

command -v oc >/dev/null 2>&1 || fail "oc CLI not found. Install it first."
info "oc CLI found"

oc whoami >/dev/null 2>&1 || fail "Not logged into a cluster. Run: oc login ..."
info "Logged in as $(oc whoami)"

if ! oc auth can-i create clusterrolebindings >/dev/null 2>&1; then
  fail "Current user lacks cluster-admin privileges."
fi
info "cluster-admin privileges confirmed"

# --- Step 2: Agentic Operator CRDs --------------------------------------------

step "2/5" "Installing Agentic Operator CRDs..."

for crd in "${CRD_FILES[@]}"; do
  oc apply -f "${GITHUB_RAW}/config/crd/bases/${crd}"
done
info "${#CRD_FILES[@]} CRDs applied"

# --- Step 3: Namespace + operator deployment ----------------------------------

step "3/5" "Deploying operator to ${NAMESPACE} (sandbox-mode=${SANDBOX_MODE})..."

if oc create namespace "${NAMESPACE}" 2>/dev/null; then
  info "Namespace created"
elif oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  info "Namespace already exists"
else
  fail "Failed to create namespace ${NAMESPACE}"
fi

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
      securityContext:
        runAsNonRoot: true
      containers:
      - name: manager
        image: ${OPERATOR_IMAGE}
$([ -n "${IMAGE_PULL_POLICY}" ] && echo "        imagePullPolicy: ${IMAGE_PULL_POLICY}")
        args:
        - "--namespace=${NAMESPACE}"
        - "--sandbox-mode=${SANDBOX_MODE}"
        - "--agentic-sandbox-image=${SANDBOX_IMAGE}"
        - "--agentic-console-image=${CONSOLE_IMAGE}"
$([ -n "${IMAGE_PULL_POLICY}" ] && echo '        - "--image-pull-policy='"${IMAGE_PULL_POLICY}"'"')
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
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 10m
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
            - ALL
        env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
EOF
info "Operator deployment applied"

# --- Step 4: ApprovalPolicy ---------------------------------------------------

step "4/5" "Creating ApprovalPolicy..."

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

# --- Step 5: Wait for operator ------------------------------------------------

step "5/5" "Waiting for operator to become ready..."

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

  Namespace     : ${NAMESPACE}
  Sandbox mode  : ${SANDBOX_MODE}
  Operator image: ${OPERATOR_IMAGE}
  Sandbox image : ${SANDBOX_IMAGE}
  Console image : ${CONSOLE_IMAGE}

  > Console works only on OpenShift 4.21+
════════════════════════════════════════════════════════════════

  NEXT: Configure your LLM provider. Pick one:

  ── Vertex AI / Claude ─────────────────────────────────────
  export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/service-account-key.json
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS="\$GOOGLE_APPLICATION_CREDENTIALS"
  curl -sLO ${EXAMPLES_BASE}/vertex-anthropic.yaml
  # Edit vertex-anthropic.yaml — set your GCP project ID and region
  oc apply -f vertex-anthropic.yaml

  ── Vertex AI / Gemini ─────────────────────────────────────
  export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/service-account-key.json
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS="\$GOOGLE_APPLICATION_CREDENTIALS"
  curl -sLO ${EXAMPLES_BASE}/vertex-google.yaml
  # Edit vertex-google.yaml — set your GCP project ID and region
  oc apply -f vertex-google.yaml

  ── OpenAI ─────────────────────────────────────────────────
  oc create secret generic llm-creds-openai -n ${NAMESPACE} \\
    --from-literal=OPENAI_API_KEY=sk-...
  curl -sLO ${EXAMPLES_BASE}/openai.yaml
  oc apply -f openai.yaml

  ── Then submit example proposal ───────────────────────────
  curl -sLO ${EXAMPLES_BASE}/namespace-inventory.yaml
  # Edit namespace-inventory.yaml if NAMESPACE is not openshift-lightspeed
  oc apply -f namespace-inventory.yaml

  # Watch until analysis completes (Analyzed=True):
  oc get proposals -n ${NAMESPACE} -w

  # Check the analysis result:
  oc get analysisresult -n ${NAMESPACE} -o json

  # Approve execution (option 0 = first option) via oc CLI or in console UI:
  oc patch proposalapproval namespace-inventory -n ${NAMESPACE} \\
    --type=json \\
    -p '[{"op":"add","path":"/spec/stages/-","value":{"type":"Execution","execution":{"option":0}}}]'

  # Watch execution progress:
  oc get proposals -n ${NAMESPACE} -w

  ── To uninstall ───────────────────────────────────────────
  bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/uninstall.sh)

DONE
