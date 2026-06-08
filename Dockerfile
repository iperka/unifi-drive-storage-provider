# --- build stage ---
FROM golang:1.26 AS build
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/unifi-drive-csi ./cmd/unifi-drive-csi

# --- runtime stage ---
# Need NFS userspace tools (mount.nfs) and ca-certificates. Base MUST be
# ubuntu:24.04 — verified 6/6 reliable for in-container NFSv3 mounts against a
# real UNAS (privileged + host networking). debian bookworm AND trixie both
# FAIL the in-container v3 handshake (mount.nfs "Protocol not supported"), so do
# not switch the base to debian without re-verifying.
#
# bindfs + fuse3 power the opt-in forceUid/forceGid uid-remap layer, which lets
# ownership-strict workloads (e.g. PostgreSQL) run on a root_squash UNAS export.
# tini is PID 1: bindfs daemonizes (double-forks) and its daemon reparents to
# PID 1 on unmount; without an init that reaps it, each mount/unmount leaks a
# <defunct> zombie. tini reaps them and forwards signals.
FROM ubuntu:24.04
RUN apt-get update \
    && apt-get install -y nfs-common ca-certificates bindfs fuse3 tini \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/unifi-drive-csi /usr/local/bin/unifi-drive-csi
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/unifi-drive-csi"]
