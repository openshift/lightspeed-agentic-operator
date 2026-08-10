#!/usr/bin/env bash
#
# Deploy the agentic operator as a standalone workload.
# Installs CRDs, creates the operator Deployment, agent RBAC,
# ApprovalPolicy, webhook resources, and suspension ValidatingAdmissionPolicy.
#
# Can be called from install.sh or run independently.
#
# Usage:
#   bash hack/quickstart/deploy-operator.sh
#   bash hack/quickstart/deploy-operator.sh --image=quay.io/my-org/my-operator:tag
#
# Prerequisites:
#   - oc CLI on PATH, logged into an OpenShift cluster
#   - Namespace openshift-lightspeed exists
#
# Flags:
#   --image=IMAGE   Operator image (default: Konflux :main)

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OPERATOR_IMAGE="quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-operator:main"
GITHUB_RAW="https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main"

while [ $# -gt 0 ]; do
  case "$1" in
    --image=*) OPERATOR_IMAGE="${1#*=}"; shift ;;
    --image)   [ $# -lt 2 ] && { echo "Missing value for $1" >&2; exit 1; }; OPERATOR_IMAGE="$2"; shift 2 ;;
    *) echo "Unknown flag: $1" >&2; exit 1 ;;
  esac
done

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

# Prefer local CRD files when running from a checkout; fall back to GitHub.
REPO_ROOT=""
if [ -n "${BASH_SOURCE[0]:-}" ]; then
  _candidate="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  if [ -d "${_candidate}/config/crd/bases" ]; then
    REPO_ROOT="${_candidate}"
  fi
fi

info()  { echo "  ✓ $*"; }
step()  { echo "[operator] $*"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

step "Deploying agentic operator to ${NAMESPACE}"
step "Image: ${OPERATOR_IMAGE}"

# --- CRDs --------------------------------------------------------------------

step "Installing CRDs..."
if [ -n "${REPO_ROOT}" ]; then
  oc apply -f "${REPO_ROOT}/config/crd/bases/"
  info "CRDs applied (from local checkout)"
else
  for crd in "${CRD_FILES[@]}"; do
    oc apply -f "${GITHUB_RAW}/config/crd/bases/${crd}"
  done
  info "${#CRD_FILES[@]} CRDs applied (from GitHub)"
fi

# --- Webhook Service (must precede Deployment so service-ca generates certs) --

step "Creating webhook Service..."
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
info "Webhook Service created (cert generation triggered)"

# --- Operator Deployment -----------------------------------------------------

step "Creating operator SA, RBAC, and Deployment..."
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
        imagePullPolicy: Always
        args:
        - "--namespace=${NAMESPACE}"
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

# --- Agent read RBAC ---------------------------------------------------------

step "Binding read permissions to lightspeed-agent SA..."
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

# --- ApprovalPolicy ----------------------------------------------------------

step "Creating ApprovalPolicy..."
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

# --- Wait for operator --------------------------------------------------------

step "Waiting for operator rollout..."
if ! oc rollout status deployment/lightspeed-agentic-operator \
    -n "${NAMESPACE}" --timeout=300s; then
  fail "Agentic operator did not become ready.

  Check:
    oc logs deployment/lightspeed-agentic-operator -n ${NAMESPACE}
    oc get configmap lightspeed-agentic-configuration -n ${NAMESPACE}"
fi
info "Agentic operator is running"

# --- Webhook Configuration (after operator is ready) -------------------------

step "Registering MutatingWebhookConfiguration..."
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

# --- Suspension ValidatingAdmissionPolicy (OLS-3267) -------------------------

step "Installing AgenticRun suspension ValidatingAdmissionPolicy..."
if [ -n "${REPO_ROOT}" ]; then
  oc apply -f "${REPO_ROOT}/config/admission/agenticrun-suspension-policy.yaml"
  oc apply -f "${REPO_ROOT}/config/admission/agenticrun-suspension-binding.yaml"
  info "ValidatingAdmissionPolicy applied (from local checkout)"
else
  oc apply -f "${GITHUB_RAW}/config/admission/agenticrun-suspension-policy.yaml"
  oc apply -f "${GITHUB_RAW}/config/admission/agenticrun-suspension-binding.yaml"
  info "ValidatingAdmissionPolicy applied (from GitHub)"
fi

info "Agentic operator deployed successfully"
