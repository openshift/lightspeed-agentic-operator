#!/usr/bin/env bash
# Konflux: install the agentic operator onto the ephemeral cluster using make targets.
# Run from the repo root after checkout.
#
# Required env:
#   IMG              — operator image (from SNAPSHOT)
#   KUBECONFIG       — path to kubeconfig
#
# Optional env:
#   OPERATOR_NAMESPACE    (default: openshift-lightspeed)
#   SANDBOX_MODE          (default: bare-pod)
#   SANDBOX_IMAGE         (default: quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent1)
#   OTEL_COLLECTOR_IMAGE  (default: quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-otel-collector:main)

set -euo pipefail

: "${IMG:?IMG must be set to the operator image}"
: "${KUBECONFIG:?KUBECONFIG must be set}"

OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-openshift-lightspeed}"
SANDBOX_MODE="${SANDBOX_MODE:-bare-pod}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent1}"
OTEL_COLLECTOR_IMAGE="${OTEL_COLLECTOR_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-otel-collector:main}"

echo "=== Agentic operator install ==="
echo "  IMG:                  ${IMG}"
echo "  OPERATOR_NAMESPACE:   ${OPERATOR_NAMESPACE}"
echo "  SANDBOX_MODE:         ${SANDBOX_MODE}"
echo "  SANDBOX_IMAGE:        ${SANDBOX_IMAGE}"
echo "  OTEL_COLLECTOR_IMAGE: ${OTEL_COLLECTOR_IMAGE}"
echo "================================="

# Ensure namespace exists.
oc create namespace "${OPERATOR_NAMESPACE}" --dry-run=client -o yaml | oc apply -f -

# Install CRDs.
echo "Installing CRDs..."
make install

# Install Agent Sandbox operator when sandbox-claim mode is requested.
if [ "${SANDBOX_MODE}" = "sandbox-claim" ]; then
  AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.0}"
  AGENT_SANDBOX_RELEASE_BASE="https://github.com/kubernetes-sigs/agent-sandbox/releases/download"
  echo "Installing Agent Sandbox operator ${AGENT_SANDBOX_VERSION}..."
  oc apply -f "${AGENT_SANDBOX_RELEASE_BASE}/${AGENT_SANDBOX_VERSION}/sandbox.yaml"
  oc apply -f "${AGENT_SANDBOX_RELEASE_BASE}/${AGENT_SANDBOX_VERSION}/extensions.yaml"
  echo "Waiting for Sandbox CRDs to be established..."
  oc wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=60s
  oc wait --for=condition=Established crd/sandboxclaims.extensions.agents.x-k8s.io --timeout=60s
  oc wait --for=condition=Established crd/sandboxtemplates.extensions.agents.x-k8s.io --timeout=60s
  oc wait --for=condition=Established crd/sandboxwarmpools.extensions.agents.x-k8s.io --timeout=60s
  echo "Waiting for Agent Sandbox controller to be ready..."
  oc rollout status deployment/agent-sandbox-controller -n agent-sandbox-system --timeout=120s
  echo "Agent Sandbox operator installed"
else
  echo "Skipping Agent Sandbox operator install (SANDBOX_MODE=${SANDBOX_MODE})"
fi

# Deploy operator (kustomize-based).
echo "Deploying operator..."
make deploy IMG="${IMG}" OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE}" SANDBOX_MODE="${SANDBOX_MODE}"

# Grant cluster-admin to operator SA (same as quickstart — covers escalation + SCC).
echo "Granting cluster-admin to operator SA..."
oc apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agentic-operator-admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: ${OPERATOR_NAMESPACE}
EOF

# Grant cluster-reader to agent SA (required by execution RBAC discovery).
echo "Granting cluster-reader to agent SA..."
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
  namespace: ${OPERATOR_NAMESPACE}
EOF

# --- OTEL Collector (debug exporter for trace verification) ---

echo "Deploying OTEL collector..."

# Collector runtime config: OTLP receiver with TLS + debug exporter.
oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: lightspeed-otel-collector-config
  namespace: ${OPERATOR_NAMESPACE}
data:
  config.yaml: |
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: "0.0.0.0:4317"
            tls:
              cert_file: /var/run/secrets/serving-cert/tls.crt
              key_file: /var/run/secrets/serving-cert/tls.key
    exporters:
      debug:
        verbosity: detailed
    extensions:
      health_check:
        endpoint: "0.0.0.0:13133"
    service:
      extensions: [health_check]
      pipelines:
        traces:
          receivers: [otlp]
          exporters: [debug]
        logs:
          receivers: [otlp]
          exporters: [debug]
EOF

# Service with serving-cert annotation — service-ca auto-generates TLS secret.
oc apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: lightspeed-otel-collector
  namespace: ${OPERATOR_NAMESPACE}
  annotations:
    service.beta.openshift.io/serving-cert-secret-name: lightspeed-otel-collector-cert
spec:
  selector:
    app: lightspeed-otel-collector
  type: ClusterIP
  ports:
  - name: otlp-grpc
    port: 4317
    protocol: TCP
    targetPort: otlp-grpc
  - name: admin
    port: 8443
    protocol: TCP
    targetPort: admin
EOF

