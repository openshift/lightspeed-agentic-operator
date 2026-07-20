#!/usr/bin/env bash
#
# Quickstart installer for Agentic OLS.
# Installs or updates Lightspeed Operator via OLM (operator-sdk run bundle /
# bundle-upgrade) when needed, then deploys the agentic operator from pre-built
# Konflux images. Pass --ols-bundle-image=IMAGE to choose the OLS bundle;
# otherwise the script resolves lightspeed-operator-bundle from
# related_images.json and prompts (decline / option 3 stops). No building or
# cloning of this repo is required; operator-sdk is downloaded on demand if
# missing from PATH.
#
# Usage (download then run — required for interactive OLS prompts):
#   curl -fsSL -o install-agentic.sh \
#     https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/install.sh
#   bash install-agentic.sh
#   bash install-agentic.sh --ols-bundle-image=quay.io/.../lightspeed-operator-bundle:tag
#
# Do not use: curl … | bash  (stdin is the script; prompts cannot read y/n)
#
# Prerequisites:
#   - oc CLI on PATH
#   - python3 on PATH only when --ols-bundle-image is omitted (related_images.json fallback)
#   - Logged into the target OpenShift cluster
#   - cluster-admin privileges
#   - Interactive terminal (TTY) for Lightspeed Operator install/update prompts
#
# Note: The console plugin requires OpenShift 4.22+.
#       Set CONSOLE_IMAGE="" to skip console deployment on older clusters.
#       The console is deployed as a standalone workload (not operator-managed).

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-lightspeed}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-operator:main}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-sandbox:main}"
CONSOLE_IMAGE="${CONSOLE_IMAGE:-quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-console:main}"
SANDBOX_MODE="${SANDBOX_MODE:-bare-pod}"
IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-}"

GITHUB_RAW="${GITHUB_RAW:-https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main}"
# Preferred OLS bundle image. Flag --ols-bundle-image overrides; env OLS_BUNDLE_IMAGE is a fallback.
# When neither is set, install/update resolves the bundle from related_images.json.
OLS_BUNDLE_IMAGE="${OLS_BUNDLE_IMAGE:-}"
# Git ref for openshift/lightspeed-operator related_images.json (used only when OLS_BUNDLE_IMAGE is empty).
OLS_GIT_REF="${OLS_GIT_REF:-main}"
OLS_RELATED_IMAGES_URL="${OLS_RELATED_IMAGES_URL:-https://raw.githubusercontent.com/openshift/lightspeed-operator/${OLS_GIT_REF}/related_images.json}"
OPERATOR_SDK_VERSION="${OPERATOR_SDK_VERSION:-1.36.1}"
BUNDLE_TIMEOUT="${BUNDLE_TIMEOUT:-30m}"
OLS_CONFIG_FILE="${OLS_CONFIG_FILE:-${TMPDIR:-/tmp}/olsconfig-quickstart.yaml}"
# How long to wait for OLSConfig status.overallStatus=Ready after each apply.
OLS_CONFIG_TIMEOUT_SEC="${OLS_CONFIG_TIMEOUT_SEC:-900}"

info()  { echo "  ✓ $*"; }
step()  { echo "[${1}] ${2}"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }
warn()  { echo "  ⚠ $*" >&2; }

