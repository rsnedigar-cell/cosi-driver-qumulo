# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# Multi-arch: buildx sets TARGETOS/TARGETARCH from --platform
# (e.g. docker build --platform linux/arm64 for EKS Graviton).
# The build stage always runs on the build host's architecture and
# cross-compiles.
#
# Two runtime images come out of this file:
#   --target cosi (default) : distroless/static, COSI driver only. The
#                             bucket provisioner never mounts anything and
#                             ships no shell, package manager, or utilities.
#   --target csi            : Alpine + nfs-utils/cifs-utils, CSI driver
#                             only. The node plugin execs mount.nfs/
#                             mount.cifs, which need a userland.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine3.24@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qumulo-cosi-driver ./cmd/qumulo-cosi-driver && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qumulo-csi-driver ./cmd/qumulo-csi-driver

# ---------------------------------------------------------------------------
# CSI runtime: needs mount.nfs/mount.cifs, so it carries an Alpine userland.
# Package versions are intentionally unpinned: Alpine stable serves only the
# latest revision per release, so exact pins break the build on every
# package revision bump. The digest-pinned base keeps builds reproducible.
FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS csi
ARG VERSION=dev
ARG VCS_REF=unknown
RUN apk add --no-cache ca-certificates cifs-utils nfs-utils && \
    command -v mount >/dev/null && \
    command -v umount >/dev/null && \
    command -v mount.nfs >/dev/null && \
    command -v mount.nfs4 >/dev/null && \
    command -v mount.cifs >/dev/null
LABEL org.opencontainers.image.title="Qumulo NFS/SMB CSI driver" \
      org.opencontainers.image.description="Kubernetes CSI driver for Qumulo NFS/SMB file storage" \
      org.opencontainers.image.source="https://github.com/rsnedigar-cell/cosi-driver-qumulo" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"
COPY --from=build /out/qumulo-csi-driver /qumulo-csi-driver
COPY LICENSE NOTICE /licenses/
# The controller deployment drops to 65532 via securityContext; the node
# DaemonSet explicitly escalates (privileged, runAsUser 0) because mount(2)
# requires it. The image default stays nonroot.
USER 65532:65532
ENTRYPOINT ["/qumulo-csi-driver"]

# ---------------------------------------------------------------------------
# COSI runtime (default build target): the live-proven bucket provisioner
# keeps its distroless static nonroot posture — no shell, no userland.
FROM gcr.io/distroless/static@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS cosi
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="Qumulo COSI driver" \
      org.opencontainers.image.description="Kubernetes COSI driver for Qumulo S3 buckets" \
      org.opencontainers.image.source="https://github.com/rsnedigar-cell/cosi-driver-qumulo" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"
COPY --from=build /out/qumulo-cosi-driver /qumulo-cosi-driver
COPY LICENSE NOTICE /licenses/
USER 65532:65532
ENTRYPOINT ["/qumulo-cosi-driver"]
