# Build the manager binary
FROM registry.redhat.io/ubi9/go-toolset:9.8-1781757851 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests (api module first: replace in root go.mod).
COPY go.mod go.mod
COPY go.sum go.sum
COPY api/go.mod api/go.mod
COPY api/go.sum api/go.sum
# cache deps before building and copying remaining source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
COPY api/ api/
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY controller/ controller/

# this directory is checked by ecosystem-cert-preflight-checks task in Konflux
COPY LICENSE /licenses/

USER 0

# Build (TARGETOS / TARGETARCH are supplied by the container build client when cross-building).
RUN CGO_ENABLED=1 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -tags strictfipsruntime -o manager cmd/main.go
# Verify manager is built for at most x86-64-v2 (on amd64 only; check is a no-op elsewhere)
RUN go build -o check-isa-level ./cmd/check-isa-level && ./check-isa-level ./manager


FROM registry.redhat.io/ubi9/ubi-minimal:9.8-1781496742

WORKDIR /
COPY --from=builder /workspace/manager .
RUN mkdir /licenses
COPY LICENSE /licenses/.
LABEL name="openshift-lightspeed/lightspeed-agentic-rhel9-operator" \
      cpe="cpe:/a:redhat:openshift_lightspeed:1::el9" \
      com.redhat.component="openshift-lightspeed" \
      io.k8s.display-name="OpenShift Lightspeed Agentic Operator" \
      summary="OpenShift Lightspeed Agentic Operator runs the agentic proposal workflow controller." \
      description="OpenShift Lightspeed Agentic Operator manages Proposal, ProposalApproval, Agent, LLMProvider, and related resources for the agentic workflow." \
      io.k8s.description="OpenShift Lightspeed Agentic Operator is a component of OpenShift Lightspeed for agentic proposal workflows." \
      io.openshift.tags="openshift-lightspeed,agentic,ols"
USER 65532:65532

ENTRYPOINT ["/manager"]
