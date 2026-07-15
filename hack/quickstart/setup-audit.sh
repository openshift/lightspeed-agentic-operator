#!/usr/bin/env bash
#
# Enable audit tracing for Agentic OLS.
# Run after install.sh — creates an AgenticOLSConfig CR that turns on
# audit tracing (stdout OTLP JSON) and optionally exports to a collector.
#
# Usage:
#   # Audit tracing only (stdout OTLP JSON, no remote collector):
#   bash hack/quickstart/setup-audit.sh
#
#   # Audit tracing + OTEL export to a Jaeger instance:
#   OTEL_ENDPOINT=jaeger-otlp-grpc.observability.svc:4317 bash hack/quickstart/setup-audit.sh
#
#   # Disable audit tracing:
#   AUDIT_LOGGING=Disabled bash hack/quickstart/setup-audit.sh
#
# Need a Jaeger instance? See hack/deploy-jaeger.sh

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
AUDIT_LOGGING="${AUDIT_LOGGING:-Enabled}"
OTEL_ENDPOINT="${OTEL_ENDPOINT:-}"
OTEL_TLS_MODE="${OTEL_TLS_MODE:-Insecure}"

echo "Configuring audit tracing..."

# Build the CR YAML — otel block is only included when OTEL_ENDPOINT is set.
if [[ -n "${OTEL_ENDPOINT}" ]]; then
cat <<EOF | oc apply -f -
apiVersion: agentic.openshift.io/v1alpha1
kind: AgenticOLSConfig
metadata:
  name: cluster
spec:
  suspended: false
  audit:
    logging: ${AUDIT_LOGGING}
    otel:
      endpoint: "${OTEL_ENDPOINT}"
      tlsMode: ${OTEL_TLS_MODE}
EOF
else
cat <<EOF | oc apply -f -
apiVersion: agentic.openshift.io/v1alpha1
kind: AgenticOLSConfig
metadata:
  name: cluster
spec:
  suspended: false
  audit:
    logging: ${AUDIT_LOGGING}
EOF
fi

echo "  ✓ AgenticOLSConfig applied (logging=${AUDIT_LOGGING})"

# Restart operator to pick up the config (read at startup).
echo "Restarting operator to apply audit config..."
oc rollout restart deployment/lightspeed-agentic-operator -n "${NAMESPACE}"
oc rollout status deployment/lightspeed-agentic-operator -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1
echo "  ✓ Operator restarted"

# Summary
echo ""
echo "  Audit logging : ${AUDIT_LOGGING}"
if [[ -n "${OTEL_ENDPOINT}" ]]; then
  echo "  OTEL endpoint : ${OTEL_ENDPOINT} (${OTEL_TLS_MODE})"
else
  echo "  OTEL tracing  : disabled (set OTEL_ENDPOINT to enable)"
fi
echo ""
