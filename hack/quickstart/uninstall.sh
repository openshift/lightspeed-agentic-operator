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

step "1/6" "Deleting custom resources..."

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

step "2/6" "Deleting credential secrets..."

for secret in llm-creds-vertex llm-creds-openai llm-creds-azure llm-creds-bedrock llm-creds-anthropic; do
  oc delete secret "${secret}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "Credential secrets deleted"

# --- Step 3: Remove console plugin --------------------------------------------

step "3/6" "Removing console plugin..."

PLUGIN_NAME="lightspeed-agentic-console-plugin"

if oc get consoleplugin "${PLUGIN_NAME}" >/dev/null 2>&1; then
  plugin_idx="$(
    oc get console.operator.openshift.io cluster -o json 2>/dev/null \
      | python3 -c "import sys,json; p=json.load(sys.stdin).get('spec',{}).get('plugins',[]); print(p.index('${PLUGIN_NAME}') if '${PLUGIN_NAME}' in p else '')" 2>/dev/null
  )"

  if [ -n "${plugin_idx}" ] && oc patch console.operator.openshift.io cluster --type=json \
    -p "[{\"op\":\"remove\",\"path\":\"/spec/plugins/${plugin_idx}\"}]" >/dev/null 2>&1; then
    info "Plugin deregistered from Console"
  else
    info "Plugin not registered in Console (or patch failed) — skipping deregistration"
  fi

  oc delete consoleplugin "${PLUGIN_NAME}" --ignore-not-found 2>/dev/null || true
  info "ConsolePlugin CR deleted"
else
  info "Console plugin not found — skipping"
fi

# --- Step 4: Delete operator --------------------------------------------------

step "4/6" "Deleting operator deployment..."

oc delete deployment lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete sa lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agentic-operator --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agent-cluster-reader --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agent-monitoring-view --ignore-not-found 2>/dev/null || true
info "Operator removed"

# --- Step 5: Delete CRDs -----------------------------------------------------

step "5/6" "Deleting Agentic Operator CRDs..."

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

# --- Step 6: Delete namespace -------------------------------------------------

step "6/6" "Deleting namespace ${NAMESPACE}..."

oc delete namespace "${NAMESPACE}" --ignore-not-found --timeout=60s 2>/dev/null || true
info "Namespace deleted"

cat <<DONE

  Agentic OLS has been uninstalled.

DONE