usage() {
  cat <<EOF
Usage: bash install-agentic.sh [options]

Options:
  --ols-bundle-image=IMAGE   Lightspeed Operator OLM bundle image for
                             operator-sdk run bundle / bundle-upgrade.
                             Always install/update when set (no prompt).
                             If omitted, resolves lightspeed-operator-bundle
                             from related_images.json (${OLS_RELATED_IMAGES_URL})
                             and prompts (decline / option 3 stops the script).
  -h, --help                 Show this help and exit

Environment variables (OPERATOR_IMAGE, SANDBOX_IMAGE, NAMESPACE, …) are
documented in hack/quickstart/README.md.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --ols-bundle-image=*)
      OLS_BUNDLE_IMAGE="${1#*=}"
      [ -n "${OLS_BUNDLE_IMAGE}" ] || fail "--ols-bundle-image requires a non-empty image"
      shift
      ;;
    --ols-bundle-image)
      [ $# -ge 2 ] || fail "--ols-bundle-image requires an image argument"
      OLS_BUNDLE_IMAGE="$2"
      [ -n "${OLS_BUNDLE_IMAGE}" ] || fail "--ols-bundle-image requires a non-empty image"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1 (try --help)"
      ;;
  esac
done

# Trim leading/trailing whitespace from flag or env. Whitespace-only → empty → error
# if the user provided a blank value; truly unset/empty stays on the related_images path.
_ols_bundle_raw="${OLS_BUNDLE_IMAGE}"
OLS_BUNDLE_IMAGE="${OLS_BUNDLE_IMAGE#"${OLS_BUNDLE_IMAGE%%[![:space:]]*}"}"
OLS_BUNDLE_IMAGE="${OLS_BUNDLE_IMAGE%"${OLS_BUNDLE_IMAGE##*[![:space:]]}"}"
if [ -n "${_ols_bundle_raw}" ] && [ -z "${OLS_BUNDLE_IMAGE}" ]; then
  fail "--ols-bundle-image / OLS_BUNDLE_IMAGE must be non-empty"
fi
unset _ols_bundle_raw

if [ -n "${OLS_BUNDLE_IMAGE}" ] && [[ "${OLS_BUNDLE_IMAGE}" != *[:/]* ]]; then
  fail "OLS_BUNDLE_IMAGE looks invalid: ${OLS_BUNDLE_IMAGE}"
fi

# Set when a related_images.json bundle is resolved (for logging / summary).
OLS_RELATED_BUNDLE_IMAGE=""
# install | update | left-as-is | none — what happened to OLS in this run.
OLS_ACTION="none"

CRD_FILES=(
  agentic.openshift.io_agenticolsconfigs.yaml
  agentic.openshift.io_agents.yaml
  agentic.openshift.io_analysisresults.yaml
  agentic.openshift.io_approvalpolicies.yaml
  agentic.openshift.io_escalationresults.yaml
  agentic.openshift.io_executionresults.yaml
  agentic.openshift.io_llmproviders.yaml
  agentic.openshift.io_agenticrunapprovals.yaml
  agentic.openshift.io_agenticruns.yaml
  agentic.openshift.io_verificationresults.yaml
)

# Prefer local CRD files when running from a checkout; fall back to GitHub.
REPO_ROOT=""
if [ -n "${BASH_SOURCE[0]:-}" ]; then
  _candidate="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  if [ -d "${_candidate}/config/crd/bases" ]; then
    REPO_ROOT="${_candidate}"
  fi
fi

ols_crd_installed() {
  oc get crd olsconfigs.ols.openshift.io >/dev/null 2>&1
}

require_tty_for_prompts() {
  if [[ ! -t 0 ]]; then
    fail "Interactive prompts require a terminal.

  Do not pipe the script into bash (curl … | bash).
  Download it, then run it:

    curl -fsSL -o install-agentic.sh \\
      ${GITHUB_RAW}/hack/quickstart/install.sh
    bash install-agentic.sh"
  fi
}

prompt_yes_no() {
  # $1 = prompt text. Returns 0 for yes, 1 for no.
  require_tty_for_prompts
  local reply
  while true; do
    printf '%s [y/n]: ' "$1"
    read -r reply
    case "${reply}" in
      [yY]|[yY][eE][sS]) return 0 ;;
      [nN]|[nN][oO]) return 1 ;;
      *) echo "  Unexpected input: '${reply}'. Please answer y or n." >&2 ;;
    esac
  done
}

