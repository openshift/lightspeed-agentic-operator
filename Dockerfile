# Build the manager binary
FROM registry.redhat.io/ubi9/go-toolset:9.8-1792425967 AS builder
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
COPY pkg/ pkg/

# this directory is checked by ecosystem-cert-preflight-checks task in Konflux
COPY LICENSE /licenses/

USER 0

# Build (TARGETOS / TARGETARCH are supplied by the container build client when cross-building).
# Build with Go's native FIPS 140-3 crypto provider (pure Go, no CGo/OpenSSL).
# GOFIPS140=certified selects the certified FIPS module; the binary defaults
# to fips140=on (see the //go:debug directive in cmd/fips.go), keeping
# FIPS-approved crypto always active (matching the previous
# strictfipsruntime behavior).
# GOAMD64 is only consulted when GOARCH=amd64; it is silently ignored for
# other target architectures (arm64 etc.), so multi-arch builds are unaffected.
RUN CGO_ENABLED=0 GOFIPS140=certified GOAMD64=v2 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager ./cmd/
# Verify manager is built for at most x86-64-v2 (on amd64 only; check is a no-op elsewhere)
RUN go build -o check-isa-level ./cmd/check-isa-level && ./check-isa-level ./manager


FROM registry.access.redhat.com/ubi9-micro:9.9-1794478307@sha256:dbd325b96bcd432dd42816d31412bdf1dc0fb973b79ed310344bc23c76c068db

WORKDIR /
COPY --from=builder /workspace/manager .
# Public CA bundle: spoke kubeconfigs may omit certificate-authority-data, in
# which case client-go verifies the spoke API server against system trust
# roots. Pure PEM data, parsed natively by Go - no OpenSSL in the runtime image.
# (In the ubi9-based builder, /etc/pki/tls/certs/ca-bundle.crt is a symlink to
# this file, so the real file is copied to the path Go's x509 checks.)
COPY --from=builder /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/pki/tls/certs/ca-bundle.crt
RUN mkdir /licenses
COPY LICENSE /licenses/.
LABEL name="openshift-lightspeed/lightspeed-agentic-rhel9-operator" \
      cpe="cpe:/a:redhat:openshift_lightspeed:2::el9" \
      com.redhat.component="openshift-lightspeed" \
      io.k8s.display-name="OpenShift Lightspeed Agentic Operator" \
      summary="OpenShift Lightspeed Agentic Operator runs the agentic run workflow controller." \
      description="OpenShift Lightspeed Agentic Operator manages AgenticRun, AgenticRunApproval, Agent, LLMProvider, and related resources for the agentic workflow." \
      io.k8s.description="OpenShift Lightspeed Agentic Operator is a component of OpenShift Lightspeed for agentic run workflows." \
      io.openshift.tags="openshift-lightspeed,agentic,ols"
USER 65532:65532

ENTRYPOINT ["/manager"]
