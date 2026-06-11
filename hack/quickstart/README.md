# Quickstart

Deploy Agentic OLS onto an OpenShift cluster using pre-built Konflux images.
No building, no cloning required.

## Prerequisites

- `oc` CLI on PATH
- Logged into the target OpenShift cluster
- cluster-admin privileges

## Install

```bash
curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/install.sh | bash
```

The script installs CRDs, deploys the operator, and creates an ApprovalPolicy.
After completion it prints instructions for configuring an LLM provider and
submitting a test proposal.

## Uninstall

```bash
curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/uninstall.sh | bash
```

Skip the confirmation prompt with `QUICKSTART_FORCE=1`.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `NAMESPACE` | `openshift-lightspeed` | Target namespace |
| `OPERATOR_IMAGE` | Konflux `:main` | Operator container image |
| `SANDBOX_IMAGE` | Konflux `:main` | Agent sandbox container image |
| `SANDBOX_MODE` | `bare-pod` | Sandbox mode (`bare-pod` or `sandbox-claim`) |

Example with overrides:

```bash
NAMESPACE=my-ns SANDBOX_MODE=sandbox-claim \
  curl -sL https://raw.githubusercontent.com/openshift/lightspeed-agentic-operator/main/hack/quickstart/install.sh | bash
```

## LLM Provider Examples

The [`examples/`](examples/) directory contains LLMProvider + Agent templates:

| File | Provider |
|---|---|
| [`openai.yaml`](examples/openai.yaml) | OpenAI (direct API) |
| [`vertex-anthropic.yaml`](examples/vertex-anthropic.yaml) | Vertex AI with Claude |
| [`vertex-google.yaml`](examples/vertex-google.yaml) | Vertex AI with Gemini |

## Example Proposal

[`namespace-inventory.yaml`](examples/namespace-inventory.yaml) submits a
proposal that analyzes workloads in the target namespace and proposes
remediation with RBAC for an execution step. Execution requires manual
approval via `ProposalApproval`.