ensure_operator_sdk() {
  if command -v operator-sdk >/dev/null 2>&1; then
    OPERATOR_SDK_BIN="$(command -v operator-sdk)"
    return 0
  fi
  command -v curl >/dev/null 2>&1 || fail "curl is required to download operator-sdk"
  local os arch dest url
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail "unsupported OS for operator-sdk download: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail "unsupported arch for operator-sdk download: $(uname -m)" ;;
  esac
  dest="${TMPDIR:-/tmp}/operator-sdk-${OPERATOR_SDK_VERSION}"
  url="https://github.com/operator-framework/operator-sdk/releases/download/v${OPERATOR_SDK_VERSION}/operator-sdk_${os}_${arch}"
  echo "  Downloading operator-sdk v${OPERATOR_SDK_VERSION}..."
  curl -fsSL -o "${dest}" "${url}"
  chmod +x "${dest}"
  OPERATOR_SDK_BIN="${dest}"
  info "operator-sdk ready (${OPERATOR_SDK_BIN})"
}

ols_bundle_image_from_git() {
  command -v python3 >/dev/null 2>&1 \
    || fail "python3 not found (needed to read related_images.json when --ols-bundle-image is omitted)"
  python3 - "${OLS_RELATED_IMAGES_URL}" <<'PY'
import json, sys, urllib.error, urllib.request
url = sys.argv[1]
try:
    with urllib.request.urlopen(url, timeout=30) as resp:
        data = json.load(resp)
except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as e:
    sys.stderr.write(f"error: fetch related_images.json: {e}\n")
    sys.exit(1)
for entry in data:
    if entry.get("name") == "lightspeed-operator-bundle" and entry.get("image"):
        print(entry["image"])
        sys.exit(0)
sys.stderr.write("error: lightspeed-operator-bundle not found in related_images.json\n")
sys.exit(1)
PY
}

fetch_related_images_bundle() {
  info "OLS bundle source: related_images.json (git)"
  echo "  Fetching related_images.json from ${OLS_RELATED_IMAGES_URL}"
  OLS_RELATED_BUNDLE_IMAGE="$(ols_bundle_image_from_git)"
  info "Resolved OLS bundle image: ${OLS_RELATED_BUNDLE_IMAGE}"
}

stop_for_custom_ols_bundle() {
  local resolved="${1:-}"
  local msg
  msg="Stopping. Re-run with your preferred OLS bundle image, for example:

  bash install-agentic.sh --ols-bundle-image=quay.io/<org>/lightspeed-operator-bundle:<tag>

  Related_images URL:
    ${OLS_RELATED_IMAGES_URL}"
  if [ -n "${resolved}" ]; then
    msg="${msg}
  Resolved related_images bundle (not used):
    ${resolved}"
  fi
  fail "${msg}"
}

ols_bundle_source_desc() {
  if [ -n "${OLS_BUNDLE_IMAGE}" ]; then
    echo "user-provided ${OLS_BUNDLE_IMAGE}"
  elif [ -n "${OLS_RELATED_BUNDLE_IMAGE}" ]; then
    echo "related_images.json ${OLS_RELATED_BUNDLE_IMAGE}"
  else
    echo "related_images.json ${OLS_RELATED_IMAGES_URL}"
  fi
}

install_or_update_ols() {
  # $1 = install | update
  # $2 = bundle image
  local mode="$1"
  local bundle_image="$2"
  ensure_operator_sdk
  info "OLS bundle image: ${bundle_image}"

  if [ "${mode}" = "update" ]; then
    echo "  Updating Lightspeed Operator via operator-sdk run bundle-upgrade..."
    if ! "${OPERATOR_SDK_BIN}" run bundle-upgrade --timeout="${BUNDLE_TIMEOUT}" --namespace "${NAMESPACE}" "${bundle_image}"; then
      fail "Lightspeed Operator bundle-upgrade failed.

  Refusing to fall back to a fresh 'run bundle' (that can leave a conflicting OLM install).
  Fix or reinstall OLS manually, then re-run this script.
  Bundle image: ${bundle_image}
  Bundle source: $(ols_bundle_source_desc)"
    fi
    info "Lightspeed Operator updated"
    OLS_ACTION="update"
    return 0
  fi

  echo "  Installing Lightspeed Operator via operator-sdk run bundle..."
  "${OPERATOR_SDK_BIN}" run bundle --timeout="${BUNDLE_TIMEOUT}" --namespace "${NAMESPACE}" "${bundle_image}" \
    || fail "Lightspeed Operator bundle install failed"
  info "Lightspeed Operator installed"
  OLS_ACTION="install"
}

