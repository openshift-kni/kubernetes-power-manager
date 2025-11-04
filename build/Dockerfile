ARG BASE_IMAGE=scratch

# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.23 AS builder
ARG TARGETOS \
    TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/
# Copy the go source
COPY build/manager/main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GO111MODULE=on GOOS=${TARGETOS:-linux} GOARCH=$TARGETARCH go build --mod=vendor -a -o manager main.go

# Final image
FROM ${BASE_IMAGE}
ARG MANIFEST=build/manifests/power-node-agent-ds.yaml
WORKDIR /
COPY LICENSE /licenses/LICENSE
COPY --from=builder /workspace/manager .
COPY ${MANIFEST} /power-manifests/
USER 10001

ENTRYPOINT ["/manager"]
