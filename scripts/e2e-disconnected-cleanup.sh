#!/usr/bin/env bash
# Shared normal/timeout teardown. Never restore egress before sandboxes are gone.
set -uo pipefail
set +x
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${E2E_DISCONNECTED_ID:?Missing invocation ID}"
if [[ -n "${E2E_DISCONNECTED_CLEANUP_MARKER:-}" && -f "$E2E_DISCONNECTED_CLEANUP_MARKER" ]]; then
    exit 0
fi
selector="agentic.openshift.io/disconnected-e2e=$E2E_DISCONNECTED_ID"
resources=pods,secrets,networkpolicies,serviceaccounts,rolebindings,llmproviders,agents,approvalpolicies
umask 077
workdir="$(mktemp -d)" || exit 1
trap 'rm -rf "$workdir"' EXIT

# Unavailable optional diagnostics (for example logs for a never-started Pod)
# are recorded as error text. Failed writes/redaction or required API fetches
# must stop cleanup before it destroys the evidence sources.
_archive() {
    local mode="$1" path="$2"
    shift 2
    "$@" 2>&1 | python3 "$SCRIPT_DIR/e2e-redact.py" > "$path"
    local statuses=("${PIPESTATUS[@]}")
    if [[ "${statuses[1]}" -ne 0 || ( "$mode" == required && "${statuses[0]}" -ne 0 ) ]]; then
        echo "Cannot archive $path; preserving resources" >&2
        return 1
    fi
}

# Provisioning can fail before the operator/CRDs exist. Collect its evidence
# before requiring workflow APIs, but never delete resources if those APIs fail.
dir=""
if [[ -n "${ARTIFACT_DIR:-}" ]]; then
    dir="$ARTIFACT_DIR/disconnected/fallback"
    mkdir -p "$dir" || exit 1
    # Recovery must not replace evidence collected before a partial deletion.
    if [[ -f "$dir/owned-resources.yaml" ]]; then
        dir="$(mktemp -d "$dir/retry.XXXXXX")" || exit 1
    fi
    # Never collect Secret data or registry credential contents.
    _archive optional "$dir/owned-resources.yaml" \
        oc get pods,networkpolicies,serviceaccounts,rolebindings -A -l "$selector" -o yaml --request-timeout=30s || exit 1
    _archive optional "$dir/operator.log" \
        oc logs deployment/controller-manager -n "${OPERATOR_NAMESPACE:-openshift-lightspeed}" --tail=500 --request-timeout=30s || exit 1
    if [[ -n "${RHOAI_VLLM_NAMESPACE:-}" ]]; then
        _archive optional "$dir/inference.yaml" \
            oc get pods,events,inferenceservices.serving.kserve.io,servingruntimes.serving.kserve.io \
            -n "$RHOAI_VLLM_NAMESPACE" -o yaml --request-timeout=30s || exit 1
        _archive optional "$dir/inference.log" \
            oc logs -n "$RHOAI_VLLM_NAMESPACE" -l serving.kserve.io/inferenceservice=vllm-model \
            --all-containers --prefix --tail=500 --request-timeout=30s || exit 1
    fi
fi

# Keep identities after per-scenario deletion: sandbox GC may still be pending.
oc get agenticruns -A -l "$selector" -o json --request-timeout=30s > "$workdir/live-runs.json" || exit 1
jq -c '.items[]? | {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid}' \
    "$workdir/live-runs.json" > "$workdir/records.jsonl" || exit 1
if [[ -n "${E2E_DISCONNECTED_RUNS_FILE:-}" && -f "$E2E_DISCONNECTED_RUNS_FILE" ]]; then
    jq -c '.' "$E2E_DISCONNECTED_RUNS_FILE" >> "$workdir/records.jsonl" || exit 1
fi
# Validate before using journal fields as selectors or artifact paths. A damaged
# journal fails closed, leaving policies/controller available for recovery.
records="$(jq -cs '
    unique_by(.uid) | .[] |
    if (.namespace | type == "string" and test("^[a-z0-9][a-z0-9-]*$")) and
       (.name | type == "string" and test("^[a-z0-9][a-z0-9.-]*$")) and
       (.uid | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9_.-]*$"))
    then . else error("invalid disconnected run identity") end
