#!/usr/bin/env bash
# Konflux: install the agentic operator onto the ephemeral cluster using make targets.
# Run from the repo root after checkout.
#
# Required env:
#   IMG              — operator image (from SNAPSHOT)
#   KUBECONFIG       — path to kubeconfig
#
# Optional env:
#   OPERATOR_NAMESPACE  (default: openshift-lightspeed)
#   SANDBOX_MODE        (default: bare-pod)
#   SANDBOX_IMAGE       (default: quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent)

set -euo pipefail

: "${IMG:?IMG must be set to the operator image}"
: "${KUBECONFIG:?KUBECONFIG must be set}"

OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-openshift-lightspeed}"
SANDBOX_MODE="${SANDBOX_MODE:-bare-pod}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent}"

echo "=== Agentic operator install ==="
echo "  IMG:                ${IMG}"
echo "  OPERATOR_NAMESPACE: ${OPERATOR_NAMESPACE}"
echo "  SANDBOX_MODE:       ${SANDBOX_MODE}"
echo "  SANDBOX_IMAGE:      ${SANDBOX_IMAGE}"
echo "================================="

# Ensure namespace exists.
oc create namespace "${OPERATOR_NAMESPACE}" --dry-run=client -o yaml | oc apply -f -

# Install CRDs.
echo "Installing CRDs..."
make install

# Deploy operator (kustomize-based).
echo "Deploying operator..."
make deploy IMG="${IMG}" OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE}" SANDBOX_MODE="${SANDBOX_MODE}" SANDBOX_IMAGE="${SANDBOX_IMAGE}"

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
  maxAttempts: 3
  maxConcurrentProposals: 5
  stages:
  - name: Analysis
    approval: Automatic
EOF

echo "=== Install complete ==="
oc get deployment -n "${OPERATOR_NAMESPACE}"
