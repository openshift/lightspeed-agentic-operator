#!/usr/bin/env bash
#
# Uninstall Agentic OLS quickstart deployment.
#
# Usage:
#   bash <(curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/uninstall.sh)

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"

info()  { echo "  ✓ $*"; }
step()  { echo "[${1}] ${2}"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

if [ "${QUICKSTART_FORCE:-}" != "1" ]; then
  echo "This will delete ALL Agentic OLS resources in namespace ${NAMESPACE},"
  echo "remove the console plugin, operator CRDs cluster-wide, and the namespace itself."
  echo ""
  read -rp "Continue? [y/N] " confirm
  case "${confirm}" in
    [yY][eE][sS]|[yY]) ;;
    *) echo "Aborted."; exit 0 ;;
  esac
fi

# --- Step 1: Delete CRs ------------------------------------------------------

step "1/7" "Deleting custom resources..."

for kind in proposals proposalapprovals analysisresults executionresults verificationresults escalationresults; do
  oc delete "${kind}" --all -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "Proposal resources deleted"

oc delete agents --all --ignore-not-found 2>/dev/null || true
oc delete llmproviders --all --ignore-not-found 2>/dev/null || true
oc delete approvalpolicy cluster --ignore-not-found 2>/dev/null || true
oc delete agenticolsconfig cluster --ignore-not-found 2>/dev/null || true
info "Agents, LLMProviders, ApprovalPolicy, AgenticOLSConfig deleted"

# --- Step 2: Delete secrets ---------------------------------------------------

step "2/7" "Deleting credential secrets..."

for secret in llm-creds-vertex llm-creds-openai llm-creds-azure llm-creds-bedrock llm-creds-anthropic; do
  oc delete secret "${secret}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "Credential secrets deleted"

# --- Step 3: Remove console plugin --------------------------------------------

step "3/7" "Removing console plugin..."

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/undeploy-console.sh" ]; then
  NAMESPACE="${NAMESPACE}" bash "${SCRIPT_DIR}/undeploy-console.sh"
else
  info "undeploy-console.sh not found — skipping console cleanup"
fi

# --- Step 4: Delete webhook resources -----------------------------------------
# Delete the MutatingWebhookConfiguration BEFORE the operator so the fail-closed
# webhook doesn't block API calls while its backend is gone.

step "4/7" "Deleting webhook resources..."
oc delete mutatingwebhookconfiguration agentic-operator-mutating-webhook --ignore-not-found 2>/dev/null || true
oc delete service agentic-operator-webhook-service -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete secret agentic-operator-webhook-certs -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
info "Webhook resources deleted"

# --- Step 5: Delete operator --------------------------------------------------

step "5/7" "Deleting operator deployment..."

oc delete deployment lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete sa lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agentic-operator --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agent-cluster-reader --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agent-monitoring-view --ignore-not-found 2>/dev/null || true
info "Operator removed"

# --- Step 6: Delete CRDs -----------------------------------------------------

step "6/7" "Deleting Agentic Operator CRDs..."

for crd in \
  agenticolsconfigs.agentic.openshift.io \
  agents.agentic.openshift.io \
  analysisresults.agentic.openshift.io \
  approvalpolicies.agentic.openshift.io \
  escalationresults.agentic.openshift.io \
  executionresults.agentic.openshift.io \
  llmproviders.agentic.openshift.io \
  proposalapprovals.agentic.openshift.io \
  proposals.agentic.openshift.io \
  verificationresults.agentic.openshift.io; do
  oc delete crd "${crd}" --ignore-not-found --timeout=30s 2>/dev/null || true
done
info "CRDs deleted"

# --- Step 7: Delete namespace -------------------------------------------------

step "7/7" "Deleting namespace ${NAMESPACE}..."

oc delete namespace "${NAMESPACE}" --ignore-not-found --timeout=60s 2>/dev/null || true
info "Namespace deleted"

cat <<DONE

  Agentic OLS has been uninstalled.

DONE
