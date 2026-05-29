# lightspeed-agentic-operator

Kubernetes controller for the agentic proposal workflow (`agentic.openshift.io/v1alpha1`): Proposal, ProposalApproval, Agent, LLMProvider, ApprovalPolicy, and related resources.

- **How to work (agents)**: [`agent.md`](agent.md)
- **Two Go modules (`go.mod` / `go.sum` at root and under `api/`)**: below; **directory map, phases, conventions**: [`CLAUDE.md`](CLAUDE.md)
- **Tests, manifests, Makefile, cluster workflow, `make api-lint`, CEL / `XValidation`**: this file

## Development

Run the manager against a cluster using the usual controller-runtime kubeconfig rules (`KUBECONFIG`, default kubeconfig path, or in-cluster as a pod). Auth plugins (OIDC, GCP, Azure, …) are registered via `k8s.io/client-go/plugin/pkg/client/auth`.

### Go modules (two `go.mod` trees)

This repo has **two Go modules**:

| Location | Module path | Role |
|----------|-------------|------|
| Repo root **`go.mod`** | `github.com/openshift/lightspeed-agentic-operator` | Controller, CLI, **`cmd/`**, **`controller/`**, etc. |
| **`api/go.mod`** | `github.com/openshift/lightspeed-agentic-operator/api` | CRD types and API helpers only—downstreams can **`require`** this module without pulling the full operator graph. |

The root module **`replace`s** `…/api` **`=> ./api`** for local builds. The **`Dockerfile`** copies **both** pairs of **`go.mod` / `go.sum`** before **`go mod download`**.

**When changing Kubernetes / controller-runtime versions**, keep the two **`go.mod`** files in sync (same PR / same intent) and run **`go mod tidy`** at the root and under **`api/`** as needed.

**`make test`** and **`make api-lint`** run tests and the kube API linter under **`api/`** with **`GOWORK=off`** so a repo-root **`go.work`** does not force the wrong module boundary.

### Agent Sandbox

Reconciling proposals uses [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) (`SandboxClaim`, `Sandbox`, `SandboxTemplate`). **`make run`** and **`make deploy`** both run **`install-agent-sandbox`** first: it only hits the network if the three Sandbox CRDs are missing (otherwise three cheap `kubectl get crd` calls). You can run **`make install-agent-sandbox`** alone to set **`AGENT_SANDBOX_VERSION`** without a full **`make run`**.

```bash
make install-agent-sandbox   # optional; also run automatically before make run / make deploy
# Override release: make install-agent-sandbox AGENT_SANDBOX_VERSION=v0.4.5
```

Upstream install details: [Agent Sandbox installation](https://agent-sandbox.sigs.k8s.io/docs/getting_started/install_prerequisites/). The Makefile target applies **`manifest.yaml`** and **`extensions.yaml`** for **`AGENT_SANDBOX_VERSION`** (default pinned in the Makefile). You still need a **`SandboxTemplate`** named like **`TEMPLATE_NAME`** (default `lightspeed-agent`) in **`OPERATOR_NAMESPACE`** — usually from Lightspeed / your platform, not from this target.

### CRDs and manifests

Do not edit **`config/crd/bases/`** or generated **`config/rbac/role.yaml`** by hand. After changing **`api/v1alpha1/`** types or kubebuilder markers, run **`make manifests`**.

### Makefile defaults

**`OPERATOR_NAMESPACE`** defaults to **`default`**: used for **`make run`** (`--namespace`), and for **`make deploy`** / **`make undeploy`** (Namespace, Deployment, ServiceAccount, RBAC subject namespace). Manifests under `config/` use **`__OPERATOR_NAMESPACE__`**; the Makefile substitutes **`$(OPERATOR_NAMESPACE)`** before `kustomize build`. Local **`make run`** uses metrics **`:18080`** and health **`:18081`** by default; override with **`METRICS_BIND_ADDRESS`** and **`HEALTH_PROBE_BIND_ADDRESS`** if needed.

### Common targets

```bash
make manifests # controller-gen → config/crd/bases + config/rbac/role.yaml
make test      # fmt + vet + go test (root + api module)
make install   # kubectl apply CRDs from config/crd only
make run       # install + install-agent-sandbox + vet + go run ./cmd/main.go
make uninstall # kubectl delete CRDs from config/crd (optional: ignore-not-found=true)

make deploy         # apply only; IMG must be pullable (CI / released image)
make deploy-local   # OpenShift (see Makefile)
make undeploy       # same layout | kubectl delete (optional: ignore-not-found=true)
make api-lint       # Kube API linter on api/ (golangci-lint custom + plugin; see below)
```

### Testing

**`make test`** runs **`fmt`**, **`vet`**, root-module tests, and **`cd api && GOWORK=off go test ./... -count=1`** (the API tree is a separate module; **`GOWORK=off`** avoids a repo-root **`go.work`** hijacking module choice). The **`test/e2e`** package is excluded from this target — it requires **`-tags=e2e`** and a running mock agent.

**`make test-e2e`** runs **`go test -tags=e2e ./test/e2e/...`** against a live cluster with the operator running. Prerequisites: **`make run TEMPLATE_NAME=lightspeed-agent-mock`** (or deployed operator with **`--template-name=lightspeed-agent-mock`**) and mock agent SandboxTemplate applied (**`kubectl apply -k test/agent/sandboxtemplate`**). See `test/e2e/` package doc for details.

For noisy debugging: **`go test ./controller/proposal/... -v`**, **`go test ./api/... -v`**, **`go test ./cli/... -v`**.

### API lint (Kube API linter)

**`make api-lint`** installs **`golangci-lint`** to **`bin/`** (version from **`.custom-gcl.yml`**), runs **`golangci-lint custom`** (builds **`bin/golangci-lint-kube-api-linter`**), then lints **`api/`** with **`GOWORK=off`** and **`.golangci-kal.yml`**.

### CEL validation (`XValidation`)

kubebuilder **`+kubebuilder:validation:XValidation`** markers become CEL rules in generated CRD YAML (**`make manifests`**).

- **`omitempty` fields**: they are absent from the stored object when unset. CEL must use **`has(field)`** before reading them, or validation fails with *no such key*. Example: **`has(old.denied) && old.denied`** instead of **`old.denied`** alone.
- **Marker placement**: struct-level **`XValidation`** can produce double-quoted strings in generated YAML. Put **`XValidation`** on the **field** in the parent struct, not on the nested type.

### `deploy`

**`kustomize build | kubectl apply`**. **`IMG`** is the image reference the Deployment uses; the image must already exist on the cluster.

### `deploy-local` (OpenShift)

**`make deploy-local`** — build, push to the cluster registry, and deploy the operator (**`oc`** and a push-capable registry required). Details live in the **`deploy-local`** recipe in the **`Makefile`**.

### `docker-build`

**`$(CONTAINER_TOOL) build -t $(IMG) .`** — see **`Dockerfile`**.
