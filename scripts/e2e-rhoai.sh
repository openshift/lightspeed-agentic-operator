#!/usr/bin/env bash
# Operator-owned connected orchestration using a pinned service's RHOAI assets.
# Usage: bash scripts/e2e-rhoai.sh <service-checkout>/tests/rhoai <private-workdir>
# Produces a non-secret, shell-quoted vllm.env for the restricted product suite.
set -euo pipefail
set +x

RHOAI_DIR="$(cd "${1:?Missing service RHOAI directory}" && pwd)"
WORKDIR="$(cd "${2:?Missing private work directory}" && pwd)"

# Validate the selected checkout before creating any cluster resources.
for asset in \
    scripts/model-profile.sh scripts/bootstrap.sh scripts/gpu-setup.sh \
    scripts/fetch-vllm-image.sh scripts/deploy-vllm.sh scripts/get-vllm-pod-info.sh \
    manifests/namespaces/nfd.yaml manifests/namespaces/nvidia-operator.yaml \
    manifests/operators/operatorgroup.yaml manifests/operators/operators.yaml \
    manifests/operators/ds-cluster.yaml manifests/gpu/create-nfd.yaml \
    manifests/gpu/cluster-policy.yaml manifests/vllm/vllm-runtime-gpu.yaml manifests/vllm/vllm-inference-service-gpu.yaml; do
    if [[ ! -f "$RHOAI_DIR/$asset" ]]; then
        echo "Pinned lightspeed-service revision lacks required RHOAI asset: $asset" >&2
        exit 1
    fi
done
for cmd in oc jq curl envsubst python3; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Missing connected provisioning tool: $cmd" >&2
        exit 1
    fi
done
: "${HUGGING_FACE_HUB_TOKEN:?Required for Gemma model/template preparation}"
: "${VLLM_API_KEY:?Required for authenticated inference}"
export VLLM_MODEL_PROFILE=gemma-4-31b
# shellcheck disable=SC1091 # The profile is supplied by the verified service checkout.
source "$RHOAI_DIR/scripts/model-profile.sh"
load_vllm_model_profile
: "${VLLM_MODEL:?Gemma profile must define its model}"
: "${VLLM_CHAT_TEMPLATE_URL:?Gemma profile must define its chat template URL}"
: "${VLLM_CHAT_TEMPLATE_CONFIGMAP:?Gemma profile must define its template ConfigMap}"
: "${VLLM_CHAT_TEMPLATE_KEY:?Gemma profile must define its template key}"
if [[ "${VLLM_TOOL_CALL_PARSER:-}" != gemma4 || ! "${VLLM_GPU_COUNT:-}" =~ ^[1-9][0-9]*$ ]]; then
    echo 'Pinned Gemma profile must define the gemma4 tool parser and GPU count' >&2
    exit 1
