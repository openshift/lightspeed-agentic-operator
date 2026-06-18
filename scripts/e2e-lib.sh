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

default_model_for_provider() {
    local provider="$1"
    case "$provider" in
        claude) echo "claude-sonnet-4-6" ;;
        gemini) echo "gemini-2.5-flash" ;;
        openai) echo "gpt-4.1-mini" ;;
        *) log_error "Unknown provider: $provider"; return 1 ;;
    esac
}

resolve_model() {
    local provider="$1"
    local override_var="${provider^^}_MODEL"
    echo "${!override_var:-$(default_model_for_provider "$provider")}"
}

create_provider_fixtures() {
    local provider="$1"
    local namespace="${OPERATOR_NAMESPACE:-openshift-lightspeed}"
    local model
    model="$(resolve_model "$provider")"

    local secret_name="e2e-${provider}-secret"
    local llm_name="e2e-${provider}-llm"
    local agent_name="e2e-agent"

    log_info "Creating fixtures for provider=$provider model=$model"

    case "$provider" in
        claude|gemini)
            local key_path="${VERTEX_PROVIDER_KEY_PATH:?Missing VERTEX_PROVIDER_KEY_PATH for provider $provider}"
            local project_id="${VERTEX_PROJECT_ID:?Missing VERTEX_PROJECT_ID for provider $provider}"
            local region="${VERTEX_REGION:-us-central1}"
            local model_provider
            case "$model" in
                claude-*) model_provider="Anthropic" ;;
                gemini-*) model_provider="Google" ;;
                *) log_error "Cannot infer modelProvider from model '$model'"; return 1 ;;
            esac

            oc create secret generic "$secret_name" \
                --from-file=GOOGLE_APPLICATION_CREDENTIALS="$key_path" \
                -n "$namespace" --dry-run=client -o yaml | oc apply -f -

            oc apply -f - <<EOF
apiVersion: agentic.openshift.io/v1alpha1
kind: LLMProvider
metadata:
  name: $llm_name
spec:
  type: GoogleCloudVertex
  googleCloudVertex:
    credentialsSecret:
      name: $secret_name
    projectID: $project_id
    region: $region
    modelProvider: $model_provider
EOF
            ;;
        openai)
            local key_path="${OPENAI_PROVIDER_KEY_PATH:?Missing OPENAI_PROVIDER_KEY_PATH for provider $provider}"

            oc create secret generic "$secret_name" \
                --from-file=OPENAI_API_KEY="$key_path" \
                -n "$namespace" --dry-run=client -o yaml | oc apply -f -

            oc apply -f - <<EOF
apiVersion: agentic.openshift.io/v1alpha1
kind: LLMProvider
metadata:
  name: $llm_name
spec:
  type: OpenAI
  openAI:
    credentialsSecret:
      name: $secret_name
EOF
            ;;
        *)
            log_error "Unknown provider: $provider"
            return 1
            ;;
    esac

    oc apply -f - <<EOF
apiVersion: agentic.openshift.io/v1alpha1
kind: Agent
metadata:
  name: $agent_name
spec:
  llmProvider:
    name: $llm_name
  model: $model
EOF

    log_info "Fixtures created for provider=$provider"
}

cleanup_provider_fixtures() {
    local provider="$1"
    local namespace="${OPERATOR_NAMESPACE:-openshift-lightspeed}"
    local secret_name="e2e-${provider}-secret"
    local llm_name="e2e-${provider}-llm"
    local agent_name="e2e-agent"

    log_info "Cleaning up fixtures for provider=$provider"
    oc delete agent "$agent_name" --ignore-not-found 2>/dev/null || true
    oc delete llmprovider "$llm_name" --ignore-not-found 2>/dev/null || true
    oc delete secret "$secret_name" -n "$namespace" --ignore-not-found 2>/dev/null || true
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
    oc get proposals -A -o yaml \
        > "$artifact_dir/proposals.yaml" 2>/dev/null || true
    oc get proposalapprovals -A -o yaml \
        > "$artifact_dir/proposalapprovals.yaml" 2>/dev/null || true
    oc get analysisresults -A -o yaml \
        > "$artifact_dir/analysisresults.yaml" 2>/dev/null || true
    oc get executionresults -A -o yaml \
        > "$artifact_dir/executionresults.yaml" 2>/dev/null || true
    oc get verificationresults -A -o yaml \
        > "$artifact_dir/verificationresults.yaml" 2>/dev/null || true
    oc get pods -n "$namespace" -o yaml \
        > "$artifact_dir/pods.yaml" 2>/dev/null || true

    log_info "Artifacts collected for $provider: $(ls "$artifact_dir" | wc -l) files"
}