prompt_ols_already_installed() {
  # $1 = related_images bundle. Prints leave | update | stop on stdout.
  local bundle_image="$1"
  require_tty_for_prompts
  cat <<EOF >&2

  Lightspeed Operator is already installed (OLSConfig CRD present).
  related_images.json resolved to:
    ${bundle_image}

  Choose:
    1) Leave current OLS as-is (continue to agentic)
    2) Update OLS to the related_images bundle above
    3) Stop and re-run with --ols-bundle-image=<your-image>

EOF
  local reply
  while true; do
    printf 'Choice [1/2/3]: ' >&2
    read -r reply
    case "${reply}" in
      1) echo leave; return 0 ;;
      2) echo update; return 0 ;;
      3) echo stop; return 0 ;;
      *) echo "  Unexpected input: '${reply}'. Please answer 1, 2, or 3." >&2 ;;
    esac
  done
}

write_olsconfig_template() {
  cat >"${OLS_CONFIG_FILE}" <<EOF
# Quickstart OLSConfig — edit provider/secret/model as needed, then apply from the installer prompt.
#
# Create the LLM credentials Secret in ${NAMESPACE} first, for example (OpenAI):
#   oc create secret generic llm-creds -n ${NAMESPACE} --from-literal=apitoken=sk-...
#
# credentialsSecretRef.name below must match that Secret.
apiVersion: ols.openshift.io/v1alpha1
kind: OLSConfig
metadata:
  name: cluster
spec:
  llm:
    providers:
      - name: openai
        type: openai
        credentialsSecretRef:
          name: llm-creds
        url: https://api.openai.com/v1
        models:
          - name: gpt-4o-mini
  ols:
    defaultProvider: openai
    defaultModel: gpt-4o-mini
EOF
}

seed_olsconfig_file() {
  if oc get olsconfig cluster >/dev/null 2>&1; then
    oc get olsconfig cluster -o yaml >"${OLS_CONFIG_FILE}"
    info "Exported existing OLSConfig cluster to ${OLS_CONFIG_FILE}"
  else
    write_olsconfig_template
    info "Wrote starter OLSConfig to ${OLS_CONFIG_FILE}"
  fi
}

dump_ols_diagnostics() {
  echo ""
  warn "OLS diagnostics (${NAMESPACE}):"
  echo "---- oc get olsconfig cluster -o yaml ----"
  oc get olsconfig cluster -o yaml 2>&1 || true
  echo "---- oc get pods -n ${NAMESPACE} ----"
  oc get pods -n "${NAMESPACE}" -o wide 2>&1 || true
  echo "---- recent events -n ${NAMESPACE} ----"
  oc get events -n "${NAMESPACE}" --sort-by=.metadata.creationTimestamp 2>&1 | tail -40 || true
  echo ""
}

wait_olsconfig_ready() {
  local elapsed=0
  local status=""
  echo "  Waiting up to ${OLS_CONFIG_TIMEOUT_SEC}s for OLSConfig status.overallStatus=Ready..."
  while (( elapsed < OLS_CONFIG_TIMEOUT_SEC )); do
    status="$(oc get olsconfig cluster -o jsonpath='{.status.overallStatus}' 2>/dev/null || true)"
    if [[ "${status}" == "Ready" ]]; then
      info "OLS is ready (overallStatus=Ready)"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    if (( elapsed % 60 == 0 )); then
      echo "  … still waiting (overallStatus=${status:-unset}, ${elapsed}s/${OLS_CONFIG_TIMEOUT_SEC}s)"
    fi
  done
  warn "OLS failed to become Ready within ${OLS_CONFIG_TIMEOUT_SEC}s (last overallStatus=${status:-unset})"
  dump_ols_diagnostics
  return 1
}

