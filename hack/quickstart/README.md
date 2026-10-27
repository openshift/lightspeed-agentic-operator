# Quickstart

Deploy Agentic OLS onto an OpenShift cluster. All components are deployed
directly — no OLM bundle or `operator-sdk` required.

## Prerequisites

- A checkout of this repository (scripts reference each other via relative paths)
- `oc` CLI on PATH, logged into an OpenShift 4.22+ cluster
- cluster-admin privileges
- `python3` and `openssl` on PATH

## Install

Run the all-in-one installer:

```bash
bash hack/quickstart/install.sh
```

Override any image:

```bash
bash hack/quickstart/install.sh \
  --operator-image=quay.io/.../lightspeed-agentic-operator:on-pr-<sha> \
  --sandbox-image=quay.io/.../lightspeed-agentic-sandbox:main
```

Deploy with Postgres backend for OTEL audit logs:

```bash
bash hack/quickstart/install.sh --postgres
```

Export traces to a Jaeger OTLP gRPC endpoint:

```bash
bash hack/quickstart/install.sh \
  --traces=jaeger-otlp-grpc.observability.svc.cluster.local:4317
```

The `--traces` endpoint uses insecure OTLP gRPC. This matches the Jaeger
instance deployed by `hack/deploy-jaeger.sh`.

Or deploy components individually (run from the repo root — scripts
call each other via `hack/quickstart/`):

```bash
bash hack/quickstart/deploy-otel.sh
bash hack/quickstart/deploy-alerts-adapter.sh
bash hack/quickstart/deploy-configmap.sh
bash hack/quickstart/deploy-operator.sh
bash hack/quickstart/deploy-console.sh
```

## Uninstall

```bash
bash hack/quickstart/uninstall.sh
```

Skip the confirmation prompt:

```bash
bash hack/quickstart/uninstall.sh --force
```

Normal resumption uses `oc agentic resume`; while cleanup or affected-run stop
protection is unconfirmed, the API server rejects resume and deletion of the
suspended config. The initial cleanup deadline is ten seconds; unresolved work
is reported as terminal error completion, not permission to resume.
`uninstall.sh` is an explicit teardown path: it removes the safety binding
before its policy and both before CRD teardown, rather than deleting the config
while the safety gate remains installed.

Or remove components individually (run from the repo root):

```bash
bash hack/quickstart/undeploy-console.sh
bash hack/quickstart/undeploy-operator.sh
bash hack/quickstart/undeploy-configmap.sh
bash hack/quickstart/undeploy-alerts-adapter.sh
bash hack/quickstart/undeploy-otel.sh
```

## CLI flags

### install.sh

| Flag | Description |
|------|-------------|
| `--operator-image=IMAGE` | Agentic operator image (default: Konflux `:main`) |
| `--sandbox-image=IMAGE` | Sandbox image for the ConfigMap PodSpec (default: Konflux `:main`) |
| `--console-image=IMAGE` | Console plugin image (default: Konflux `:main`) |
| `--alerts-adapter-image=IMAGE` | Alerts adapter image (default: Konflux `:main`) |
| `--otel-image=IMAGE` | OTEL collector image (default: Konflux `:main`) |
| `--postgres` | Deploy Postgres backend for OTEL audit logs |
| `--traces=ENDPOINT` | Export traces to an insecure OTLP gRPC endpoint |

### Individual scripts

| Script | Flag | Default |
|--------|------|---------|
| `deploy-operator.sh` | `--image=IMAGE` | Konflux `:main` |
| `deploy-console.sh` | `--image=IMAGE` | Konflux `:main` |
| `deploy-alerts-adapter.sh` | `--image=IMAGE` | Konflux `:main` |
| `deploy-otel.sh` | `--image=IMAGE` | Konflux `:main` |
| `deploy-otel.sh` | `--postgres` | Deploy Postgres backend for audit logs |
| `deploy-otel.sh` | `--traces=ENDPOINT` | Export traces to an insecure OTLP gRPC endpoint |
| `deploy-configmap.sh` | `--sandbox-image=IMAGE` | Konflux `:main` |

All images use the Konflux floating `:main` tag by default.

## Image resolution

All quickstart images are built by the Konflux push pipeline under the same
tenant (`crt-nshift-lightspeed-tenant`) and have a floating `:main` tag.

To use a specific PR build of the agentic operator:

```bash
bash hack/quickstart/deploy-operator.sh \
  --image=quay.io/redhat-user-workloads/crt-nshift-lightspeed-tenant/lightspeed-agentic-operator:on-pr-<commit-sha>
```

## Components

| Component | Script | What it deploys |
|-----------|--------|-----------------|
| OTEL collector | `deploy-otel.sh` | Collector accepting OTLP with TLS via service-ca; optionally `--postgres` for audit log storage |
| Postgres | `deploy-postgres.sh` | Minimal Postgres instance (emptyDir), auto-deployed by `deploy-otel.sh --postgres` |
| Alerts adapter | `deploy-alerts-adapter.sh` | Polls Alertmanager, creates AgenticRuns, RBAC for agenticruns + alertmanager |
| Configuration | `deploy-configmap.sh` | `lightspeed-agentic-configuration` ConfigMap + OTEL CA secret |
| Operator | `deploy-operator.sh` | CRDs, SA, cluster-admin, Deployment, agent RBAC, ApprovalPolicy, webhook, and static admission policies |
| Console | `deploy-console.sh` | Console plugin with nginx + TLS, registers with OpenShift Console |

## LLM Provider Examples

The [`examples/`](examples/) directory contains LLMProvider + Agent templates:

| File | Provider |
|------|----------|
| [`openai.yaml`](examples/openai.yaml) | OpenAI (direct API) |
| [`vertex-anthropic.yaml`](examples/vertex-anthropic.yaml) | Vertex AI with Claude |
| [`vertex-google.yaml`](examples/vertex-google.yaml) | Vertex AI with Gemini |

## CLI Plugin

Install the `oc-agentic` CLI plugin to manage agenticruns from the command line
([install instructions](../../README.md#install)).

```bash
oc agentic version
oc agentic run create --request="Deploy a test nginx workload in the default namespace"
oc agentic run list
```