' "$workdir/records.jsonl")" || exit 1
records_tsv="$(jq -r '[.namespace, .name, .uid] | @tsv' <<< "$records")" || exit 1
if [[ -n "$records" ]]; then
    : "${E2E_DISCONNECTED_RUNS_FILE:?Run journal is required before destructive cleanup}"
    # Also retain runs discovered after interruption or a failed Go journal write.
    # Otherwise deleting their CR would lose sandbox ownership on the next retry.
    jq -c '.items[]? | {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid}' \
        "$workdir/live-runs.json" >> "$E2E_DISCONNECTED_RUNS_FILE" || exit 1
fi

if [[ -n "$dir" ]]; then
    while IFS=$'\t' read -r namespace _name uid; do
        [[ -n "$uid" ]] || continue
        run_dir="$dir/runs/$uid"
        mkdir -p "$run_dir" || exit 1
        # shellcheck disable=SC2016 # $uid is a jq variable supplied with --arg.
        _archive required "$run_dir/identity.json" \
            jq --arg uid "$uid" 'select(.uid == $uid)' <<< "$records" || exit 1
        # Use the owned snapshot, not a name lookup that could find a reused run.
        # shellcheck disable=SC2016 # $uid is a jq variable supplied with --arg.
        _archive required "$run_dir/agenticrun.json" \
            jq --arg uid "$uid" '.items[]? | select(.metadata.uid == $uid)' "$workdir/live-runs.json" || exit 1
        for resource in analysisresults executionresults verificationresults pods; do
            _archive required "$run_dir/$resource.yaml" \
                oc get "$resource" -n "$namespace" -l "agentic.openshift.io/run=$uid" -o yaml --request-timeout=30s || exit 1
        done
        _archive optional "$run_dir/sandbox.log" \
            oc logs -n "$namespace" -l "agentic.openshift.io/run=$uid" \
            --all-containers --prefix --tail=500 --request-timeout=30s || exit 1
    done <<< "$records_tsv"
fi

# Mark runs deleting first so the controller cannot recreate their sandboxes.
oc delete agenticruns -A -l "$selector" --ignore-not-found --wait=false --request-timeout=30s || exit 1
while IFS=$'\t' read -r namespace _name uid; do
    [[ -n "$uid" ]] || continue
    oc delete pods -n "$namespace" -l "agentic.openshift.io/run=$uid" \
        --ignore-not-found --wait=true --timeout=60s --request-timeout=30s || exit 1
done <<< "$records_tsv"
oc delete agenticruns,agenticrunapprovals -A -l "$selector" --ignore-not-found --wait=true --timeout=60s --request-timeout=30s || exit 1
# Check again after finalizers finish, including Pods whose run CR is already gone.
while IFS=$'\t' read -r namespace _name uid; do
    [[ -n "$uid" ]] || continue
    remaining="$(oc get pods -n "$namespace" -l "agentic.openshift.io/run=$uid" -o name --request-timeout=30s)" || exit 1
    if [[ -n "$remaining" ]]; then
        echo "Disconnected cleanup left sandbox Pods for run $uid; preserving policies" >&2
        exit 1
    fi
done <<< "$records_tsv"
oc delete "$resources" -A -l "$selector" --ignore-not-found --wait=true --timeout=60s --request-timeout=30s || true
remaining="$(oc get "$resources",agenticruns,agenticrunapprovals -A -l "$selector" -o name --request-timeout=30s)" || exit 1
if [[ -n "$remaining" ]]; then
    echo 'Disconnected cleanup left invocation-owned resources' >&2
    exit 1
fi
if [[ -n "${E2E_DISCONNECTED_CLEANUP_MARKER:-}" ]]; then
    touch "$E2E_DISCONNECTED_CLEANUP_MARKER" || exit 1
fi