# Interactive loop: edit OLSConfig file, apply, wait for overallStatus=Ready.
configure_olsconfig_interactive() {
  require_tty_for_prompts
  seed_olsconfig_file

  while true; do
    cat <<EOF

  OLSConfig file: ${OLS_CONFIG_FILE}

  Before applying, you must:
    1. Create a Secret in ${NAMESPACE} with your LLM API key, for example (OpenAI):
         oc create secret generic llm-creds -n ${NAMESPACE} \\
           --from-literal=apitoken=sk-...
    2. Edit ${OLS_CONFIG_FILE} and set provider information to match:
         - spec.llm.providers[].type / name / url / models
         - spec.llm.providers[].credentialsSecretRef.name (must match the Secret)
         - spec.ols.defaultProvider and spec.ols.defaultModel

  Open the file in another terminal to accept the starter values or customize them.
  OLS will stay NotReady until the Secret and provider config are correct.

EOF
    local reply
    while true; do
      printf 'Apply OLSConfig from this file now? [y=apply / e=edit first / a=abort]: '
      read -r reply
      case "${reply}" in
        [yY]|[yY][eE][sS]) break ;;
        [eE]|[eE][dD][iI][tT])
          echo "  Create the LLM Secret and edit provider fields in ${OLS_CONFIG_FILE}, then choose y to apply."
          continue 2
          ;;
        [aA]|[aA][bB][oO][rR][tT])
          fail "Aborted OLSConfig setup. Re-run the installer when ready."
          ;;
        *) echo "  Unexpected input: '${reply}'. Please answer y, e, or a." >&2 ;;
      esac
    done

    echo "  Applying ${OLS_CONFIG_FILE}..."
    if ! oc apply -f "${OLS_CONFIG_FILE}"; then
      warn "oc apply failed — fix the file and try again"
      continue
    fi
    info "OLSConfig applied"

    if wait_olsconfig_ready; then
      return 0
    fi

    warn "OLS is not Ready. Fix the OLSConfig (and Secrets), then apply again."
    if ! prompt_yes_no "Return to the OLSConfig edit/apply loop?"; then
      fail "OLSConfig did not become Ready. Fix OLS, then re-run this script."
    fi
  done
}

# --- Step 1: Prerequisites ---------------------------------------------------

step "1/9" "Checking prerequisites..."

command -v oc >/dev/null 2>&1 || fail "oc CLI not found. Install it first."
info "oc CLI found"

if [ -z "${OLS_BUNDLE_IMAGE}" ]; then
  command -v python3 >/dev/null 2>&1 \
    || fail "python3 not found (needed to read related_images.json when --ols-bundle-image is omitted)"
  info "python3 found (related_images.json fallback)"
else
  info "OLS bundle image override: ${OLS_BUNDLE_IMAGE}"
fi

oc whoami >/dev/null 2>&1 || fail "Not logged into a cluster. Run: oc login ..."
info "Logged in as $(oc whoami)"

if ! oc auth can-i create clusterrolebindings >/dev/null 2>&1; then
  fail "Current user lacks cluster-admin privileges."
fi
info "cluster-admin privileges confirmed"

# --- Step 2: Namespace --------------------------------------------------------

step "2/9" "Ensuring namespace ${NAMESPACE}..."

if oc create namespace "${NAMESPACE}" 2>/dev/null; then
  info "Namespace created"
elif oc get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  info "Namespace already exists"
else
  fail "Failed to create namespace ${NAMESPACE}"
fi

# --- Step 3: Lightspeed Operator ----------------------------------------------

step "3/9" "Lightspeed Operator..."

CONFIGURE_OLSCONFIG=0

if [ -n "${OLS_BUNDLE_IMAGE}" ]; then
  # User supplied an image — always install or update; they know what they are doing.
  info "OLS bundle source: user-provided (--ols-bundle-image / OLS_BUNDLE_IMAGE)"
  if ols_crd_installed; then
    info "OLSConfig CRD present — updating Lightspeed Operator"
    install_or_update_ols update "${OLS_BUNDLE_IMAGE}"
  else
    info "OLSConfig CRD not found — installing Lightspeed Operator"
    install_or_update_ols install "${OLS_BUNDLE_IMAGE}"
  fi
  CONFIGURE_OLSCONFIG=1