fi
if [[ "$VLLM_CHAT_TEMPLATE_URL" != https://huggingface.co/* ]]; then
    echo 'Gemma chat template must use an HTTPS Hugging Face URL' >&2
    exit 1
fi
if [[ "$HUGGING_FACE_HUB_TOKEN" == *$'\n'* || "$HUGGING_FACE_HUB_TOKEN" == *$'\r'* ]]; then
    echo 'Invalid Hugging Face token format' >&2
    exit 1
fi

# Names come from the service's existing RawDeployment manifests.
export RHOAI_VLLM_NAMESPACE=e2e-rhoai-dsc
export RHOAI_VLLM_SERVICE_NAME=vllm-model-predictor
isvc=vllm-model
umask 077
printf '%s' "$HUGGING_FACE_HUB_TOKEN" > "$WORKDIR/hf-token"
printf '%s' "$VLLM_API_KEY" > "$WORKDIR/vllm-key"
printf 'Authorization: Bearer %s\n' "$HUGGING_FACE_HUB_TOKEN" > "$WORKDIR/hf-header"

oc apply -f "$RHOAI_DIR/manifests/namespaces/nfd.yaml"
oc apply -f "$RHOAI_DIR/manifests/namespaces/nvidia-operator.yaml"
bash "$RHOAI_DIR/scripts/bootstrap.sh" "$RHOAI_DIR"
bash "$RHOAI_DIR/scripts/gpu-setup.sh" "$RHOAI_DIR"
oc get namespace "$RHOAI_VLLM_NAMESPACE" &>/dev/null || oc create namespace "$RHOAI_VLLM_NAMESPACE"
# Secret-backed environment values do not update in existing Pods. Restart a
# reused RawDeployment after applying the profile/credentials, but do not force
# a second model download on the first deployment.
restart_inference=false
if oc get deployment "$RHOAI_VLLM_SERVICE_NAME" -n "$RHOAI_VLLM_NAMESPACE" &>/dev/null; then
    restart_inference=true
fi
# Keep credential values out of process arguments and generated artifacts.
oc create secret generic hf-token-secret -n "$RHOAI_VLLM_NAMESPACE" \
    --from-file="token=$WORKDIR/hf-token" --dry-run=client -o yaml | oc apply -f -
oc create secret generic vllm-api-key-secret -n "$RHOAI_VLLM_NAMESPACE" \
    --from-file="key=$WORKDIR/vllm-key" --dry-run=client -o yaml | oc apply -f -

curl --fail --location --connect-timeout 30 --max-time 300 \
    --header "@$WORKDIR/hf-header" --output "$WORKDIR/chat-template.jinja" "$VLLM_CHAT_TEMPLATE_URL"
oc create configmap "$VLLM_CHAT_TEMPLATE_CONFIGMAP" -n "$RHOAI_VLLM_NAMESPACE" \
    --from-file="$VLLM_CHAT_TEMPLATE_KEY=$WORKDIR/chat-template.jinja" --dry-run=client -o yaml | oc apply -f -
# shellcheck disable=SC1091 # Exports VLLM_IMAGE for the service's deployment script.
source "$RHOAI_DIR/scripts/fetch-vllm-image.sh"
: "${VLLM_IMAGE:?Service image discovery did not return a vLLM image}"
bash -e -o pipefail "$RHOAI_DIR/scripts/deploy-vllm.sh" "$RHOAI_DIR"
if [[ "$restart_inference" == true ]]; then
    oc rollout restart "deployment/$RHOAI_VLLM_SERVICE_NAME" -n "$RHOAI_VLLM_NAMESPACE"
    oc rollout status "deployment/$RHOAI_VLLM_SERVICE_NAME" -n "$RHOAI_VLLM_NAMESPACE" \
        --timeout="${E2E_RHOAI_READY_TIMEOUT:-60m}"
fi
oc wait --for=condition=Ready "inferenceservice/$isvc" -n "$RHOAI_VLLM_NAMESPACE" \
    --timeout="${E2E_RHOAI_READY_TIMEOUT:-60m}"
ENV_FILE="$WORKDIR/pod.env" bash "$RHOAI_DIR/scripts/get-vllm-pod-info.sh" "$isvc"

# Do not source pod.env or trust its KSVC_URL: that helper currently uses the
# target port in its Service URL. Clients must use the published Service port.
oc get service "$RHOAI_VLLM_SERVICE_NAME" -n "$RHOAI_VLLM_NAMESPACE" -o json > "$WORKDIR/service.json"
service_port="$(jq -cer '
    [.spec.ports[] | select((.protocol // "TCP") == "TCP")] as $tcp |
    [$tcp[] | select(.name == "http1")] as $http |
    if ($http | length) == 1 then $http[0]
    elif ($tcp | length) == 1 then $tcp[0]
    else error("ambiguous inference Service port") end
' "$WORKDIR/service.json")"
export RHOAI_VLLM_SERVICE_PORT
RHOAI_VLLM_SERVICE_PORT="$(jq -er '.port | select(type == "number" and . == floor and . > 0 and . <= 65535)' <<< "$service_port")" || {
    echo 'Inference Service port must be an integer in 1..65535' >&2
    exit 1
}
export RHOAI_VLLM_POD_SELECTOR_JSON
RHOAI_VLLM_POD_SELECTOR_JSON="$(jq -cer '
    .spec.selector | select(type == "object" and length > 0) |
    select(all(.[]; type == "string")) | {matchLabels: .}
' "$WORKDIR/service.json")" || { echo 'Inference Service requires a nonempty valid Pod selector' >&2; exit 1; }
selector="$(jq -r '.matchLabels | to_entries | map(.key + "=" + .value) | join(",")' <<< "$RHOAI_VLLM_POD_SELECTOR_JSON")"
oc get pods -n "$RHOAI_VLLM_NAMESPACE" -l "$selector" -o json > "$WORKDIR/inference-pods.json"
pod="$(jq -er '[.items[] | select(.status.phase == "Running" and .metadata.deletionTimestamp == null)][0].metadata.name | select(type == "string" and length > 0)' "$WORKDIR/inference-pods.json")"
target_port="$(jq -r '.targetPort // .port' <<< "$service_port")"
if jq -e '.targetPort | type == "string"' <<< "$service_port" >/dev/null; then
    target_port="$(jq -er --arg name "$target_port" '
        [.items[].spec.containers[] | select(.name == "kserve-container") |
         .ports[]? | select(.name == $name) | .containerPort] | unique |
        select(length == 1) | .[0]
    ' "$WORKDIR/inference-pods.json")" || {
        echo 'Could not resolve the named inference Pod target port' >&2
        exit 1
    }
fi
if [[ ! "$target_port" =~ ^[0-9]+$ ]] || (( target_port < 1 || target_port > 65535 )); then
    echo 'Could not resolve a valid inference Pod target port' >&2
    exit 1
fi
export RHOAI_VLLM_NETWORK_PORT="$target_port"
export RHOAI_VLLM_BASE_URL="http://${RHOAI_VLLM_SERVICE_NAME}.${RHOAI_VLLM_NAMESPACE}.svc:${RHOAI_VLLM_SERVICE_PORT}/v1"

# Cluster Service DNS is not necessarily resolvable on the runner host. Check
# the authenticated models API inside the existing inference container, using
# stdin to validate the caller's current key, not a possibly stale Pod environment.
# The key is never present in exec arguments.
oc exec -i -n "$RHOAI_VLLM_NAMESPACE" "$pod" -c kserve-container -- python3 -c '
import json, sys, urllib.request
request = urllib.request.Request(sys.argv[1]+"/models", headers={"Authorization": "Bearer "+sys.stdin.read().strip()})
with urllib.request.urlopen(request, timeout=30) as response:
    print(json.dumps(json.load(response)))
' "$RHOAI_VLLM_BASE_URL" < "$WORKDIR/vllm-key" > "$WORKDIR/models.json"
export RHOAI_VLLM_MODEL
RHOAI_VLLM_MODEL="$(jq -er --arg model "$VLLM_MODEL" '.data[] | select(.id == $model) | .id' "$WORKDIR/models.json")" || {
    echo 'Authenticated vLLM models response does not advertise the requested Gemma model' >&2
    exit 1
}

# This handoff is produced by the operator harness, not a service entrypoint.
# Emit only non-secret fields using Bash quoting; restricted tests verify that
# the selector matches exactly the owning InferenceService's backing Pods.
for name in RHOAI_VLLM_BASE_URL RHOAI_VLLM_MODEL RHOAI_VLLM_NAMESPACE \
    RHOAI_VLLM_SERVICE_NAME RHOAI_VLLM_SERVICE_PORT RHOAI_VLLM_NETWORK_PORT \
    RHOAI_VLLM_POD_SELECTOR_JSON; do
    printf '%s=%q\n' "$name" "${!name}"
done > "$WORKDIR/vllm.env"
echo "Gemma provisioning ready: model=$RHOAI_VLLM_MODEL endpoint=$RHOAI_VLLM_BASE_URL"
