#!/usr/bin/env bash
#
# Usage:
#   # Tekton (SNAPSHOT set by pipeline):
#   bash scripts/e2e-cluster.sh
#
#   # Local (specify providers and IMG directly):
#   IMG=quay.io/... VERTEX_PROVIDER_KEY_PATH=/path/to/creds.json \
#     VERTEX_PROJECT_ID=my-project OPENAI_PROVIDER_KEY_PATH=/path/to/key \
#     bash scripts/e2e-cluster.sh claude openai
#
# Required env:
#   IMG or SNAPSHOT          — operator image
#   VERTEX_PROVIDER_KEY_PATH — GCP SA JSON (for claude/gemini)
#   VERTEX_PROJECT_ID        — GCP project ID (for claude/gemini)
#   OPENAI_PROVIDER_KEY_PATH — OpenAI API key file (for openai)
#
# Optional env:
#   OPERATOR_NAMESPACE       — default: openshift-lightspeed
#   VERTEX_REGION            — default: global
#   E2E_POLL_TIMEOUT          — default: 20m
#   E2E_MODEL                — override default model for the provider
#   ARTIFACT_DIR             — directory for test artifacts
#   KONFLUX_COMPONENT_NAME   — component name for SNAPSHOT parsing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# shellcheck source=scripts/e2e-lib.sh
source "$SCRIPT_DIR/e2e-lib.sh"

PROVIDERS="${*:-claude gemini openai}"
NAMESPACE="${OPERATOR_NAMESPACE:-openshift-lightspeed}"
export OPERATOR_NAMESPACE="$NAMESPACE"

log_info "=== e2e-cluster.sh ==="
log_info "Providers: $PROVIDERS"
log_info "Namespace: $NAMESPACE"

check_prerequisites
parse_snapshot

cd "$REPO_ROOT"

deploy_operator

declare -A results
overall_rc=0

run_provider() {
    local provider="$1"
    local model
    model="$(resolve_model "$provider")"
    local key_path

    case "$provider" in
        claude|gemini)
            key_path="${VERTEX_PROVIDER_KEY_PATH:?Missing VERTEX_PROVIDER_KEY_PATH}"
            ;;
        openai)
            key_path="${OPENAI_PROVIDER_KEY_PATH:?Missing OPENAI_PROVIDER_KEY_PATH}"
            ;;
        *)
            log_error "Unknown provider: $provider"
            return 1
            ;;
    esac

    log_info "--- Running e2e for provider=$provider model=$model ---"

    create_provider_fixtures "$provider"

    local test_rc=0

    if [[ -n "${ARTIFACT_DIR:-}" ]]; then
        mkdir -p "$ARTIFACT_DIR/$provider"
        E2E_PROVIDER="$provider" \
        E2E_MODEL="$model" \
        E2E_PROVIDER_KEY_PATH="$key_path" \
        E2E_POLL_TIMEOUT="${E2E_POLL_TIMEOUT:-20m}" \
        VERTEX_PROJECT_ID="${VERTEX_PROJECT_ID:-}" \
        VERTEX_REGION="${VERTEX_REGION:-global}" \
        TEST_NAMESPACE="$NAMESPACE" \
        make test-e2e 2>&1 | tee "$ARTIFACT_DIR/$provider/test-output.log" || test_rc=$?
    else
        E2E_PROVIDER="$provider" \
        E2E_MODEL="$model" \
        E2E_PROVIDER_KEY_PATH="$key_path" \
        E2E_POLL_TIMEOUT="${E2E_POLL_TIMEOUT:-20m}" \
        VERTEX_PROJECT_ID="${VERTEX_PROJECT_ID:-}" \
        VERTEX_REGION="${VERTEX_REGION:-global}" \
        TEST_NAMESPACE="$NAMESPACE" \
        make test-e2e || test_rc=$?
    fi

    collect_artifacts "$provider"
    cleanup_provider_fixtures "$provider"

    return "$test_rc"
}

_cleanup_on_exit() {
    log_info "Running cleanup..."
    for provider in $PROVIDERS; do
        cleanup_provider_fixtures "$provider" 2>/dev/null || true
    done
    cleanup_operator
}
trap _cleanup_on_exit EXIT

for provider in $PROVIDERS; do
    set +e
    run_provider "$provider"
    provider_rc=$?
    set -e
    results["$provider"]=$provider_rc
    (( overall_rc = overall_rc || provider_rc ))
done

log_info "=== Results ==="
printf "%-12s %s\n" "PROVIDER" "STATUS" >&2
printf "%-12s %s\n" "--------" "------" >&2
for provider in $PROVIDERS; do
    if [[ "${results[$provider]}" -eq 0 ]]; then
        printf "%-12s %s\n" "$provider" "PASS" >&2
    else
        printf "%-12s %s\n" "$provider" "FAIL" >&2
    fi
done

if [[ "$overall_rc" -ne 0 ]]; then
    log_error "One or more providers failed"
fi

exit "$overall_rc"
