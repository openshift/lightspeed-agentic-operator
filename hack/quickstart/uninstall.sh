#!/usr/bin/env bash
#
# Uninstall Agentic OLS quickstart deployment.
# Optionally uninstalls Lightspeed Operator if it was installed via OLM
# (operator-sdk run bundle), matching hack/quickstart/install.sh.
#
# Usage (download then run — required for interactive OLS prompts):
#   curl -fsSL -o uninstall-agentic.sh \
#     https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/uninstall.sh
#   bash uninstall-agentic.sh
#
# Do not use: curl … | bash  (stdin is the script; prompts cannot read y/n)
#
# Env:
#   NAMESPACE       (default: openshift-lightspeed)
#   QUICKSTART_FORCE=1  skip the top-level confirmation
#   REMOVE_OLS=1|0      non-interactive OLS uninstall decision (skips the y/n prompt)
#   OPERATOR_SDK_VERSION, CLEANUP_TIMEOUT

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OPERATOR_SDK_VERSION="${OPERATOR_SDK_VERSION:-1.36.1}"
CLEANUP_TIMEOUT="${CLEANUP_TIMEOUT:-5m}"
# OLM package name used by operator-sdk run bundle for Lightspeed Operator.
OLS_PACKAGE_NAME="${OLS_PACKAGE_NAME:-lightspeed-operator}"

info()  { echo "  ✓ $*"; }
warn()  { echo "  ⚠ $*" >&2; }
step()  { echo "[${1}] ${2}"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

require_tty_for_prompts() {
  if [[ ! -t 0 ]]; then
    fail "Interactive prompts require a terminal.

  Do not pipe the script into bash (curl … | bash).
  Download it, then run it:

    curl -fsSL -o uninstall-agentic.sh \\
      https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/uninstall.sh
    bash uninstall-agentic.sh

  Or set REMOVE_OLS=0|1 for a non-interactive OLS decision."
  fi
}

prompt_yes_no() {
  # $1 = prompt text. Returns 0 for yes, 1 for no.
  require_tty_for_prompts
  local reply
  while true; do
    printf '%s [y/n]: ' "$1"
    read -r reply
    case "${reply}" in
      [yY]|[yY][eE][sS]) return 0 ;;
      [nN]|[nN][oO]) return 1 ;;
      *) echo "  Please answer y or n." ;;
    esac
  done
}

ensure_operator_sdk() {
  if command -v operator-sdk >/dev/null 2>&1; then
    OPERATOR_SDK_BIN="$(command -v operator-sdk)"
    return 0
  fi
  command -v curl >/dev/null 2>&1 || fail "curl is required to download operator-sdk"
  local os arch dest url
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail "unsupported OS for operator-sdk download: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail "unsupported arch for operator-sdk download: $(uname -m)" ;;
  esac
  dest="${TMPDIR:-/tmp}/operator-sdk-${OPERATOR_SDK_VERSION}"
  url="https://github.com/operator-framework/operator-sdk/releases/download/v${OPERATOR_SDK_VERSION}/operator-sdk_${os}_${arch}"
  echo "  Downloading operator-sdk v${OPERATOR_SDK_VERSION}..."
  curl -fsSL -o "${dest}" "${url}"
  chmod +x "${dest}"
  OPERATOR_SDK_BIN="${dest}"
  info "operator-sdk ready (${OPERATOR_SDK_BIN})"
}

uninstall_ols() {
  ensure_operator_sdk
  echo "  Deleting OLSConfig (if present)..."
  oc delete olsconfig cluster --ignore-not-found 2>/dev/null || true
  echo "  Running: operator-sdk cleanup ${OLS_PACKAGE_NAME} -n ${NAMESPACE}..."
  "${OPERATOR_SDK_BIN}" cleanup "${OLS_PACKAGE_NAME}" \
    --namespace "${NAMESPACE}" \
    --timeout="${CLEANUP_TIMEOUT}" \
    || fail "operator-sdk cleanup failed for ${OLS_PACKAGE_NAME}.
  Fix OLM leftovers manually, then re-run this script (or set REMOVE_OLS=0 to skip OLS)."
  info "Lightspeed Operator uninstalled"
}

if [ "${QUICKSTART_FORCE:-}" != "1" ]; then
  require_tty_for_prompts
  echo "This will delete ALL Agentic OLS resources in namespace ${NAMESPACE},"
  echo "remove the console plugin, and operator CRDs cluster-wide."
  echo "You will be asked separately whether to also uninstall Lightspeed Operator."
  echo ""
  read -rp "Continue? [y/N] " confirm
  case "${confirm}" in
    [yY][eE][sS]|[yY]) ;;
    *) echo "Aborted."; exit 0 ;;
  esac
fi

REMOVE_OLS_OPERATOR=0

