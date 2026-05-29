# =============================================================================
# lightspeed-agentic-operator — Makefile
# =============================================================================
# Development and build targets in the same spirit as lightspeed-operator
# (help categories, fmt/vet/test/build/run).
# =============================================================================

# Image / artifact version tag (used by docker-build IMG=...).
VERSION ?= latest

# Image URL for deploy / docker-build (same pattern as lightspeed-operator Makefile).
IMG ?= lightspeed-agentic-operator:$(VERSION)
export IMG

# -----------------------------------------------------------------------------
# Tool paths — where `go install` puts binaries when GOBIN is unset
# -----------------------------------------------------------------------------
ifeq (,$(shell go env GOBIN))
GOBIN := $(shell go env GOPATH)/bin
else
GOBIN := $(shell go env GOBIN)
endif

# Container engine for docker-build / docker-push (prefers podman if on PATH).
CONTAINER_TOOL ?= "$(shell which podman >/dev/null 2>&1 && echo podman || echo docker)"

# Local tool binaries (kustomize, controller-gen).
LOCALBIN ?= $(shell pwd)/bin

# Bash with strict errors (matches lightspeed-operator Makefile).
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Default goal when you run plain `make`.
.PHONY: all
all: build

##@ General
# Targets below this marker appear under "General" in `make help`.

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development
# Go fmt/vet/test, build the manager, and `make run` for local controller testing.

# Namespace for the manager (sandbox templates/claims) and for in-cluster
# Deployment/RBAC from make deploy / make undeploy.
OPERATOR_NAMESPACE ?= default

# Name of the base SandboxTemplate the reconciler resolves per step
# (flag --template-name in cmd/main.go).
TEMPLATE_NAME ?= lightspeed-agent

# Local `make run` defaults avoid clashing with other processes on :8080/:8081.
METRICS_BIND_ADDRESS ?= :18080
HEALTH_PROBE_BIND_ADDRESS ?= :18081

CONTROLLER_GEN_VERSION ?= v0.19.0
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

# Must match version in .custom-gcl.yml (golangci-lint custom).
GOLANGCI_LINT_VERSION ?= v2.9.0
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_KAL ?= $(LOCALBIN)/golangci-lint-kube-api-linter

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run unit tests (main + api + cli modules).
	# Root module: controller, cli, etc. (exclude test/e2e — needs -tags=e2e and a running mock; see test-e2e).
	go test $$(go list ./... | grep -vF 'github.com/openshift/lightspeed-agentic-operator/test/e2e') -count=1
	# API module is separate go.mod; GOWORK=off avoids picking up a repo root go.work.
	cd api && GOWORK=off go test ./... -count=1

##@ Testing

.PHONY: test-e2e
test-e2e: ## Run e2e tests against a live cluster (operator must be running). See test/e2e/ for prereqs.
	go test -tags=e2e ./test/e2e/... -count=1 -v -timeout 30m

.PHONY: api-lint
api-lint: golangci-lint ## Kube API linter on api/ (installs golangci-lint to bin/; see README.md).
	$(GOLANGCI_LINT) custom
	cd api && GOWORK=off $(GOLANGCI_LINT_KAL) run --config ../.golangci-kal.yml ./...

.PHONY: build
build: fmt vet ## Build manager binary to bin/manager.
	go build -o bin/manager ./cmd

.PHONY: manifests
manifests: controller-gen ## Regenerate CRD YAML and RBAC ClusterRole (do not edit role.yaml by hand).
	$(CONTROLLER_GEN) rbac:roleName=agentic-operator-manager-role crd paths=./api/v1alpha1/... paths=./controller/proposal/... output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

##@ Deployment
# Same pattern as lightspeed-operator: `kustomize build config/crd | kubectl apply`
# (see lightspeed-operator Makefile install / uninstall).

ifndef ignore-not-found
  ignore-not-found = false
endif

