# Mock agent image build and push.
# Build from repo root, push to Quay (or any registry).
#
# Usage:
#   make -C test/agent docker-build
#   make -C test/agent docker-push
_AGENT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
ROOT := $(abspath $(_AGENT_DIR)/../..)
CONTAINER_TOOL ?= $(shell which podman >/dev/null 2>&1 && echo podman || echo docker)
# Tag must match the image the e2e SandboxTemplate / pipeline pull
# (test/agent/sandboxtemplate/sandboxtemplate.yaml and
# .tekton/integration-tests). The batch model uses the "1"-suffixed tag.
IMAGE ?= quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent2

.PHONY: docker-build docker-push

docker-build: ## Build mock agent image.
	cd $(ROOT) && $(CONTAINER_TOOL) build -f test/agent/Dockerfile -t $(IMAGE) .

docker-push: ## Push $(IMAGE) to registry.
	$(CONTAINER_TOOL) push $(IMAGE)
