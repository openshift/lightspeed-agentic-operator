#!/usr/bin/env bash
# Source this file; do not execute directly.

set -euo pipefail

log_info()  { echo "[INFO]  $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" >&2; }
log_warn()  { echo "[WARN]  $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" >&2; }
log_error() { echo "[ERROR] $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" >&2; }

check_prerequisites() {
    local missing=()
    for cmd in oc make go jq; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing[*]}"
        exit 1
    fi

    if ! oc whoami &>/dev/null; then
        log_error "Not logged in to an OpenShift cluster (oc whoami failed)"
        exit 1
    fi

    if ! oc whoami --show-server &>/dev/null; then
        log_error "Cluster not reachable (oc whoami --show-server failed)"
        exit 1
    fi

    log_info "Prerequisites OK: oc=$(oc version --client -o json | jq -r '.clientVersion.gitVersion // "unknown"'), cluster=$(oc whoami --show-server)"
}

parse_snapshot() {
    if [[ -n "${SNAPSHOT:-}" ]]; then
        local component_name="${KONFLUX_COMPONENT_NAME:?KONFLUX_COMPONENT_NAME must be set when SNAPSHOT is provided}"
        IMG="$(jq -r --arg component_name "$component_name" \
            '.components[] | select(.name == $component_name) | .containerImage' \
            <<< "$SNAPSHOT")"
        if [[ -z "$IMG" || "$IMG" == "null" ]]; then
            log_error "Could not extract operator image from SNAPSHOT for component '$component_name'"
            exit 1
        fi
        export IMG
        log_info "Extracted IMG=$IMG from SNAPSHOT (component=$component_name)"
    elif [[ -z "${IMG:-}" ]]; then
        log_error "Either SNAPSHOT or IMG must be set"
        exit 1
    else
        log_info "Using IMG=$IMG (no SNAPSHOT)"
    fi
}

_OPERATOR_DEPLOYED_BY_SCRIPT=0

deploy_operator() {
    local namespace="${OPERATOR_NAMESPACE:-openshift-lightspeed}"

    if oc get deployment controller-manager -n "$namespace" &>/dev/null; then
        local available
        available="$(oc get deployment controller-manager -n "$namespace" \
            -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)"
        if [[ "$available" == "True" ]]; then
            log_info "Operator already deployed and available in $namespace — skipping install"
            return 0
        fi
        log_warn "Operator deployment exists but not Available — reinstalling"
    fi

    log_info "Deploying operator (IMG=$IMG, namespace=$namespace)..."

    local script_dir
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

    IMG="$IMG" \
    KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" \
    OPERATOR_NAMESPACE="$namespace" \
    SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent}" \
    bash "${script_dir}/.tekton/integration-tests/scripts/install-operator.sh"

    _OPERATOR_DEPLOYED_BY_SCRIPT=1
    wait_for_deployment "$namespace"
}

wait_for_deployment() {
    local namespace="$1"
    local timeout="${2:-120s}"
    log_info "Waiting for operator deployment (timeout=$timeout)..."
    if ! oc rollout status deployment/controller-manager -n "$namespace" --timeout="$timeout"; then
        log_error "Operator deployment did not become available within $timeout"
        oc get deployment controller-manager -n "$namespace" -o yaml >&2 || true
        exit 1
    fi
    log_info "Operator deployment is available"
}

cleanup_operator() {
    if [[ "$_OPERATOR_DEPLOYED_BY_SCRIPT" -eq 1 ]]; then
        log_info "Cleaning up operator (deployed by this script)..."
        make undeploy ignore-not-found=true 2>/dev/null || true
    else
        log_info "Skipping operator cleanup (was pre-existing)"
    fi
}