# kubernetes-sigs/agent-sandbox release used by install-agent-sandbox (manifest + extensions).
AGENT_SANDBOX_VERSION ?= v0.4.5
AGENT_SANDBOX_RELEASE_BASE ?= https://github.com/kubernetes-sigs/agent-sandbox/releases/download

# Image name under the current oc project for deploy-local (OpenShift integrated registry only).
LOCAL_IMAGE_NAME ?= lightspeed-agentic-operator

# When the cluster has no public image registry hostname (oc registry info --public fails),
# set this to a host your machine can push to, or export it in the shell.
DEPLOY_LOCAL_REGISTRY ?=

# When 1, deploy-local will not patch imageregistry/cluster (spec.defaultRoute) to expose the registry.
DEPLOY_LOCAL_SKIP_REGISTRY_ROUTE_PATCH ?=

.PHONY: install-agent-sandbox
install-agent-sandbox: ## Install Agent Sandbox (core+extensions) if Sandbox CRDs are missing. See README.md.
	@set -e; \
	ext_crd=sandboxclaims.extensions.agents.x-k8s.io; \
	core_crd=sandboxes.agents.x-k8s.io; \
	st_crd=sandboxtemplates.extensions.agents.x-k8s.io; \
	if $(KUBECTL) get crd "$$ext_crd" >/dev/null 2>&1 && $(KUBECTL) get crd "$$core_crd" >/dev/null 2>&1 && $(KUBECTL) get crd "$$st_crd" >/dev/null 2>&1; then \
		echo "Agent Sandbox already installed ($$ext_crd, $$core_crd, $$st_crd)."; \
	else \
		v="$(AGENT_SANDBOX_VERSION)"; \
		base="$(AGENT_SANDBOX_RELEASE_BASE)"; \
		echo "Applying Agent Sandbox $$v ($$base/$$v/{manifest,extensions}.yaml) ..."; \
		$(KUBECTL) apply -f "$$base/$$v/manifest.yaml"; \
		$(KUBECTL) apply -f "$$base/$$v/extensions.yaml"; \
	fi

.PHONY: install
install: kustomize ## Install CRDs from config/crd into the cluster (current kubeconfig).
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: kustomize ## Remove CRDs from config/crd. Use ignore-not-found=true if resources are already gone.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests install-agent-sandbox kustomize ## Pre-built image: apply CRDs+RBAC+Deployment (set IMG). For OpenShift dev build+push+integrated registry use deploy-local.
	@tmpdir=$$(mktemp -d); \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	cp -a config "$$tmpdir/"; \
	for f in "$$tmpdir/config/manager/manager.yaml" "$$tmpdir/config/rbac/role_binding.yaml" "$$tmpdir/config/rbac/service_account.yaml" "$$tmpdir/config/default/kustomization.yaml"; do \
		sed -e 's|__OPERATOR_NAMESPACE__|$(OPERATOR_NAMESPACE)|g' "$$f" > "$$f.tmp" && mv "$$f.tmp" "$$f"; \
	done; \
	cd "$$tmpdir/config/manager" && $(KUSTOMIZE) edit set image controller=$(IMG); \
	$(KUSTOMIZE) build "$$tmpdir/config/default" | $(KUBECTL) apply -f -

