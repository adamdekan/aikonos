# broker/Dockerfile
# Multi-stage build — final image is distroless (no shell, no package manager)
# Build args allow pinning base image digests in CI for supply-chain hygiene.

# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.23-bookworm AS builder

WORKDIR /build

# Cache deps before copying source
COPY go.mod go.sum ./
RUN go mod download

# Proto toolchain — generate the Go stubs in-image so a clean checkout builds
# with no host `task proto:gen` prep (H6). protoc-gen-go is pinned to the
# google.golang.org/protobuf version in go.mod; protoc-gen-go-grpc to a
# compatible release. The runtime stage is distroless, so none of this reaches
# the final image. Versions below — keep in lockstep with .github/workflows/ci.yml
# (broker-test job's "Install protoc and Go plugins" step).
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev \
    && rm -rf /var/lib/apt/lists/*
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2 \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# Copy source (gen/ is .dockerignore'd so the in-image stubs are authoritative)
COPY . .

# Generate Go stubs at the module root (gen/go/<pkg>/v1/). --proto_path=. makes
# the `import "proto/...";` lines resolve; /usr/include (libprotobuf-dev)
# supplies the google/protobuf well-known types.
RUN protoc --proto_path=. --proto_path=/usr/include \
    --go_out=. --go_opt=module=github.com/adamdekan/aikonos \
    --go-grpc_out=. --go-grpc_opt=module=github.com/adamdekan/aikonos \
    proto/*.proto

# Build static binary — CGO disabled for distroless compatibility
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w -extldflags=-static \
      -X main.version=$(git describe --tags --always 2>/dev/null || echo dev) \
      -X main.commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
      -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /broker \
    ./broker/cmd/broker

# Verify binary is static (no `file` in the builder image; use ldd instead —
# a static binary makes ldd report "not a dynamic executable").
RUN ldd /broker 2>&1 | grep -q "not a dynamic executable" || \
    (echo "Binary is dynamically linked — distroless will fail" && exit 1)

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Copy only the binary
COPY --from=builder /broker /broker

# Non-root user (uid 65532 = nonroot in distroless)
USER 65532:65532

# Ports:
#   9090 — north-bound gRPC (frontend → broker)
#   9091 — south-bound gRPC (sandbox → broker)
#   9092 — health/metrics
EXPOSE 9090 9091 9092

ENTRYPOINT ["/broker"]
