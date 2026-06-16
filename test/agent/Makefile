# Mock agent image build and push.
# Build from repo root, push to Quay (or any registry).
#
# Usage:
#   make -C test/agent docker-build
#   make -C test/agent docker-push
_AGENT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
ROOT := $(abspath $(_AGENT_DIR)/../..)
CONTAINER_TOOL ?= $(shell which podman >/dev/null 2>&1 && echo podman || echo docker)
IMAGE ?= quay.io/openshift-lightspeed/ols-qe:lightspeed-mock-agent-metric

.PHONY: docker-build docker-push

docker-build: ## Build mock agent image.
	cd $(ROOT) && $(CONTAINER_TOOL) build -f test/agent/Dockerfile -t $(IMAGE) .

docker-push: ## Push $(IMAGE) to registry.
	$(CONTAINER_TOOL) push $(IMAGE)
