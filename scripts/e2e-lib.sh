#!/usr/bin/env bash
# Source this file; do not execute directly.

set -euo pipefail

log_info()  { echo "[INFO]  $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" >&2; }
log_warn()  { echo "[WARN]  $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" >&2; }
log_error() { echo "[ERROR] $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" >&2; }

check_prerequisites() {
    local missing=()
    for cmd in oc make go jq git; do
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
_E2E_OTEL_DEPLOYED_BY_SCRIPT=0
_E2E_MANAGER_ROLE_CREATED_BY_SCRIPT=0
_E2E_MANAGER_RB_CREATED_BY_SCRIPT=0
_E2E_READER_RBAC_CREATED_BY_SCRIPT=0

ensure_operator_rbac() {
    local namespace="$1"
    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

    log_info "Ensuring operator manager RBAC in $namespace"
    if ! oc get clusterrole agentic-operator-manager-role &>/dev/null; then
        _E2E_MANAGER_ROLE_CREATED_BY_SCRIPT=1
    fi
    if ! oc get clusterrolebinding agentic-operator-manager-rolebinding &>/dev/null; then
        _E2E_MANAGER_RB_CREATED_BY_SCRIPT=1
    fi
    oc apply -f "$repo_root/config/rbac/role.yaml"
    sed "s|__OPERATOR_NAMESPACE__|${namespace}|g" "$repo_root/config/rbac/role_binding.yaml" | oc apply -f -

    log_info "Ensuring lightspeed-agent-cluster-reader ClusterRoleBinding in $namespace"
    if ! oc get clusterrolebinding lightspeed-agent-cluster-reader &>/dev/null; then
        _E2E_READER_RBAC_CREATED_BY_SCRIPT=1
    fi
    oc apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agent-cluster-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-reader
subjects:
- kind: ServiceAccount
  name: lightspeed-agent
  namespace: ${namespace}
EOF
}

ensure_config_configmap() {
    local namespace="$1"
    local sandbox_mode="${SANDBOX_MODE:-bare-pod}"
    local sandbox_image="${SANDBOX_IMAGE:-quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent}"

    if oc get configmap lightspeed-agentic-configuration -n "$namespace" &>/dev/null; then
        return 0
    fi

    log_info "Creating lightspeed-agentic-configuration ConfigMap in $namespace"
    oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: lightspeed-agentic-configuration
  namespace: ${namespace}
data:
  sandbox-mode: "${sandbox_mode}"
  sandbox-pod-spec: |
    {
      "containers": [{
        "name": "agent",
        "image": "${sandbox_image}",
        "ports": [{"containerPort": 8080}],
        "securityContext": {
          "allowPrivilegeEscalation": false,
          "runAsNonRoot": true,
          "capabilities": {"drop": ["ALL"]},
          "seccompProfile": {"type": "RuntimeDefault"}
        }
      }],
      "securityContext": {
        "runAsNonRoot": true,
        "seccompProfile": {"type": "RuntimeDefault"}
      }
    }
EOF
}

# ensure_e2e_otel deploys the persistent OTEL collector used to retain sandbox
# logs, then adds its connection details to the existing handoff ConfigMap.  It
# deliberately patches only OTEL keys: the classic operator (or
# ensure_config_configmap) remains the owner of the sandbox configuration.
# Set E2E_OTEL_ENABLED=false to opt out, or E2E_OTEL_IMAGE to override the
# collector image used by hack/quickstart/deploy-otel.sh.
ensure_e2e_otel() {
    local namespace="$1"
    if [[ "${E2E_OTEL_ENABLED:-true}" == "false" ]]; then
        log_info "E2E OTEL collection disabled (E2E_OTEL_ENABLED=false)"
        return 0
    fi

    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    local deploy_args=(--postgres)
    if [[ -n "${E2E_OTEL_IMAGE:-}" ]]; then
        deploy_args+=("--image=${E2E_OTEL_IMAGE}")
    fi

    log_info "Deploying persistent OTEL collector for E2E artifact collection"
    _E2E_OTEL_DEPLOYED_BY_SCRIPT=1
    NAMESPACE="$namespace" bash "$repo_root/hack/quickstart/deploy-otel.sh" "${deploy_args[@]}"

    # The serving CA is asynchronously injected by OpenShift.  Do not write a
    # partial OTEL configuration: sandbox pods would then fail TLS validation.
    local ca_bundle=""
    for _ in $(seq 1 60); do
        ca_bundle="$(oc get configmap openshift-service-ca.crt -n "$namespace" \
            -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || true)"
        [[ -n "$ca_bundle" ]] && break
        sleep 1
    done
    if [[ -z "$ca_bundle" ]]; then
        log_error "OTEL collector is deployed but openshift-service-ca.crt is unavailable in $namespace"
        return 1
    fi

    printf '%s' "$ca_bundle" | oc create secret generic lightspeed-agentic-otel-ca \
        -n "$namespace" --from-file=otel-ca.crt=/dev/stdin \
        --dry-run=client -o yaml | oc apply -f -

    local otel_patch
    otel_patch="$(jq -cn --arg endpoint "lightspeed-otel-collector.${namespace}.svc:4317" \
        --arg admin "https://lightspeed-otel-collector.${namespace}.svc:8080" \
        --arg ca "lightspeed-agentic-otel-ca" \
        '{data:{"otel-collector-endpoint":$endpoint,"otel-admin-endpoint":$admin,"otel-ca-secret":$ca}}')"
    oc patch configmap lightspeed-agentic-configuration -n "$namespace" \
        --type=merge -p "$otel_patch"
    log_info "OTEL artifact collection configured (collector + admin API)"
}