else
  # related_images.json path — resolve concrete image before any install/update decision.
  fetch_related_images_bundle

  if ols_crd_installed; then
    info "OLSConfig CRD present"
    case "$(prompt_ols_already_installed "${OLS_RELATED_BUNDLE_IMAGE}")" in
      leave)
        warn "Leaving current Lightspeed Operator as-is"
        OLS_ACTION="left-as-is"
        ;;
      update)
        install_or_update_ols update "${OLS_RELATED_BUNDLE_IMAGE}"
        CONFIGURE_OLSCONFIG=1
        ;;
      stop)
        stop_for_custom_ols_bundle "${OLS_RELATED_BUNDLE_IMAGE}"
        ;;
    esac
  else
    info "OLSConfig CRD not found"
    cat <<EOF

  Lightspeed Operator is not installed.
  Installing from related_images.json would use:
    ${OLS_RELATED_BUNDLE_IMAGE}

  Declining stops this script — re-run with --ols-bundle-image=<your-image>
  if you want a different bundle.

EOF
    if prompt_yes_no "Install Lightspeed Operator using ${OLS_RELATED_BUNDLE_IMAGE}?"; then
      install_or_update_ols install "${OLS_RELATED_BUNDLE_IMAGE}"
      CONFIGURE_OLSCONFIG=1
    else
      stop_for_custom_ols_bundle "${OLS_RELATED_BUNDLE_IMAGE}"
    fi
  fi
fi

if [ "${CONFIGURE_OLSCONFIG}" = "1" ]; then
  configure_olsconfig_interactive
fi

# --- Step 4: Agentic Operator CRDs --------------------------------------------

step "4/9" "Installing Agentic Operator CRDs..."

for crd in "${CRD_FILES[@]}"; do
  if [ -n "${REPO_ROOT}" ]; then
    oc apply -f "${REPO_ROOT}/config/crd/bases/${crd}"
  else
    oc apply -f "${GITHUB_RAW}/config/crd/bases/${crd}"
  fi
done
if [ -n "${REPO_ROOT}" ]; then
  info "${#CRD_FILES[@]} CRDs applied (from local checkout)"
else
  info "${#CRD_FILES[@]} CRDs applied (from GitHub)"
fi

# --- Step 5: Agentic operator deployment --------------------------------------

step "5/9" "Deploying agentic operator to ${NAMESPACE} (sandbox-mode=${SANDBOX_MODE})..."

