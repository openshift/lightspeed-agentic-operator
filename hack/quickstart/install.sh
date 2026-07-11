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
# Note: The console plugin requires OpenShift 4.22+.
#       Set CONSOLE_IMAGE="" to skip console deployment on older clusters.
#       The console is deployed as a standalone workload (not operator-managed).

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-operator:main}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-sandbox:main}"
CONSOLE_IMAGE="${CONSOLE_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-console:main}"
SANDBOX_MODE="${SANDBOX_MODE:-bare-pod}"
IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-}"

GITHUB_RAW="${GITHUB_RAW:-https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main}"

CRD_FILES=(
  agentic.openshift.io_agenticolsconfigs.yaml
  agentic.openshift.io_agents.yaml
  agentic.openshift.io_analysisresults.yaml
  agentic.openshift.io_approvalpolicies.yaml
  agentic.openshift.io_escalationresults.yaml
  agentic.openshift.io_executionresults.yaml
  agentic.openshift.io_llmproviders.yaml
  agentic.openshift.io_agenticrunapprovals.yaml
  agentic.openshift.io_agenticruns.yaml
  agentic.openshift.io_verificationresults.yaml
)

info()  { echo "  ✓ $*"; }
step()  { echo "[${1}] ${2}"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

# --- Step 1: Prerequisites ---------------------------------------------------

step "1/7" "Checking prerequisites..."

command -v oc >/dev/null 2>&1 || fail "oc CLI not found. Install it first."
info "oc CLI found"

oc whoami >/dev/null 2>&1 || fail "Not logged into a cluster. Run: oc login ..."
info "Logged in as $(oc whoami)"

if ! oc auth can-i create clusterrolebindings >/dev/null 2>&1; then
  fail "Current user lacks cluster-admin privileges."
fi
info "cluster-admin privileges confirmed"

# --- Step 2: Agentic Operator CRDs --------------------------------------------

step "2/7" "Installing Agentic Operator CRDs..."

# Prefer local CRD files when running from a checkout; fall back to GitHub.
REPO_ROOT=""
if [ -n "${BASH_SOURCE[0]:-}" ]; then
  _candidate="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  if [ -d "${_candidate}/config/crd/bases" ]; then
    REPO_ROOT="${_candidate}"
  fi
fi

for crd in "${CRD_FILES[@]}"; do
  if [ -n "${REPO_ROOT}" ]; then
    oc apply -f "${REPO_ROOT}/config/crd/bases/${crd}"
  else
    oc apply -f "${GITHUB_RAW}/config/crd/bases/${crd}"
  fi
done
if [ -n "${REPO_ROOT}" ]; then
  info "${#CRD_FILES[@]} CRDs applied (from local checkout)"
else
  info "${#CRD_FILES[@]} CRDs applied (from GitHub)"
fi

# --- Step 3: Namespace + operator deployment ----------------------------------

step "3/7" "Deploying operator to ${NAMESPACE} (sandbox-mode=${SANDBOX_MODE})..."

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
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: manager
        image: ${OPERATOR_IMAGE}
$([ -n "${IMAGE_PULL_POLICY}" ] && echo "        imagePullPolicy: ${IMAGE_PULL_POLICY}")
        args:
        - "--namespace=${NAMESPACE}"
        - "--sandbox-mode=${SANDBOX_MODE}"
        - "--agentic-sandbox-image=${SANDBOX_IMAGE}"
$([ -n "${IMAGE_PULL_POLICY}" ] && echo '        - "--image-pull-policy='"${IMAGE_PULL_POLICY}"'"')
        ports:
        - name: metrics
          containerPort: 8080
          protocol: TCP
        - name: health
          containerPort: 8081
          protocol: TCP
        - name: webhook
          containerPort: 9443
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
        volumeMounts:
        - name: webhook-certs
          mountPath: /tmp/k8s-webhook-server/serving-certs
          readOnly: true
        env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
      volumes:
      - name: webhook-certs
        secret:
          secretName: agentic-operator-webhook-certs
          optional: true
EOF
info "Operator deployment applied"

# --- Step 3b: Agent read RBAC -------------------------------------------------

info "Binding read permissions to lightspeed-agent SA..."

oc apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agent-cluster-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-reader
subjects:
- kind: ServiceAccount
  name: lightspeed-agent
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agent-monitoring-view
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-monitoring-view
subjects:
- kind: ServiceAccount
  name: lightspeed-agent
  namespace: ${NAMESPACE}
EOF
info "Agent read RBAC applied (cluster-reader + cluster-monitoring-view)"

# --- Step 4: ApprovalPolicy ---------------------------------------------------

step "4/7" "Creating ApprovalPolicy..."

oc apply -f - <<'EOF'
apiVersion: agentic.openshift.io/v1alpha1
kind: ApprovalPolicy
metadata:
  name: cluster
spec:
  maxAttempts: 3
  maxConcurrentRuns: 5
  stages:
  - name: Analysis
    approval: Automatic
  - name: Execution
    approval: Manual
  - name: Verification
    approval: Automatic
EOF
info "ApprovalPolicy created"

# --- Step 5: Webhook Service --------------------------------------------------

step "5/7" "Creating webhook Service..."

oc apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: agentic-operator-webhook-service
  namespace: ${NAMESPACE}
  annotations:
    service.beta.openshift.io/serving-cert-secret-name: agentic-operator-webhook-certs
spec:
  ports:
    - port: 443
      targetPort: 9443
      protocol: TCP
  selector:
    app: lightspeed-agentic-operator
EOF
info "Webhook Service created"

# --- Step 6: Wait for operator ------------------------------------------------

step "6/7" "Waiting for operator to become ready..."

if oc rollout status deployment/lightspeed-agentic-operator \
    -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1; then
  info "Operator is running"
else
  echo ""
  echo "  ⚠ Operator did not become ready within 120s."
  echo "    Check logs: oc logs deployment/lightspeed-agentic-operator -n ${NAMESPACE}"
  echo ""
fi

# --- Step 7: Webhook Configuration (after operator is ready) ------------------

step "7/7" "Registering fail-closed MutatingWebhookConfiguration..."

oc apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: agentic-operator-mutating-webhook
  annotations:
    service.beta.openshift.io/inject-cabundle: "true"
webhooks:
  - name: agenticrunapproval-mutator.agentic.openshift.io
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: ${NAMESPACE}
    clientConfig:
      service:
        name: agentic-operator-webhook-service
        namespace: ${NAMESPACE}
        path: /mutate-agenticrunapproval
    rules:
      - operations: ["UPDATE"]
        apiGroups: ["agentic.openshift.io"]
        apiVersions: ["v1alpha1"]
        resources: ["agenticrunapprovals"]
    failurePolicy: Fail
    sideEffects: None
    admissionReviewVersions: ["v1"]
EOF
info "MutatingWebhookConfiguration registered"

# --- Step: Deploy console plugin (standalone) --------------------------------

if [ -n "${CONSOLE_IMAGE}" ]; then
  SCRIPT_DIR=""
  if [ -n "${BASH_SOURCE[0]:-}" ]; then
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  fi
  if [ -n "${SCRIPT_DIR}" ] && [ -f "${SCRIPT_DIR}/deploy-console.sh" ]; then
    NAMESPACE="${NAMESPACE}" CONSOLE_IMAGE="${CONSOLE_IMAGE}" bash "${SCRIPT_DIR}/deploy-console.sh"
  else
    curl -fsL "${GITHUB_RAW}/hack/quickstart/deploy-console.sh" \
      | NAMESPACE="${NAMESPACE}" CONSOLE_IMAGE="${CONSOLE_IMAGE}" bash
  fi
else
  echo "  CONSOLE_IMAGE is empty — skipping console deployment"
fi

# --- Done ---------------------------------------------------------------------

if [ -n "${REPO_ROOT}" ] && [ -d "${REPO_ROOT}/hack/quickstart/examples" ]; then
  EXAMPLES_BASE="${REPO_ROOT}/hack/quickstart/examples"
else
  EXAMPLES_BASE="${GITHUB_RAW}/hack/quickstart/examples"
fi

cat <<DONE

════════════════════════════════════════════════════════════════
  Agentic OLS installed successfully!

  Namespace     : ${NAMESPACE}
  Sandbox mode  : ${SANDBOX_MODE}
  Operator image: ${OPERATOR_IMAGE}
  Sandbox image : ${SANDBOX_IMAGE}
  Console image : ${CONSOLE_IMAGE}

  > Console works only on OpenShift 4.22+
════════════════════════════════════════════════════════════════

  NEXT: Configure your LLM provider. Pick one:

  ── Vertex AI / Claude ─────────────────────────────────────
  export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/service-account-key.json
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS="\$GOOGLE_APPLICATION_CREDENTIALS"
  # Edit vertex-anthropic.yaml — set your GCP project ID and region
  oc apply -f ${EXAMPLES_BASE}/vertex-anthropic.yaml

  ── Vertex AI / Gemini ─────────────────────────────────────
  export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/service-account-key.json
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS="\$GOOGLE_APPLICATION_CREDENTIALS"
  # Edit vertex-google.yaml — set your GCP project ID and region
  oc apply -f ${EXAMPLES_BASE}/vertex-google.yaml

  ── OpenAI ─────────────────────────────────────────────────
  oc create secret generic llm-creds-openai -n ${NAMESPACE} \\
    --from-literal=OPENAI_API_KEY=sk-...
  oc apply -f ${EXAMPLES_BASE}/openai.yaml

  ── Then submit an example run ────────────────────────

  # Investigate namespace workloads, remediate if issues found:
  oc apply -f ${EXAMPLES_BASE}/namespace-inventory.yaml

  # Deploy a test workload (analysis + execution):
  oc apply -f ${EXAMPLES_BASE}/deploy-test-workload.yaml

  # Watch until analysis completes (Analyzed=True):
  oc get agenticruns -n ${NAMESPACE} -w

  # Check the analysis result:
  oc get analysisresult -n ${NAMESPACE} -o json

  # Approve execution (option 0 = first option) via oc CLI or in console UI:
  oc patch agenticrunapproval namespace-inventory -n ${NAMESPACE} \\
    --type=json \\
    -p '[{"op":"add","path":"/spec/stages/-","value":{"type":"Execution","execution":{"option":0}}}]'

  # Watch execution progress:
  oc get agenticruns -n ${NAMESPACE} -w

  ── Enable audit logging / OTEL tracing ────────────────────
  # Deploy Jaeger first (if you don't have a collector):
  bash <(curl -sL ${GITHUB_RAW}/hack/deploy-jaeger.sh)

  # Then enable audit logging + OTEL:
  OTEL_ENDPOINT=jaeger-otlp-grpc.observability.svc:4317 \\
    bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/setup-audit.sh)

  # Audit logging only (no OTEL):
  bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/setup-audit.sh)

  ── To uninstall ───────────────────────────────────────────
  bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/uninstall.sh)

DONE
