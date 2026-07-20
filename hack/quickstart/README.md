# Quickstart

Deploy Agentic OLS onto an OpenShift cluster using pre-built Konflux images.
Installs or updates Lightspeed Operator via OLM (`operator-sdk run bundle` /
`bundle-upgrade`) when needed, then deploys the agentic operator. Pass
`--ols-bundle-image` to choose the OLS bundle; otherwise the script resolves
`lightspeed-operator-bundle` from git `related_images.json` and prompts
(decline / option 3 stops). No building or cloning of this repo is required.

## Prerequisites

- `oc` CLI on PATH
- Logged into the target OpenShift cluster
- cluster-admin privileges
- `python3` on PATH only when `--ols-bundle-image` is omitted (related_images fallback)

## Install

Download the script, then run it (required for interactive Lightspeed Operator prompts):

```bash
curl -fsSL -o install-agentic.sh \
  https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/install.sh
bash install-agentic.sh
```

Prefer an explicit OLS bundle (recommended when `related_images.json` may lag):

```bash
bash install-agentic.sh \
  --ols-bundle-image=quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-operator-bundle:main
```

Do **not** use `curl … | bash` — stdin is the script, so `y`/`n` prompts cannot work.

The script:

1. Detects whether Lightspeed Operator is installed (`OLSConfig` CRD).
2. Installs or updates OLS via `operator-sdk run bundle` / `bundle-upgrade`:
   - **`--ols-bundle-image` / `OLS_BUNDLE_IMAGE` set:** always install (if missing)
     or update (if present) with that image — no confirmation.
   - **No user image, OLS missing:** resolves
     [`lightspeed-operator-bundle` from `related_images.json`](https://github.com/openshift/lightspeed-operator/blob/main/related_images.json)
     (git ref `OLS_GIT_REF`, default `main`), shows the concrete image, and asks
     to install it. Declining **stops** the script (re-run with
     `--ols-bundle-image=…`).
   - **No user image, OLS present:** three-way choice — (1) leave OLS as-is and
     continue to agentic, (2) update to the related_images bundle, or (3) stop
     and re-run with `--ols-bundle-image=…`.
3. After a successful OLS install/update: writes/exports an `OLSConfig` file
   (`OLS_CONFIG_FILE`), lets you edit/apply in a loop, and waits until
   `status.overallStatus=Ready` (dumps diagnostics on failure).
4. Installs Agentic Operator CRDs, Deployment, ApprovalPolicy, and webhook.

After completion it prints instructions for configuring an LLM provider and
submitting a test run.

## Uninstall

Download the script, then run it (required for interactive Lightspeed Operator prompts):

```bash
curl -fsSL -o uninstall-agentic.sh \
  https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/uninstall.sh
bash uninstall-agentic.sh
```

Do **not** use `curl … | bash` — stdin is the script, so `y`/`n` prompts cannot work.

The script removes Agentic Operator resources and CRDs, and asks whether to also
uninstall Lightspeed Operator via `operator-sdk cleanup`. Declining leaves OLS
installed and keeps the namespace.

Skip the top-level confirmation with `QUICKSTART_FORCE=1`. For non-interactive
OLS decisions, set `REMOVE_OLS=1` (uninstall OLS) or `REMOVE_OLS=0` (keep OLS).

## CLI options

| Flag | Description |
|---|---|
| `--ols-bundle-image=IMAGE` | Lightspeed Operator OLM bundle for `operator-sdk run bundle` / `bundle-upgrade`. Required non-empty when set. If omitted, resolves `lightspeed-operator-bundle` from `related_images.json` and prompts (see Install flow above). |
| `-h`, `--help` | Show usage and exit |

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `NAMESPACE` | `openshift-lightspeed` | Target namespace |
| `OPERATOR_IMAGE` | Konflux `:main` | Agentic operator container image |
| `SANDBOX_IMAGE` | Konflux `:main` | Agent sandbox container image |
| `SANDBOX_MODE` | `bare-pod` | Sandbox mode (`bare-pod` or `sandbox-claim`) |
| `IMAGE_PULL_POLICY` | *(empty — Kubernetes default)* | Image pull policy for operator and sandbox pods (`Always`, `IfNotPresent`, `Never`) |
| `OLS_BUNDLE_IMAGE` | *(empty)* | Same as `--ols-bundle-image`; the flag overrides this env var |
| `OLS_GIT_REF` | `main` | Git ref for `openshift/lightspeed-operator` `related_images.json` (fallback only) |
| `OLS_RELATED_IMAGES_URL` | raw GitHub URL for that ref | Override the related_images.json URL (fallback only) |
| `BUNDLE_TIMEOUT` | `30m` | `operator-sdk run bundle` timeout |
| `OLS_CONFIG_FILE` | `$TMPDIR/olsconfig-quickstart.yaml` | Path for the editable OLSConfig manifest |
| `OLS_CONFIG_TIMEOUT_SEC` | `900` | Seconds to wait for `OLSConfig` `overallStatus=Ready` |
| `AGENTIC_ROLLOUT_TIMEOUT` | `300s` | `oc rollout status` timeout for the agentic Deployment |
| `REMOVE_OLS` | *(unset — prompt)* | Uninstall: `1` remove Lightspeed Operator, `0` keep it |
| `CLEANUP_TIMEOUT` | `5m` | Uninstall: `operator-sdk cleanup` timeout |

Example with overrides:

```bash
NAMESPACE=my-ns SANDBOX_MODE=sandbox-claim bash install-agentic.sh \
  --ols-bundle-image=quay.io/example/lightspeed-operator-bundle:main
```

For dev environments with floating tags like `:main`, force fresh pulls:

```bash
IMAGE_PULL_POLICY=Always bash install-agentic.sh
```

## LLM Provider Examples

The [`examples/`](examples/) directory contains LLMProvider + Agent templates:

| File | Provider |
|---|---|
| [`openai.yaml`](examples/openai.yaml) | OpenAI (direct API) |
| [`vertex-anthropic.yaml`](examples/vertex-anthropic.yaml) | Vertex AI with Claude |
| [`vertex-google.yaml`](examples/vertex-google.yaml) | Vertex AI with Gemini |

## CLI Plugin

Install the `oc-agentic` CLI plugin to manage agenticruns from the command line
([install instructions](../../README.md#install)).

Verify installation:

```bash
oc agentic version
```

## Example AgenticRun

[`deploy-test-workload.yaml`](examples/deploy-test-workload.yaml) submits a
run that analyzes the target namespace and deploys a test workload
(nginx Deployment + Service). Execution requires manual approval via
`AgenticRunApproval`.

### Using the CLI

Instead of applying YAML, you can create and manage agenticruns with the CLI:

```bash
# Create a run
oc agentic run create --request="Deploy a test nginx workload" --target-namespaces=default

# Watch it progress
oc agentic run list
oc agentic run get <name>

# Approve analysis, then execution
oc agentic run approve <name> --stage=analysis
oc agentic run approve <name> --stage=execution --option=0

# Stream sandbox logs
oc agentic run logs <name> -f

# Check system status
oc agentic status
```

See the [CLI reference](../../README.md#cli-plugin-oc-agentic) for all commands and flags.