oc apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: lightspeed-agentic-operator
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agentic-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: lightspeed-agentic-operator
  namespace: ${NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lightspeed-agentic-operator
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: lightspeed-agentic-operator
  template:
    metadata:
      labels:
        app: lightspeed-agentic-operator
    spec:
      serviceAccountName: lightspeed-agentic-operator
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: manager
        image: ${OPERATOR_IMAGE}
$([ -n "${IMAGE_PULL_POLICY}" ] && echo "        imagePullPolicy: ${IMAGE_PULL_POLICY}")
        args:
        - "--namespace=${NAMESPACE}"
        - "--sandbox-mode=${SANDBOX_MODE}"
        - "--agentic-sandbox-image=${SANDBOX_IMAGE}"
$([ -n "${IMAGE_PULL_POLICY}" ] && echo '        - "--image-pull-policy='"${IMAGE_PULL_POLICY}"'"')
        ports:
        - name: metrics
          containerPort: 8080
          protocol: TCP
        - name: health
          containerPort: 8081
          protocol: TCP
        - name: webhook
          containerPort: 9443
          protocol: TCP
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 10m
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
            - ALL
        volumeMounts:
        - name: webhook-certs
          mountPath: /tmp/k8s-webhook-server/serving-certs
          readOnly: true
        env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
      volumes:
      - name: webhook-certs
        secret:
          secretName: agentic-operator-webhook-certs
          optional: true
EOF
info "Operator deployment applied"

# --- Step 5b: Agent read RBAC -------------------------------------------------

info "Binding read permissions to lightspeed-agent SA..."

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
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agent-monitoring-view
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-monitoring-view
subjects:
- kind: ServiceAccount
  name: lightspeed-agent
  namespace: ${NAMESPACE}
EOF
info "Agent read RBAC applied (cluster-reader + cluster-monitoring-view)"

# --- Step 6: ApprovalPolicy ---------------------------------------------------

step "6/9" "Creating ApprovalPolicy..."

oc apply -f - <<'EOF'
apiVersion: agentic.openshift.io/v1alpha1
kind: ApprovalPolicy
metadata:
  name: cluster
spec:
  maxAttempts: 3
  maxConcurrentRuns: 5
  stages:
  - name: Analysis
    approval: Automatic
  - name: Execution
    approval: Manual
  - name: Verification
    approval: Automatic
EOF
info "ApprovalPolicy created"

# --- Step 7: Webhook Service --------------------------------------------------

step "7/9" "Creating webhook Service..."

oc apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: agentic-operator-webhook-service
  namespace: ${NAMESPACE}
  annotations:
    service.beta.openshift.io/serving-cert-secret-name: agentic-operator-webhook-certs
spec:
  ports:
    - port: 443
      targetPort: 9443
      protocol: TCP
  selector:
    app: lightspeed-agentic-operator
EOF
info "Webhook Service created"

# --- Step 8: Wait for operator ------------------------------------------------

step "8/9" "Waiting for agentic operator to become ready..."

# Agentic blocks at startup on lightspeed-otel-collector-client (up to ~5m).
AGENTIC_ROLLOUT_TIMEOUT="${AGENTIC_ROLLOUT_TIMEOUT:-300s}"
if ! oc rollout status deployment/lightspeed-agentic-operator \
    -n "${NAMESPACE}" --timeout="${AGENTIC_ROLLOUT_TIMEOUT}" >/dev/null 2>&1; then
  fail "Agentic operator did not become ready within ${AGENTIC_ROLLOUT_TIMEOUT}.

  Check:
    oc logs deployment/lightspeed-agentic-operator -n ${NAMESPACE}
    oc get configmap lightspeed-otel-collector-client -n ${NAMESPACE}
    oc get olsconfig cluster -o yaml"
fi
info "Agentic operator is running"

# --- Step 9: Webhook Configuration (after operator is ready) ------------------

step "9/9" "Registering fail-closed MutatingWebhookConfiguration..."

oc apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: agentic-operator-mutating-webhook
  annotations:
    service.beta.openshift.io/inject-cabundle: "true"
webhooks:
  - name: agenticrunapproval-mutator.agentic.openshift.io
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: ${NAMESPACE}
    clientConfig:
      service:
        name: agentic-operator-webhook-service
        namespace: ${NAMESPACE}
        path: /mutate-agenticrunapproval
    rules:
      - operations: ["UPDATE"]
        apiGroups: ["agentic.openshift.io"]
        apiVersions: ["v1alpha1"]
        resources: ["agenticrunapprovals"]
    failurePolicy: Fail
    sideEffects: None
    admissionReviewVersions: ["v1"]
EOF
info "MutatingWebhookConfiguration registered"

# --- Step: Deploy console plugin (standalone) --------------------------------

if [ -n "${CONSOLE_IMAGE}" ]; then
  SCRIPT_DIR=""
  if [ -n "${BASH_SOURCE[0]:-}" ]; then
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  fi
  if [ -n "${SCRIPT_DIR}" ] && [ -f "${SCRIPT_DIR}/deploy-console.sh" ]; then
    NAMESPACE="${NAMESPACE}" CONSOLE_IMAGE="${CONSOLE_IMAGE}" bash "${SCRIPT_DIR}/deploy-console.sh"
  else
    curl -fsL "${GITHUB_RAW}/hack/quickstart/deploy-console.sh" \
      | NAMESPACE="${NAMESPACE}" CONSOLE_IMAGE="${CONSOLE_IMAGE}" bash
  fi
else
  echo "  CONSOLE_IMAGE is empty — skipping console deployment"
fi

# --- Done ---------------------------------------------------------------------

if [ -n "${REPO_ROOT}" ] && [ -d "${REPO_ROOT}/hack/quickstart/examples" ]; then
  EXAMPLES_BASE="${REPO_ROOT}/hack/quickstart/examples"
else
  EXAMPLES_BASE="${GITHUB_RAW}/hack/quickstart/examples"
fi

if [ "${OLS_ACTION}" = "left-as-is" ]; then
  OLS_SUMMARY_BUNDLE="(not changed)"
  OLS_SUMMARY_SUGGESTED="
  related_images suggested: ${OLS_RELATED_BUNDLE_IMAGE}"
else
  OLS_SUMMARY_BUNDLE="$(ols_bundle_source_desc)"
  OLS_SUMMARY_SUGGESTED=""
fi

cat <<DONE

════════════════════════════════════════════════════════════════
  Agentic OLS installed successfully!

  Namespace     : ${NAMESPACE}
  Sandbox mode  : ${SANDBOX_MODE}
  Operator image: ${OPERATOR_IMAGE}
  Sandbox image : ${SANDBOX_IMAGE}
  Console image : ${CONSOLE_IMAGE}
  OLS bundle    : ${OLS_SUMMARY_BUNDLE}${OLS_SUMMARY_SUGGESTED}
  OLS action    : ${OLS_ACTION}
  OLSConfig file: ${OLS_CONFIG_FILE}

  > Console works only on OpenShift 4.22+
════════════════════════════════════════════════════════════════

  OLS status (if Lightspeed Operator was installed/updated):
    oc get olsconfig cluster -o jsonpath='{.status.overallStatus}{"\\n"}'
    oc get olsconfig cluster -o yaml

  NEXT: Configure your agentic LLM provider. Pick one:

  ── Vertex AI / Claude ─────────────────────────────────────
  export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/service-account-key.json
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS="\$GOOGLE_APPLICATION_CREDENTIALS"
  # Edit vertex-anthropic.yaml — set your GCP project ID and region
  oc apply -f ${EXAMPLES_BASE}/vertex-anthropic.yaml

  ── Vertex AI / Gemini ─────────────────────────────────────
  export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/service-account-key.json
  oc create secret generic llm-creds-vertex -n ${NAMESPACE} \\
    --from-file=GOOGLE_APPLICATION_CREDENTIALS="\$GOOGLE_APPLICATION_CREDENTIALS"
  # Edit vertex-google.yaml — set your GCP project ID and region
  oc apply -f ${EXAMPLES_BASE}/vertex-google.yaml

  ── OpenAI ─────────────────────────────────────────────────
  oc create secret generic llm-creds-openai -n ${NAMESPACE} \\
    --from-literal=OPENAI_API_KEY=sk-...
  oc apply -f ${EXAMPLES_BASE}/openai.yaml

  ── Then submit an example run ────────────────────────

  # Investigate namespace workloads, remediate if issues found:
  oc apply -f ${EXAMPLES_BASE}/namespace-inventory.yaml

  # Deploy a test workload (analysis + execution):
  oc apply -f ${EXAMPLES_BASE}/deploy-test-workload.yaml

  # Watch until analysis completes (Analyzed=True):
  oc get agenticruns -n ${NAMESPACE} -w

  # Check the analysis result:
  oc get analysisresult -n ${NAMESPACE} -o json

  # Approve execution (option 0 = first option) via oc CLI or in console UI:
  oc patch agenticrunapproval namespace-inventory -n ${NAMESPACE} \\
    --type=json \\
    -p '[{"op":"add","path":"/spec/stages/-","value":{"type":"Execution","execution":{"option":0}}}]'

  # Watch execution progress:
  oc get agenticruns -n ${NAMESPACE} -w

  ── Enable audit tracing ───────────────────────────────────
  # Deploy Jaeger first (if you don't have a collector):
  bash <(curl -sL ${GITHUB_RAW}/hack/deploy-jaeger.sh)

  # Then enable audit tracing + OTEL export:
  OTEL_ENDPOINT=jaeger-otlp-grpc.observability.svc:4317 \\
    bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/setup-audit.sh)

  # Audit tracing only (stdout JSON, no remote collector):
  bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/setup-audit.sh)

  ── To uninstall ───────────────────────────────────────────
  bash <(curl -sL ${GITHUB_RAW}/hack/quickstart/uninstall.sh)

DONE
