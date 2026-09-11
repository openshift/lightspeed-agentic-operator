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
#   E2E_OTEL_ENABLED         — deploy persistent OTEL/Postgres collector for
#                              sandbox-log artifacts (default: true)
#   E2E_OTEL_IMAGE           — override the OTEL collector image

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# shellcheck source=scripts/e2e-lib.sh
source "$SCRIPT_DIR/e2e-lib.sh"

PROVIDERS="${*:-claude gemini openai}"
NAMESPACE="${OPERATOR_NAMESPACE:-openshift-lightspeed}"
export OPERATOR_NAMESPACE="$NAMESPACE"
SCENARIOS_TMPDIR=""

# shellcheck disable=SC2329 # Invoked indirectly by the EXIT trap.
_cleanup_on_exit() {
    log_info "Running cleanup..."
    cleanup_e2e_otel "$NAMESPACE"
    cleanup_operator
    if [[ -n "${SCENARIOS_TMPDIR:-}" && -d "$SCENARIOS_TMPDIR" ]]; then
        log_info "Removing scenarios clone: $SCENARIOS_TMPDIR"
        rm -rf "$SCENARIOS_TMPDIR"
    fi
}
trap _cleanup_on_exit EXIT

log_info "=== e2e-cluster.sh ==="
log_info "Providers: $PROVIDERS"
log_info "Namespace: $NAMESPACE"

check_prerequisites
parse_snapshot

cd "$REPO_ROOT"

deploy_operator
ensure_e2e_otel "$NAMESPACE"

# Clone troubleshooting scenarios from rhobs for product_e2e tests.
SCENARIOS_TMPDIR="$(mktemp -d)"
log_info "Cloning rhobs/troubleshooting-scenarios → $SCENARIOS_TMPDIR"
git clone --depth 1 https://github.com/rhobs/troubleshooting-scenarios.git "$SCENARIOS_TMPDIR"
export E2E_SCENARIOS_DIR="$SCENARIOS_TMPDIR"

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

    local e2e_env=(
        E2E_PROVIDER="$provider"
        E2E_MODEL="$model"
        E2E_PROVIDER_KEY_PATH="$key_path"
        E2E_POLL_TIMEOUT="${E2E_POLL_TIMEOUT:-20m}"
        VERTEX_PROJECT_ID="${VERTEX_PROJECT_ID:-}"
        VERTEX_REGION="${VERTEX_REGION:-global}"
        TEST_NAMESPACE="$NAMESPACE"
    )

    local test_rc=0

    # Product e2e (troubleshooting scenarios against real LLMs).
    # Mock-only tests (failure_test.go etc.) run in precommit CI via `make test-e2e`.
    if [[ -n "${E2E_SCENARIOS_DIR:-}" ]]; then
        log_info "--- Running product e2e for provider=$provider ---"
        if [[ -n "${ARTIFACT_DIR:-}" ]]; then
            mkdir -p "$ARTIFACT_DIR/$provider"
            env "${e2e_env[@]}" E2E_SCENARIOS_DIR="$E2E_SCENARIOS_DIR" \
                go test -tags=product_e2e ./test/e2e/... -count=1 -v -timeout 120m \
                2>&1 | tee "$ARTIFACT_DIR/$provider/product-e2e-output.log" || test_rc=$?
        else
            env "${e2e_env[@]}" E2E_SCENARIOS_DIR="$E2E_SCENARIOS_DIR" \
                go test -tags=product_e2e ./test/e2e/... -count=1 -v -timeout 120m || test_rc=$?
        fi
    fi

    collect_artifacts "$provider"

    return "$test_rc"
}

for provider in $PROVIDERS; do
    set +e
    run_provider "$provider"
    provider_rc=$?
    set -e
    results["$provider"]=$provider_rc
    (( overall_rc = overall_rc || provider_rc )) || true
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
