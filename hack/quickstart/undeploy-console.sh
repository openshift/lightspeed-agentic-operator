#!/usr/bin/env bash
#
# Remove the agentic console plugin standalone workload.
# Can be called from uninstall.sh or run independently.
#
# Usage:
#   bash hack/quickstart/undeploy-console.sh
#
# Environment variables:
#   NAMESPACE  (default: openshift-lightspeed)

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
PLUGIN_NAME="lightspeed-agentic-console-plugin"

info()  { echo "  ✓ $*"; }
step()  { echo "[console] $*"; }

# Deregister from OpenShift Console
step "Deregistering console plugin"
plugin_idx="$(
  oc get console.operator.openshift.io cluster -o json 2>/dev/null \
    | python3 -c "import sys,json; p=json.load(sys.stdin).get('spec',{}).get('plugins',[]); print(p.index('${PLUGIN_NAME}') if '${PLUGIN_NAME}' in p else '')" 2>/dev/null
)" || true
if [ -n "${plugin_idx}" ]; then
  oc patch console.operator.openshift.io cluster --type=json \
    -p "[{\"op\":\"remove\",\"path\":\"/spec/plugins/${plugin_idx}\"}]" >/dev/null 2>&1 || true
  info "Plugin deregistered from Console"
else
  info "Plugin not registered in Console — skipping"
fi
oc delete consoleplugin "${PLUGIN_NAME}" --ignore-not-found 2>/dev/null || true
info "ConsolePlugin CR deleted"

# Delete workload resources
step "Deleting console plugin workload"
oc delete deployment "${PLUGIN_NAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete service "${PLUGIN_NAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete configmap "${PLUGIN_NAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete sa "${PLUGIN_NAME}" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
oc delete secret "${PLUGIN_NAME}-cert" -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true
info "Console plugin workload resources deleted"
