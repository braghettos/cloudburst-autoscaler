# Build the manager binary
FROM golang:1.24 AS builder
WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to rebuild deps
# every time we change source code
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY internal/ internal/

# Build (auto-detect target arch from build platform)
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o manager main.go

# Minimal runtime image. The binary is statically linked (CGO_ENABLED=0) so scratch works.
# Falls back to distroless if gcr.io is reachable: gcr.io/distroless/static:nonroot
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /workspace/manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