# ConfigMap for CA bundle injection by service-ca.
oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: lightspeed-otel-collector-cabundle
  namespace: ${OPERATOR_NAMESPACE}
  annotations:
    service.beta.openshift.io/inject-cabundle: "true"
data: {}
EOF

echo "Waiting for OTEL collector TLS secret..."
for _ in $(seq 1 30); do
  if oc get secret lightspeed-otel-collector-cert -n "${OPERATOR_NAMESPACE}" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
oc get secret lightspeed-otel-collector-cert -n "${OPERATOR_NAMESPACE}" || {
  echo "ERROR: TLS secret not created by service-ca after 60s"
  exit 1
}

echo "Waiting for CA bundle injection..."
CA_BUNDLE=""
for _ in $(seq 1 30); do
  CA_BUNDLE=$(oc get configmap lightspeed-otel-collector-cabundle -n "${OPERATOR_NAMESPACE}" \
    -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || true)
  if [ -n "${CA_BUNDLE}" ]; then
    break
  fi
  sleep 2
done
if [ -z "${CA_BUNDLE}" ]; then
  echo "ERROR: CA bundle not injected after 60s"
  exit 1
fi

# Create the CA Secret the operator reads (key must be otel-ca.crt).
TMPCA=$(mktemp)
echo "${CA_BUNDLE}" > "${TMPCA}"
oc create secret generic lightspeed-otel-ca -n "${OPERATOR_NAMESPACE}" \
  --from-file=otel-ca.crt="${TMPCA}" --dry-run=client -o yaml | oc apply -f -
rm -f "${TMPCA}"

# Deployment.
oc apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lightspeed-otel-collector
  namespace: ${OPERATOR_NAMESPACE}
  labels:
    app: lightspeed-otel-collector
spec:
  replicas: 1
  selector:
    matchLabels:
      app: lightspeed-otel-collector
  template:
    metadata:
      labels:
        app: lightspeed-otel-collector
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
      containers:
      - name: otel-collector
        image: ${OTEL_COLLECTOR_IMAGE}
        args: ["--config=/etc/otel/config.yaml"]
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
          seccompProfile:
            type: RuntimeDefault
        ports:
        - name: otlp-grpc
          containerPort: 4317
          protocol: TCP
        - name: health
          containerPort: 13133
          protocol: TCP
        - name: admin
          containerPort: 8443
          protocol: TCP
        livenessProbe:
          httpGet:
            path: /
            port: health
          initialDelaySeconds: 10
          periodSeconds: 15
        readinessProbe:
          httpGet:
            path: /
            port: health
          initialDelaySeconds: 5
          periodSeconds: 10
        volumeMounts:
        - name: config
          mountPath: /etc/otel
          readOnly: true
        - name: serving-cert
          mountPath: /var/run/secrets/serving-cert
          readOnly: true
      volumes:
      - name: config
        configMap:
          name: lightspeed-otel-collector-config
      - name: serving-cert
        secret:
          secretName: lightspeed-otel-collector-cert
EOF

echo "Waiting for OTEL collector rollout..."
oc rollout status deployment/lightspeed-otel-collector -n "${OPERATOR_NAMESPACE}" --timeout=120s

OTEL_ENDPOINT="lightspeed-otel-collector.${OPERATOR_NAMESPACE}.svc:4317"
OTEL_ADMIN="https://lightspeed-otel-collector.${OPERATOR_NAMESPACE}.svc:8443"
echo "OTEL collector ready at ${OTEL_ENDPOINT}"

# Create the unified configuration ConfigMap.
echo "Creating lightspeed-agentic-configuration ConfigMap..."
oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: lightspeed-agentic-configuration
  namespace: ${OPERATOR_NAMESPACE}
data:
  sandbox-mode: "${SANDBOX_MODE}"
  sandbox-pod-spec: |
    {
      "containers": [{
        "name": "agent",
        "image": "${SANDBOX_IMAGE}",
        "imagePullPolicy": "Always",
        "ports": [{"containerPort": 8080}],
        "securityContext": {
          "allowPrivilegeEscalation": false,
          "runAsNonRoot": true,
          "capabilities": {"drop": ["ALL"]},
          "seccompProfile": {"type": "RuntimeDefault"}
        }
      }],
      "securityContext": {
        "runAsNonRoot": true,
        "seccompProfile": {"type": "RuntimeDefault"}
      }
    }
  otel-collector-endpoint: "${OTEL_ENDPOINT}"
  otel-ca-secret: "lightspeed-otel-ca"
  otel-admin-endpoint: "${OTEL_ADMIN}"
EOF

# Wait for rollout.
echo "Waiting for operator rollout..."
oc rollout status deployment/controller-manager -n "${OPERATOR_NAMESPACE}" --timeout=120s

echo "Operator pods:"
oc get pods -n "${OPERATOR_NAMESPACE}" -l control-plane=controller-manager

# Create ApprovalPolicy (analysis auto-approved for e2e).
echo "Creating ApprovalPolicy..."
oc apply -f - <<'EOF'
apiVersion: agentic.openshift.io/v1alpha1
kind: ApprovalPolicy
metadata:
  name: cluster
spec:
  maxConcurrentRuns: 5
  stages:
  - name: Analysis
    approval: Automatic
EOF

echo "=== Install complete ==="
oc get deployment -n "${OPERATOR_NAMESPACE}"
