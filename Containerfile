# Build the manager and console-proxy binaries
FROM registry.access.redhat.com/hi/go:1.26-builder AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests (including the nested api module)
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum api/
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY . ./

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o console-proxy cmd/console-proxy/main.go

# Use Red Hat hardened core-runtime as minimal base image to package the binaries
FROM registry.access.redhat.com/hi/core-runtime:latest
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/console-proxy .
USER 65532:65532

ENTRYPOINT ["/manager"]
