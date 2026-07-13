# HTTP mock for the in-sandbox agent contract (POST /v1/agent/run on :8080).
# Build: make -C test/agent docker-build
# Push:  make -C test/agent docker-push-local  OR  docker-build-quay + docker-push-quay QUAY_IMAGE=quay.io/...
# Cluster: base SandboxTemplate — test/agent/sandboxtemplate/ (kubectl apply -k; edit images for Quay).
#
# Builder image tracks go.mod; bump if toolchain rejects the tag.
FROM golang:1.25.7-bookworm AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum api/
COPY api/ api/
RUN go mod download

COPY controller/ controller/
COPY test/agent/ test/agent/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /mock-agent ./test/agent

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /mock-agent /mock-agent
COPY --from=builder /workspace/test/agent/skills/ /skills/
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/mock-agent"]
CMD ["-addr", ":8080"]
