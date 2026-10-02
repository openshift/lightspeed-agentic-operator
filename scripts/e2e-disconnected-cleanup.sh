#!/usr/bin/env bash
# Independent cleanup for hard test timeouts. Delete only this invocation's resources.
set -uo pipefail
set +x
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${E2E_DISCONNECTED_ID:?Missing invocation ID}"
selector="agentic.openshift.io/disconnected-e2e=$E2E_DISCONNECTED_ID"
resources=pods,secrets,networkpolicies,serviceaccounts,rolebindings,llmproviders,agents,approvalpolicies

if [[ -n "${ARTIFACT_DIR:-}" ]]; then
    dir="$ARTIFACT_DIR/disconnected/fallback"
    mkdir -p "$dir"
    # Never collect Secret data or registry credential contents.
    oc get pods,networkpolicies,serviceaccounts,rolebindings -A -l "$selector" -o yaml --request-timeout=30s \
        2>&1 | python3 "$SCRIPT_DIR/e2e-redact.py" > "$dir/owned-resources.yaml" || true
    oc logs deployment/controller-manager -n "${OPERATOR_NAMESPACE:-openshift-lightspeed}" --tail=500 --request-timeout=30s \
        2>&1 | python3 "$SCRIPT_DIR/e2e-redact.py" > "$dir/operator.log" || true
    if [[ -n "${RHOAI_VLLM_NAMESPACE:-}" ]]; then
        oc get pods,events,inferenceservices.serving.kserve.io,servingruntimes.serving.kserve.io \
            -n "$RHOAI_VLLM_NAMESPACE" -o yaml --request-timeout=30s \
            2>&1 | python3 "$SCRIPT_DIR/e2e-redact.py" > "$dir/inference.yaml" || true
        # Readiness may fail before the service Pod-info helper collects logs.
        oc logs -n "$RHOAI_VLLM_NAMESPACE" -l serving.kserve.io/inferenceservice=vllm-model \
            --all-containers --prefix --tail=500 --request-timeout=30s \
            2>&1 | python3 "$SCRIPT_DIR/e2e-redact.py" > "$dir/inference.log" || true
    fi
fi

# Stop invocation-owned runs and their sandbox Pods before restoring egress.
run_uids="$(oc get agenticruns -A -l "$selector" -o json --request-timeout=30s | jq -r '.items[]?.metadata.uid')" || exit 1
for uid in $run_uids; do
    oc delete pods -n "${OPERATOR_NAMESPACE:-openshift-lightspeed}" -l "agentic.openshift.io/run=$uid" \
        --ignore-not-found --wait=true --timeout=60s --request-timeout=30s || exit 1
done
oc delete agenticruns,agenticrunapprovals -A -l "$selector" --ignore-not-found --wait=true --timeout=60s --request-timeout=30s || exit 1
oc delete "$resources" -A -l "$selector" --ignore-not-found --wait=true --timeout=60s --request-timeout=30s || true
# Report residual resources without obscuring the caller's original failure.
remaining="$(oc get "$resources",agenticruns,agenticrunapprovals -A -l "$selector" -o name --request-timeout=30s)" || exit 1
if [[ -n "$remaining" ]]; then
    echo 'Disconnected cleanup left invocation-owned resources' >&2
    exit 1
fi