.PHONY: deploy-local
deploy-local: ## OpenShift dev: build + push to integrated registry + deploy. Patches default registry route if needed; ignores IMG. See README.md.
	@set -e; \
	if ! command -v oc >/dev/null 2>&1; then \
		echo "error: deploy-local requires oc"; exit 1; \
	fi; \
	resolve_reg() { \
		local r; \
		r="$(strip $(DEPLOY_LOCAL_REGISTRY))"; \
		if [ -z "$$r" ] && [ -n "$${DEPLOY_LOCAL_REGISTRY:-}" ]; then r="$${DEPLOY_LOCAL_REGISTRY}"; fi; \
		if [ -z "$$r" ]; then r=$$(oc registry info --public 2>/dev/null) || true; fi; \
		if [ -z "$$r" ]; then \
			r=$$(oc get routes.route.openshift.io -n openshift-image-registry -o jsonpath='{range .items[*]}{.spec.host}{"\n"}{end}' 2>/dev/null | head -1) || true; \
		fi; \
		if [ -z "$$r" ]; then \
			r=$$(oc get configs.imageregistry.operator.openshift.io cluster -o jsonpath='{.status.routes[0].hostname}' 2>/dev/null) || true; \
		fi; \
		if [ -z "$$r" ]; then \
			r=$$(oc get configs.imageregistry.operator.openshift.io cluster -o jsonpath='{.spec.routes[0].hostname}' 2>/dev/null) || true; \
		fi; \
		printf '%s' "$$r"; \
	}; \
	reg=$$(resolve_reg); \
	if [ -z "$$reg" ] && [ -z "$(strip $(DEPLOY_LOCAL_REGISTRY))" ] && [ -z "$${DEPLOY_LOCAL_REGISTRY:-}" ]; then \
		if [ "$(strip $(DEPLOY_LOCAL_SKIP_REGISTRY_ROUTE_PATCH))" != "1" ]; then \
			echo "deploy-local: no registry hostname yet; patching configs.imageregistry.operator.openshift.io/cluster (spec.defaultRoute=true) ..."; \
			if oc patch configs.imageregistry.operator.openshift.io/cluster --type=merge -p '{"spec":{"defaultRoute":true}}'; then \
				_w=0; \
				while [ $$_w -lt 60 ]; do \
					reg=$$(resolve_reg); \
					if [ -n "$$reg" ]; then break; fi; \
					if [ $$_w -eq 0 ]; then echo "deploy-local: waiting for registry hostname (up to ~120s) ..."; fi; \
					sleep 2; \
					_w=$$((_w+1)); \
				done; \
			else \
				echo "error: deploy-local: oc patch imageregistry/cluster failed (need cluster-admin, or set DEPLOY_LOCAL_REGISTRY=host)."; exit 1; \
			fi; \
		fi; \
	fi; \
	ns="$(OPERATOR_NAMESPACE)"; \
	if [ -z "$$ns" ]; then \
		echo "error: deploy-local: OPERATOR_NAMESPACE is empty"; exit 1; \
	fi; \
	if ! oc get namespace "$$ns" >/dev/null 2>&1; then \
		echo "error: deploy-local: namespace $$ns not found (create it or set OPERATOR_NAMESPACE)"; exit 1; \
	fi; \
	if [ -z "$$reg" ]; then \
		echo "error: deploy-local: no pushable image registry hostname after discovery"; \
		echo "  (and optional defaultRoute patch / wait). Internal only:"; \
		echo "  $$(oc registry info 2>/dev/null | tail -1)"; \
		echo "  Override: make deploy-local DEPLOY_LOCAL_REGISTRY=registry.apps...."; \
		echo "  Or skip cluster patch: DEPLOY_LOCAL_SKIP_REGISTRY_ROUTE_PATCH=1 and expose the registry out-of-band."; \
		exit 1; \
	fi; \
	push_img="$$reg/$$ns/$(LOCAL_IMAGE_NAME):$(VERSION)"; \
	echo "deploy-local: $$push_img"; \
	echo "deploy-local: $(CONTAINER_TOOL) login -> $$reg"; \
	oc whoami -t | $(CONTAINER_TOOL) login -u "$$(oc whoami)" --password-stdin "$$reg"; \
	$(MAKE) docker-build IMG=$$push_img; \
	$(MAKE) docker-push IMG=$$push_img; \
	$(MAKE) deploy IMG=$$push_img; \
	echo "deploy-local: integrated registry requires pull credentials — linking to serviceaccount/controller-manager"; \
	oc -n "$(OPERATOR_NAMESPACE)" create secret docker-registry integrated-registry-pull-secret \
		--docker-server="$$reg" \
		--docker-username="$$(oc whoami)" \
		--docker-password="$$(oc whoami -t)" \
		--dry-run=client -o yaml | oc apply -f -; \
	oc -n "$(OPERATOR_NAMESPACE)" secrets link serviceaccount/controller-manager integrated-registry-pull-secret --for=pull; \
	oc -n "$(OPERATOR_NAMESPACE)" rollout restart deployment/controller-manager

