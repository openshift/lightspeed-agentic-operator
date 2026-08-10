#!/usr/bin/env bash
#
# Remove the agentic operator quickstart deployment.
# Can be called from uninstall.sh or run independently.
#
# Usage:
#   bash hack/quickstart/undeploy-operator.sh

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"

info()  { echo "  ✓ $*"; }
step()  { echo "[operator] $*"; }

# Delete admission policy first so fail-closed VAP/webhook don't block cleanup.
step "Deleting suspension ValidatingAdmissionPolicy"
oc delete validatingadmissionpolicybinding agentic.openshift.io-agenticrun-suspension --ignore-not-found ||
  step "Warning: could not delete ValidatingAdmissionPolicyBinding (managed cluster?)"
oc delete validatingadmissionpolicy agentic.openshift.io-agenticrun-suspension --ignore-not-found ||
  step "Warning: could not delete ValidatingAdmissionPolicy (managed cluster?)"
info "Suspension ValidatingAdmissionPolicy deleted"

step "Deleting webhook resources"
oc delete mutatingwebhookconfiguration agentic-operator-mutating-webhook --ignore-not-found ||
  step "Warning: could not delete MutatingWebhookConfiguration (managed cluster?)"
oc delete service agentic-operator-webhook-service -n "${NAMESPACE}" --ignore-not-found
oc delete secret agentic-operator-webhook-certs -n "${NAMESPACE}" --ignore-not-found
info "Webhook resources deleted"

step "Deleting ApprovalPolicy"
oc delete approvalpolicy cluster --ignore-not-found
info "ApprovalPolicy deleted"

step "Deleting custom resources (operator still running to process finalizers)"
CR_TYPES=(
  agenticruns.agentic.openshift.io
  agenticrunapprovals.agentic.openshift.io
  analysisresults.agentic.openshift.io
  executionresults.agentic.openshift.io
  escalationresults.agentic.openshift.io
  verificationresults.agentic.openshift.io
)
for cr in "${CR_TYPES[@]}"; do
  oc delete "${cr}" --all -n "${NAMESPACE}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
done

step "Waiting for custom resources to be fully removed..."
STUCK=""
for cr in "${CR_TYPES[@]}"; do
  remaining="$(oc get "${cr}" -n "${NAMESPACE}" --no-headers 2>/dev/null | wc -l || echo 0)"
  if [ "${remaining}" -gt 0 ]; then
    STUCK="${STUCK} ${cr}"
  fi
done
if [ -n "${STUCK}" ]; then
  step "Warning: resources still exist (stuck finalizers?):${STUCK}"
  step "Stripping finalizers to unblock deletion..."
  for cr in ${STUCK}; do
    for name in $(oc get "${cr}" -n "${NAMESPACE}" --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null); do
      oc patch "${cr}" "${name}" -n "${NAMESPACE}" --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    done
  done
  sleep 2
fi
info "Custom resources removed"

step "Deleting operator deployment"
oc delete deployment lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found
oc delete sa lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found
oc delete clusterrolebinding lightspeed-agentic-operator --ignore-not-found ||
  step "Warning: could not delete ClusterRoleBinding lightspeed-agentic-operator (managed cluster?)"
oc delete clusterrolebinding lightspeed-agent-cluster-reader --ignore-not-found ||
  step "Warning: could not delete ClusterRoleBinding lightspeed-agent-cluster-reader (managed cluster?)"
oc delete clusterrolebinding lightspeed-agent-monitoring-view --ignore-not-found ||
  step "Warning: could not delete ClusterRoleBinding lightspeed-agent-monitoring-view (managed cluster?)"
info "Operator resources deleted"

step "Deleting CRDs"
for crd in \
  agenticolsconfigs.agentic.openshift.io \
  agents.agentic.openshift.io \
  analysisresults.agentic.openshift.io \
  approvalpolicies.agentic.openshift.io \
  escalationresults.agentic.openshift.io \
  executionresults.agentic.openshift.io \
  llmproviders.agentic.openshift.io \
  agenticrunapprovals.agentic.openshift.io \
  agenticruns.agentic.openshift.io \
  verificationresults.agentic.openshift.io; do
  oc delete crd "${crd}" --ignore-not-found --timeout=30s
done
info "CRDs deleted"

info "Agentic operator removed"
