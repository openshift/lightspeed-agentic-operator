#!/usr/bin/env bash
#
# Uninstall Agentic OLS quickstart deployment.
#
# Usage:
#   curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/uninstall.sh | bash

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"

info()  { echo "  ✓ $*"; }
step()  { echo "[${1}] ${2}"; }

# --- Step 1: Delete CRs ------------------------------------------------------

step "1/5" "Deleting custom resources..."

for kind in proposals proposalapprovals analysisresults executionresults verificationresults escalationresults; do
  oc delete "${kind}" --all -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "Proposal resources deleted"

oc delete agents --all --ignore-not-found 2>/dev/null || true
oc delete llmproviders --all --ignore-not-found 2>/dev/null || true
oc delete approvalpolicy cluster --ignore-not-found 2>/dev/null || true
info "Agents, LLMProviders, ApprovalPolicy deleted"

# --- Step 2: Delete secrets ---------------------------------------------------

step "2/5" "Deleting credential secrets..."

for secret in llm-creds-vertex llm-creds-openai llm-creds-azure llm-creds-bedrock llm-creds-anthropic; do
  oc delete secret "${secret}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "Credential secrets deleted"

# --- Step 3: Delete operator --------------------------------------------------

step "3/5" "Deleting operator deployment..."

oc delete deployment lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete sa lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agentic-operator --ignore-not-found 2>/dev/null || true
info "Operator removed"

# --- Step 4: Delete namespace -------------------------------------------------

step "4/5" "Deleting namespace ${NAMESPACE}..."

oc delete namespace "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
info "Namespace deleted"

# --- Step 5: Delete CRDs -----------------------------------------------------

step "5/5" "Deleting Agentic Operator CRDs..."

for crd in \
  agents.agentic.openshift.io \
  analysisresults.agentic.openshift.io \
  approvalpolicies.agentic.openshift.io \
  escalationresults.agentic.openshift.io \
  executionresults.agentic.openshift.io \
  llmproviders.agentic.openshift.io \
  proposalapprovals.agentic.openshift.io \
  proposals.agentic.openshift.io \
  verificationresults.agentic.openshift.io; do
  oc delete crd "${crd}" --ignore-not-found 2>/dev/null || true
done
info "CRDs deleted"

cat <<DONE

  Agentic OLS has been uninstalled.

  Note: The Agent Sandbox controller was NOT removed (other workloads
  may depend on it). To remove it manually:

    oc delete -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.4.5/extensions.yaml
    oc delete -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.4.5/manifest.yaml

DONE
