# Batch mock agent for e2e testing. Reads /input/ ConfigMap, creates Result CR
# via the Kubernetes API, patches status, and exits 0.
# Build: make -C test/agent docker-build
# Push:  make -C test/agent docker-push
#
# Builder image tracks go.mod; bump if toolchain rejects the tag.
FROM golang:1.26.0-bookworm AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum api/
COPY api/ api/
RUN go mod download

COPY controller/agenticrun/ controller/agenticrun/
COPY pkg/configuration/ pkg/configuration/
COPY test/agent/ test/agent/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /mock-agent ./test/agent

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /mock-agent /mock-agent
COPY --from=builder /workspace/test/agent/skills/ /skills/
USER 65532:65532
ENTRYPOINT ["/mock-agent"]