deploy_operator() {
    local namespace="${OPERATOR_NAMESPACE:-openshift-lightspeed}"

    if oc get deployment controller-manager -n "$namespace" &>/dev/null; then
        local available
        available="$(oc get deployment controller-manager -n "$namespace" \
            -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)"
        if [[ "$available" == "True" ]]; then
            log_info "Operator already deployed and available in $namespace — skipping install"
            ensure_operator_rbac "$namespace"
            ensure_config_configmap "$namespace"
            return 0
        fi
        log_warn "Operator deployment exists but not Available — reinstalling"
    fi

    log_info "Deploying operator (IMG=$IMG, namespace=$namespace)..."

    local script_dir
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

    _OPERATOR_DEPLOYED_BY_SCRIPT=1
    _E2E_READER_RBAC_CREATED_BY_SCRIPT=1
    IMG="$IMG" \
    KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" \
    OPERATOR_NAMESPACE="$namespace" \
    SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent}" \
    bash "${script_dir}/.tekton/integration-tests/scripts/install-operator.sh"

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
    oc delete clusterrolebinding lightspeed-agentic-operator-admin --ignore-not-found 2>/dev/null || true

    if [[ "$_E2E_READER_RBAC_CREATED_BY_SCRIPT" -eq 1 ]]; then
        oc delete clusterrolebinding lightspeed-agent-cluster-reader --ignore-not-found 2>/dev/null || true
    fi
    if [[ "$_E2E_MANAGER_RB_CREATED_BY_SCRIPT" -eq 1 ]]; then
        oc delete clusterrolebinding agentic-operator-manager-rolebinding --ignore-not-found 2>/dev/null || true
    fi
    if [[ "$_E2E_MANAGER_ROLE_CREATED_BY_SCRIPT" -eq 1 ]]; then
        oc delete clusterrole agentic-operator-manager-role --ignore-not-found 2>/dev/null || true
    fi

    if [[ "$_OPERATOR_DEPLOYED_BY_SCRIPT" -eq 1 ]]; then
        log_info "Cleaning up operator (deployed by this script)..."
        make undeploy ignore-not-found=true 2>/dev/null || true
    else
        log_info "Skipping operator cleanup (was pre-existing)"
    fi
}

cleanup_e2e_otel() {
    local namespace="$1"
    if [[ "$_E2E_OTEL_DEPLOYED_BY_SCRIPT" -eq 1 ]]; then
        local repo_root
        repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
        log_info "Cleaning up E2E OTEL/Postgres resources..."
        NAMESPACE="$namespace" bash "$repo_root/hack/quickstart/undeploy-otel.sh" 2>/dev/null || true
    else
        log_info "Skipping E2E OTEL cleanup (not deployed by this script)"
    fi
}

resolve_model() {
    local provider="$1"
    if [[ -n "${E2E_MODEL:-}" ]]; then
        echo "$E2E_MODEL"
        return
    fi
    case "$provider" in
        claude) echo "claude-sonnet-4-6" ;;
        gemini) echo "gemini-3.1-flash-lite" ;;
        openai) echo "gpt-5.4-mini" ;;
        *) log_error "Unknown provider: $provider"; return 1 ;;
    esac
}

collect_artifacts() {
    local provider="$1"
    local namespace="${OPERATOR_NAMESPACE:-openshift-lightspeed}"

    if [[ -z "${ARTIFACT_DIR:-}" ]]; then
        log_info "ARTIFACT_DIR not set — skipping artifact collection for $provider"
        return 0
    fi

    local artifact_dir="$ARTIFACT_DIR/$provider"
    mkdir -p "$artifact_dir"

    log_info "Collecting artifacts for provider=$provider → $artifact_dir"

    oc logs deployment/controller-manager -n "$namespace" --tail=500 \
        > "$artifact_dir/operator-logs.txt" 2>/dev/null || true
    oc logs deployment/controller-manager -n "$namespace" --tail=500 --previous \
        > "$artifact_dir/operator-logs-previous.txt" 2>/dev/null || true
    oc get agenticruns -A -o yaml \
        > "$artifact_dir/agenticruns.yaml" 2>/dev/null || true
    oc get agenticrunapprovals -A -o yaml \
        > "$artifact_dir/agenticrunapprovals.yaml" 2>/dev/null || true
    oc get analysisresults -A -o yaml \
        > "$artifact_dir/analysisresults.yaml" 2>/dev/null || true
    oc get executionresults -A -o yaml \
        > "$artifact_dir/executionresults.yaml" 2>/dev/null || true
    oc get verificationresults -A -o yaml \
        > "$artifact_dir/verificationresults.yaml" 2>/dev/null || true
    oc get pods -n "$namespace" -o yaml \
        > "$artifact_dir/pods.yaml" 2>/dev/null || true

    # Collect sandbox pod logs (pods named ls-analysis-*, ls-execution-*, ls-verification-*)
    for pod in $(oc get pods -n "$namespace" -o name 2>/dev/null | grep -E '^pod/ls-' | sed 's|^pod/||'); do
        oc logs -n "$namespace" "$pod" --all-containers \
            > "$artifact_dir/sandbox-${pod}.log" 2>/dev/null || true
    done

    log_info "Artifacts collected for $provider: $(find "$artifact_dir" -maxdepth 1 -type f | wc -l) files"
}