.PHONY: undeploy
undeploy: kustomize ## Remove in-cluster operator (CRDs + RBAC + Deployment). Use ignore-not-found=true if resources are already gone.
	@tmpdir=$$(mktemp -d); \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	cp -a config "$$tmpdir/"; \
	for f in "$$tmpdir/config/manager/manager.yaml" "$$tmpdir/config/rbac/role_binding.yaml" "$$tmpdir/config/rbac/service_account.yaml" "$$tmpdir/config/default/kustomization.yaml"; do \
		sed -e 's|__OPERATOR_NAMESPACE__|$(OPERATOR_NAMESPACE)|g' "$$f" > "$$f.tmp" && mv "$$f.tmp" "$$f"; \
	done; \
	cd "$$tmpdir/config/manager" && $(KUSTOMIZE) edit set image controller=$(IMG); \
	$(KUSTOMIZE) build "$$tmpdir/config/default" | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f - || true

.PHONY: run
run: install install-agent-sandbox vet ## install + install-agent-sandbox (no-op if CRDs exist) + vet, then run the controller locally.
	# Same kubeconfig discovery as other controller-runtime apps (see README.md).
	go run ./cmd/main.go \
		--namespace=$(OPERATOR_NAMESPACE) \
		--template-name=$(TEMPLATE_NAME) \
		--metrics-bind-address=$(METRICS_BIND_ADDRESS) \
		--health-probe-bind-address=$(HEALTH_PROBE_BIND_ADDRESS)

##@ Build Dependencies
# Binaries under ./bin (same layout as lightspeed-operator).

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

KUSTOMIZE_VERSION ?= v5.3.0
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint to $(LOCALBIN) if missing or wrong version.

$(GOLANGCI_LINT): $(LOCALBIN)
	@if test -x $(GOLANGCI_LINT) && ! $(GOLANGCI_LINT) --version 2>&1 | grep -q $(GOLANGCI_LINT_VERSION); then \
		echo "$(GOLANGCI_LINT) version is not expected $(GOLANGCI_LINT_VERSION). Removing it before installing."; \
		rm -rf $(GOLANGCI_LINT); \
	fi
	test -s $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen to $(LOCALBIN) if missing or wrong version.
$(CONTROLLER_GEN): $(LOCALBIN)
	@if test -x $(CONTROLLER_GEN) && ! $(CONTROLLER_GEN) --version 2>&1 | grep -q $(CONTROLLER_GEN_VERSION); then \
		echo "$(CONTROLLER_GEN) version is not expected $(CONTROLLER_GEN_VERSION). Removing it before installing."; \
		rm -rf $(CONTROLLER_GEN); \
	fi
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize to $(LOCALBIN) if missing or wrong version.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q $(KUSTOMIZE_VERSION); then \
		echo "$(LOCALBIN)/kustomize version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -rf $(LOCALBIN)/kustomize; \
	fi
	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) go install -mod=mod sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

##@ Build
# OCI image build/push — requires a Dockerfile at repo root (not checked in yet).

.PHONY: docker-build
docker-build: ## Build container image (requires Dockerfile in repo root).
	@test -f Dockerfile || (echo "error: no Dockerfile in repo root; add one or build with: go build -o bin/manager ./cmd" >&2; exit 1)
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push $(IMG); registry host comes from IMG only (not from kubeconfig—use oc registry login + full registry URL for OpenShift).
	$(CONTAINER_TOOL) push $(IMG)
