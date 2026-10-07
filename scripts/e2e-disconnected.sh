#!/usr/bin/env bash
# Connected provisioning followed by restricted Gemma product E2E (OLS-4226).
# This caller orchestrates the service checkout's RHOAI scripts and manifests.
set -euo pipefail
# Never trace credentials inherited from CI.
set +x

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! "${LIGHTSPEED_SERVICE_REF:-}" =~ ^[[:xdigit:]]{40}$ ]]; then
    echo 'LIGHTSPEED_SERVICE_REF must be exactly 40 hexadecimal characters (a full commit SHA)' >&2
    exit 1
fi
export LIGHTSPEED_SERVICE_REF="${LIGHTSPEED_SERVICE_REF,,}"
: "${HUGGING_FACE_HUB_TOKEN:?Required for connected model preparation}"
: "${VLLM_API_KEY:?Required for the authenticated vLLM endpoint}"
: "${SANDBOX_IMAGE:?Provide a sandbox image mirrored to the OpenShift internal registry}"
if [[ "$SANDBOX_IMAGE" != image-registry.openshift-image-registry.svc:5000/* ]]; then
    echo 'SANDBOX_IMAGE must use the OpenShift cluster-local registry' >&2
    exit 1
fi
if [[ -n "${E2E_SKIP_SCENARIOS:-}" || "${E2E_SCENARIO_TAGS:-core}" != core ]]; then
    echo 'Disconnected product E2E requires core tags and no scenario exclusions' >&2
    exit 1
fi

WORKDIR="$(mktemp -d)"
export E2E_DISCONNECTED_ID="e2e-${WORKDIR##*/}"
# The product runner creates this only after its cleanup script succeeds.
export E2E_DISCONNECTED_CLEANUP_MARKER="$WORKDIR/cleanup-complete"
export E2E_DISCONNECTED_RUNS_FILE="$WORKDIR/runs.jsonl"
export E2E_DISCONNECTED_CLEANUP_SCRIPT="$SCRIPT_DIR/e2e-disconnected-cleanup.sh"
export E2E_DISCONNECTED_OPERATOR_STATE="$WORKDIR/operator-cleanup.env"
child_pid=""
output_filter_pid=""
# Preserve the original result; never retry after restoring external access.
_cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    if [[ -n "$child_pid" ]] && kill -0 "$child_pid" 2>/dev/null; then
        kill -TERM -- "-$child_pid" 2>/dev/null || true
        wait "$child_pid" || true
    fi
    if [[ -n "$output_filter_pid" ]]; then
        kill "$output_filter_pid" 2>/dev/null || true
        wait "$output_filter_pid" || true
    fi
    local cleanup_rc=0
    if [[ ! -f "$E2E_DISCONNECTED_CLEANUP_MARKER" ]]; then
        bash "$SCRIPT_DIR/e2e-disconnected-cleanup.sh" || cleanup_rc=$?
    fi
    if [[ "$cleanup_rc" -eq 0 && -f "$E2E_DISCONNECTED_OPERATOR_STATE" ]]; then
        (
            # Load library defaults, then restore the child runner's ownership.
            # shellcheck source=scripts/e2e-lib.sh
            source "$SCRIPT_DIR/e2e-lib.sh"
            # shellcheck disable=SC1090 # Produced by our child in a private workdir.
            source "$E2E_DISCONNECTED_OPERATOR_STATE"
            cd "$SCRIPT_DIR/.."
            cleanup_operator
        ) || cleanup_rc=$?
    fi
    if [[ "$rc" -eq 0 ]]; then rc=$cleanup_rc; fi
    rm -rf "$WORKDIR" || true
    exit "$rc"
}
trap _cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

git clone https://github.com/openshift/lightspeed-service.git "$WORKDIR/service"
git -C "$WORKDIR/service" cat-file -e "${LIGHTSPEED_SERVICE_REF}^{commit}"
git -C "$WORKDIR/service" checkout --detach "$LIGHTSPEED_SERVICE_REF"
if [[ "$(git -C "$WORKDIR/service" rev-parse HEAD)" != "$LIGHTSPEED_SERVICE_REF" ]]; then
    echo 'lightspeed-service checkout does not match LIGHTSPEED_SERVICE_REF' >&2
    exit 1
fi
# Existing service manifests use this namespace; expose it to failure
# diagnostics even if provisioning exits before producing its handoff.
export RHOAI_VLLM_NAMESPACE=e2e-rhoai-dsc
provision_timeout="${E2E_RHOAI_PROVISION_TIMEOUT:-120m}"
if [[ ! "$provision_timeout" =~ ^[0-9]+([.][0-9]+)?[smhd]?$ || ! "$provision_timeout" =~ [1-9] ]]; then
    echo 'E2E_RHOAI_PROVISION_TIMEOUT must be a positive GNU timeout duration' >&2
    exit 1
fi
# Bound the service scripts' discovery loops as a process group. A private FIFO
# keeps output redacted without losing the timeout/group-leader PID to a pipeline.
mkfifo "$WORKDIR/rhoai-output"
python3 "$SCRIPT_DIR/e2e-redact.py" < "$WORKDIR/rhoai-output" &
output_filter_pid=$!
setsid timeout --kill-after=30s "$provision_timeout" \
    bash "$SCRIPT_DIR/e2e-rhoai.sh" "$WORKDIR/service/tests/rhoai" "$WORKDIR" \
    > "$WORKDIR/rhoai-output" 2>&1 &
child_pid=$!
provision_rc=0
wait "$child_pid" || provision_rc=$?
child_pid=""
filter_rc=0
wait "$output_filter_pid" || filter_rc=$?
output_filter_pid=""
if [[ "$provision_rc" -ne 0 ]]; then exit "$provision_rc"; fi
if [[ "$filter_rc" -ne 0 ]]; then exit "$filter_rc"; fi
# The caller produces a safely shell-quoted, non-secret handoff from the
# discovered Service and authenticated models response.
set -a
# shellcheck disable=SC1091
source "$WORKDIR/vllm.env"
set +a
: "${RHOAI_VLLM_BASE_URL:?Provisioning did not return its internal /v1 endpoint}"
: "${RHOAI_VLLM_MODEL:?Provisioning did not return its confirmed model}"

umask 077
printf '%s' "$VLLM_API_KEY" > "$WORKDIR/vllm-key"
export OPENAI_PROVIDER_KEY_PATH="$WORKDIR/vllm-key"
export E2E_MODEL="$RHOAI_VLLM_MODEL"
export E2E_OPENAI_URL="$RHOAI_VLLM_BASE_URL"
export E2E_DISCONNECTED=true E2E_SCENARIO_TAGS=core SANDBOX_MODE=bare-pod
# The narrow boundary permits DNS/API/vLLM only, not optional OTEL/MCP traffic.
export E2E_OTEL_ENABLED=false
export E2E_SUITE_TIMEOUT="${E2E_SUITE_TIMEOUT:-12h}"
# The standard runner clones scenarios and deploys the operator while connected;
# the product test validates all images and installs policies before fixtures/runs.
setsid bash "$SCRIPT_DIR/e2e-cluster.sh" openai &
child_pid=$!
wait "$child_pid"