# Ask the user (unless REMOVE_OLS is set for non-interactive runs).
# No cluster probes — the answer alone decides whether OLS is cleaned up.
case "${REMOVE_OLS:-}" in
  1|true|yes|YES)
    REMOVE_OLS_OPERATOR=1
    ;;
  0|false|no|NO)
    REMOVE_OLS_OPERATOR=0
    ;;
  "")
    cat <<EOF

  Quickstart may also have installed Lightspeed Operator (OLS) via OLM.
  Answer n to leave OLS installed (namespace ${NAMESPACE} will be kept).

EOF
    if prompt_yes_no "Uninstall Lightspeed Operator as well?"; then
      REMOVE_OLS_OPERATOR=1
    else
      REMOVE_OLS_OPERATOR=0
    fi
    ;;
  *)
    fail "REMOVE_OLS must be 0 or 1 (got: ${REMOVE_OLS})"
    ;;
esac

if [ "${REMOVE_OLS_OPERATOR}" = "0" ]; then
  warn "Lightspeed Operator will be left installed; namespace ${NAMESPACE} will be kept"
fi

# --- Step 1: Delete Agentic CRs ----------------------------------------------

step "1/8" "Deleting Agentic custom resources..."

for kind in agenticruns agenticrunapprovals analysisresults executionresults verificationresults escalationresults; do
  oc delete "${kind}" --all -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "AgenticRun resources deleted"

oc delete agents --all --ignore-not-found 2>/dev/null || true
oc delete llmproviders --all --ignore-not-found 2>/dev/null || true
oc delete approvalpolicy cluster --ignore-not-found 2>/dev/null || true
oc delete agenticolsconfig cluster --ignore-not-found 2>/dev/null || true
info "Agents, LLMProviders, ApprovalPolicy, AgenticOLSConfig deleted"

# --- Step 2: Optional Lightspeed Operator ------------------------------------

step "2/8" "Optional Lightspeed Operator uninstall..."

if [ "${REMOVE_OLS_OPERATOR}" = "1" ]; then
  uninstall_ols
else
  info "Skipped Lightspeed Operator uninstall"
fi

# --- Step 3: Delete secrets ---------------------------------------------------

step "3/8" "Deleting credential secrets..."

for secret in llm-creds-vertex llm-creds-openai llm-creds-azure llm-creds-bedrock llm-creds-anthropic; do
  oc delete secret "${secret}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
done
info "Credential secrets deleted"

# --- Step 4: Remove console plugin --------------------------------------------

step "4/8" "Removing console plugin..."

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/undeploy-console.sh" ]; then
  NAMESPACE="${NAMESPACE}" bash "${SCRIPT_DIR}/undeploy-console.sh"
else
  info "undeploy-console.sh not found — skipping console cleanup"
fi

# --- Step 5: Delete webhook resources -----------------------------------------
# Delete the MutatingWebhookConfiguration BEFORE the operator so the fail-closed
# webhook doesn't block API calls while its backend is gone.

step "5/8" "Deleting webhook resources..."
oc delete mutatingwebhookconfiguration agentic-operator-mutating-webhook --ignore-not-found 2>/dev/null || true
oc delete service agentic-operator-webhook-service -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete secret agentic-operator-webhook-certs -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
info "Webhook resources deleted"

# --- Step 6: Delete operator --------------------------------------------------

step "6/8" "Deleting agentic operator deployment..."

oc delete deployment lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete sa lightspeed-agentic-operator -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agentic-operator --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agent-cluster-reader --ignore-not-found 2>/dev/null || true
oc delete clusterrolebinding lightspeed-agent-monitoring-view --ignore-not-found 2>/dev/null || true
info "Agentic operator removed"

# --- Step 7: Delete CRDs -----------------------------------------------------

step "7/8" "Deleting Agentic Operator CRDs..."

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
  oc delete crd "${crd}" --ignore-not-found --timeout=30s 2>/dev/null || true
done
info "Agentic CRDs deleted"

# --- Step 8: Delete namespace -------------------------------------------------

step "8/8" "Namespace ${NAMESPACE}..."

if [ "${REMOVE_OLS_OPERATOR}" = "0" ]; then
  warn "Keeping namespace ${NAMESPACE} (Lightspeed Operator left installed)"
else
  oc delete namespace "${NAMESPACE}" --ignore-not-found --timeout=60s 2>/dev/null || true
  info "Namespace deleted"
fi

cat <<DONE

  Agentic OLS has been uninstalled.
DONE

if [ "${REMOVE_OLS_OPERATOR}" = "1" ]; then
  echo "  Lightspeed Operator was also uninstalled."
else
  echo "  Lightspeed Operator was left installed in ${NAMESPACE}."
fi
echo ""
