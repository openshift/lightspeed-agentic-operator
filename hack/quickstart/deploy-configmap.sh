#!/usr/bin/env bash
#
# Create the lightspeed-agentic-configuration ConfigMap.
# Contains sandbox PodSpec and optionally OTEL collector settings.
#
# If the OTEL collector was deployed (deploy-otel.sh), the script
# auto-detects its Service and wires otel-collector-endpoint,
# otel-admin-endpoint, and otel-ca-secret into the ConfigMap.
#
# Usage:
#   bash hack/quickstart/deploy-configmap.sh
#   bash hack/quickstart/deploy-configmap.sh --sandbox-image=quay.io/my-org/my-sandbox:tag
#
# Prerequisites:
#   - oc CLI on PATH, logged into an OpenShift cluster
#   - Namespace openshift-lightspeed exists
#
# Flags:
#   --sandbox-image=IMAGE   Sandbox image (default: Konflux :main)

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
SANDBOX_IMAGE="quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-sandbox:main"

while [ $# -gt 0 ]; do
  case "$1" in
  --sandbox-image=*)
    SANDBOX_IMAGE="${1#*=}"
    shift
    ;;
  --sandbox-image)
    [ $# -lt 2 ] && {
      echo "Missing value for $1" >&2
      exit 1
    }
    SANDBOX_IMAGE="$2"
    shift 2
    ;;
  *)
    echo "Unknown flag: $1" >&2
    exit 1
    ;;
  esac
done

CONFIGMAP_NAME="lightspeed-agentic-configuration"
COLLECTOR_NAME="lightspeed-otel-collector"
COLLECTOR_CERT_SECRET="${COLLECTOR_NAME}-cert"

info() { echo "  ✓ $*"; }
step() { echo "[configmap] $*"; }
fail() {
  echo "  ✗ $*" >&2
  exit 1
}

step "Creating ${CONFIGMAP_NAME} in ${NAMESPACE}"
step "Sandbox image: ${SANDBOX_IMAGE}"

POD_SPEC="{\"containers\":[{\"name\":\"agent\",\"image\":\"${SANDBOX_IMAGE}\",\"resources\":{\"requests\":{\"cpu\":\"100m\",\"memory\":\"256Mi\"},\"limits\":{\"cpu\":\"1\",\"memory\":\"1Gi\"}},\"securityContext\":{\"allowPrivilegeEscalation\":false,\"runAsNonRoot\":true,\"capabilities\":{\"drop\":[\"ALL\"]},\"seccompProfile\":{\"type\":\"RuntimeDefault\"}}}],\"securityContext\":{\"runAsNonRoot\":true,\"seccompProfile\":{\"type\":\"RuntimeDefault\"}}}"

# Detect OTEL collector if deployed.
OTEL_DATA=""
OTEL_CA_SECRET_NAME="lightspeed-agentic-otel-ca"
if oc get service "${COLLECTOR_NAME}" -n "${NAMESPACE}" >/dev/null 2>&1; then
  OTEL_ENDPOINT="${COLLECTOR_NAME}.${NAMESPACE}.svc:4317"
  OTEL_ADMIN="https://${COLLECTOR_NAME}.${NAMESPACE}.svc:8080"
  step "OTEL collector detected: ${OTEL_ENDPOINT}"

  # Create the CA secret from the service-ca bundle.
  # The service-ca controller injects the CA into a well-known ConfigMap.
  step "Creating OTEL CA secret from service-ca bundle..."
  CA_BUNDLE="$(oc get configmap openshift-service-ca.crt -n "${NAMESPACE}" -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || true)"
  if [ -n "${CA_BUNDLE}" ]; then
    oc apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${OTEL_CA_SECRET_NAME}
  namespace: ${NAMESPACE}
type: Opaque
stringData:
  otel-ca.crt: |
$(echo "${CA_BUNDLE}" | sed 's/^/    /')
EOF
    info "OTEL CA secret created"
  else
    fail "openshift-service-ca.crt ConfigMap not found in ${NAMESPACE}.
  service-ca is required for OTEL TLS. Check that the service-ca operator is running."
  fi

  OTEL_DATA="  otel-collector-endpoint: \"${OTEL_ENDPOINT}\"
  otel-ca-secret: \"${OTEL_CA_SECRET_NAME}\""

  if oc get deployment lightspeed-postgres-server -n "${NAMESPACE}" >/dev/null 2>&1; then
    OTEL_DATA="${OTEL_DATA}
  otel-admin-endpoint: \"${OTEL_ADMIN}\""
    info "Postgres detected — admin endpoint advertised"
  fi
else
  step "No OTEL collector found — skipping OTEL config"
fi

oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CONFIGMAP_NAME}
  namespace: ${NAMESPACE}
data:
  sandbox-mode: "bare-pod"
  sandbox-pod-spec: '${POD_SPEC}'
  tls-profile: "IntermediateType"
  tls-min-version: "VersionTLS12"
  tls-cipher-suites: '["TLS_AES_128_GCM_SHA256","TLS_AES_256_GCM_SHA384","TLS_CHACHA20_POLY1305_SHA256","ECDHE-ECDSA-AES128-GCM-SHA256","ECDHE-RSA-AES128-GCM-SHA256","ECDHE-ECDSA-AES256-GCM-SHA384","ECDHE-RSA-AES256-GCM-SHA384","ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","DHE-RSA-AES128-GCM-SHA256","DHE-RSA-AES256-GCM-SHA384"]'
${OTEL_DATA}
EOF

info "ConfigMap ${CONFIGMAP_NAME} created"
