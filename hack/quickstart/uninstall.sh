#!/usr/bin/env bash
#
# Uninstall Agentic OLS quickstart deployment.
# Removes all components in reverse order.
#
# Usage:
#   bash hack/quickstart/uninstall.sh
#
# Flags:
#   --force   Skip confirmation prompt

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --force) FORCE=1; shift ;;
    *) echo "Unknown flag: $1" >&2; exit 1 ;;
  esac
done

info()  { echo "  ✓ $*"; }
step()  { echo ""; echo "=== $* ==="; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for script in undeploy-console.sh undeploy-operator.sh undeploy-configmap.sh undeploy-alerts-adapter.sh undeploy-otel.sh; do
  [ -f "${SCRIPT_DIR}/${script}" ] || fail "Missing script: ${SCRIPT_DIR}/${script}. Run from a repo checkout."
done

if [ "${FORCE}" != "1" ]; then
  echo "This will delete all supporting deployments (OTEL collector, alerts adapter,"
  echo "console plugin, Postgres), the operator, CRDs, and the ${NAMESPACE} namespace."
  echo ""
  read -rp "Continue? [y/N] " confirm
  case "${confirm}" in
    [yY][eE][sS]|[yY]) ;;
    *) echo "Aborted."; exit 0 ;;
  esac
fi

# --- Delete Agentic CRs ------------------------------------------------------

step "1/8 Deleting Agentic custom resources"

for kind in agenticruns agenticrunapprovals analysisresults executionresults verificationresults escalationresults; do
  oc delete "${kind}" --all -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
done
info "AgenticRun resources deleted"

oc delete agents --all --ignore-not-found >/dev/null 2>&1 || true
oc delete llmproviders --all --ignore-not-found >/dev/null 2>&1 || true
info "Agents and LLMProviders deleted"

# --- Delete secrets -----------------------------------------------------------

step "2/8 Deleting credential secrets"

for secret in llm-creds-vertex llm-creds-openai llm-creds-azure llm-creds-bedrock llm-creds-anthropic; do
  oc delete secret "${secret}" -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
done
info "Credential secrets deleted"

# --- Console plugin -----------------------------------------------------------

step "3/8 Removing console plugin"
bash "${SCRIPT_DIR}/undeploy-console.sh"

# --- Operator -----------------------------------------------------------------

step "4/8 Removing agentic operator"
bash "${SCRIPT_DIR}/undeploy-operator.sh"

# --- Configuration ConfigMap --------------------------------------------------

step "5/8 Removing configuration"
bash "${SCRIPT_DIR}/undeploy-configmap.sh"

# --- Alerts adapter -----------------------------------------------------------

step "6/8 Removing alerts adapter"
bash "${SCRIPT_DIR}/undeploy-alerts-adapter.sh"

# --- OTEL collector -----------------------------------------------------------

step "7/8 Removing OTEL collector"
bash "${SCRIPT_DIR}/undeploy-otel.sh"

# --- Namespace ----------------------------------------------------------------

step "8/8 Namespace ${NAMESPACE}"
oc delete namespace "${NAMESPACE}" --ignore-not-found --timeout=60s || true
if oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  echo "  ! Namespace ${NAMESPACE} is still terminating — check for stuck finalizers" >&2
  echo "  ! A reinstall may fail until the namespace is fully gone" >&2
else
  info "Namespace deleted"
fi

cat <<DONE

  Agentic OLS has been uninstalled.
DONE
