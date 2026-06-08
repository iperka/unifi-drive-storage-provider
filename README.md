# UniFi Drive CSI Driver

A Kubernetes [CSI](https://kubernetes-csi.github.io/docs/) driver for the
Ubiquiti **UNAS** appliance running **UniFi Drive**. It provisions a UniFi
Drive share per `PersistentVolumeClaim`, enables NFS on it, locks the export to
your nodes, and mounts it into pods — with a built-in workaround for UNAS
`root_squash` so pods running as any UID can read and write.

- **Dynamic provisioning** — share created on claim, removed on delete.
- **`ReadWriteMany`** — NFS export, many pods on many nodes.
- **`root_squash` workaround** — works for non-root and ownership-strict pods.
- **One image, two modes** — controller (`Deployment`) + node plugin (`DaemonSet`).

> Provisioner: `drive.unifi.iperka.com` · Image: `ghcr.io/iperka/unifi-drive-csi`

## Requirements

- A UNAS appliance running UniFi Drive (verified on UNAS Pro, Drive 4.2.6 /
  firmware 5.1.15).
- A dedicated **local** UniFi admin account (not SSO/cloud).
- Kubernetes v1.20+ on **real Linux nodes** (need an NFSv3 client kernel —
  Docker Desktop's kernel is NFSv4-only and won't mount; see [notes](#notes)).

## Install

```bash
git clone https://github.com/iperka/unifi-drive-storage-provider
cd unifi-drive-storage-provider

# 1. Credentials — set host / username / password
cp deploy/secret.example.yaml deploy/secret.yaml
$EDITOR deploy/secret.yaml

# 2. StorageClass — set `server` (UNAS IP) and `allowedCIDRs` (your node IPs)
$EDITOR deploy/storageclass.yaml

# 3. Deploy (namespace, RBAC, CSIDriver, Secret, controller, node, StorageClass)
make deploy
```

That's it. Verify with the bundled smoke test:

```bash
kubectl apply -f deploy/example-pvc.yaml
kubectl get pvc unifi-drive-test     # → Bound
kubectl logs pod/unifi-drive-test    # → "hello"
kubectl delete -f deploy/example-pvc.yaml
```

## Usage

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: ["ReadWriteMany"]
  storageClassName: unifi-drive
  resources:
    requests:
      storage: 5Gi
```

The requested size becomes the share quota; the UNAS enforces it and
`NodeGetVolumeStats` reports usage against it (`kubelet_volume_stats_*` metrics).

## StorageClass parameters

Set under `parameters:` in `deploy/storageclass.yaml`.

| Parameter               | Required | Default           | Description |
|-------------------------|:--------:|-------------------|-------------|
| `server`                | ✅       | —                 | UNAS host the node mounts from. |
| `allowedCIDRs`          | ✅       | —                 | Comma-separated NFS client IPs (your node IPs). "Allow all" is `0.0.0.0,0.0.0.1`. |
| `exportBasePath`        |          | `/var/nfs/shared` | Export base; node mounts `<server>:<base>/<DriveName>`. |
| `mountOptions`          |          | —                 | Extra NFS options after defaults `nfsvers=3,nolock,noatime`. Keep v3. |
| `mountPermissions`      |          | `0777`            | Octal mode applied to the volume root after mount. |
| `uid` / `gid`           |          | —                 | `chown` the volume root after mount. |
| `forceUid` / `forceGid` |          | —                 | `bindfs` remap so the volume *appears* owned by this uid/gid. See [permissions](#permissions). |
| `nconnect`              |          | unset             | Parallel TCP connections to the UNAS (kernel ≥ 5.3). See [performance](#performance). |
| `readCache`            |          | `false`           | Read-side caching on the `bindfs` layer. Single-writer only. |

## Permissions

UNAS exports apply `root_squash`, so every write is squashed to a fixed anon
user and arbitrary-UID pods get `permission denied`. The driver works around it
in layers (cheapest first):

1. **`chmod`/`chown` after mount** — `mountPermissions` (default `0777`),
   `uid`/`gid`. Opens the volume to any pod UID. The primary fix.
2. **`fsGroup`** — `CSIDriver.fsGroupPolicy: File` + pod `securityContext.fsGroup`.
3. **`bindfs` uid/gid-remap** — `forceUid`/`forceGid`. A FUSE layer that
   *presents* the volume as owned by a fixed uid/gid.

Use layer 3 for **ownership-strict** workloads that check the data directory is
*owned* by their uid (e.g. PostgreSQL won't start otherwise) — `chmod` can't
satisfy that because the appliance squashes the owner. A ready-made StorageClass
for CloudNativePG (uid/gid `26`) ships at `deploy/storageclass-postgres.yaml`.
Trade-off: a userspace FUSE daemon per mount; opt-in.

> The UNAS exposes no anon-mapping API, so the squash owner can't be pinned —
> these three layers are the fix.

## Performance

Defaults are conservative. For throughput-sensitive workloads:

- **`nconnect=<n>`** — parallel TCP connections (kernel ≥ 5.3, NFSv3).
  ~+26% pgbench write TPS under concurrency. **Caveat:** it's per-`(client,
  server)`, so the first mount fixes the count for *all* shares on that UNAS —
  use the same value everywhere (or none).
- **`readCache: "true"`** — keeps the page cache across opens and lengthens
  metadata-cache timeouts. ~5.7× faster repeated reads, ~10× faster stat.
  **Single-writer/single-node only** (a remote writer's changes may read stale);
  affects `bindfs` volumes only.

`noatime` is applied to every volume by default.

## Capabilities

| Feature | Status |
|---|---|
| `CreateVolume` / `DeleteVolume`, NFS allowlisting | ✅ |
| NFSv3 mount + `root_squash` workaround | ✅ |
| `NodeGetVolumeStats` (quota-aware), `ReadWriteMany` | ✅ |
| Attach/detach | ➖ not required (`attachRequired: false`) |
| Volume expansion · snapshots · topology · SMB | ❌ not yet |

## Notes

- **Node kernel.** Real Linux nodes have the NFSv3 client; **Docker Desktop's
  LinuxKit kernel is NFSv4-only**, so the mount fails (`Protocol not supported`)
  there. The controller (provisioning) still works in kind — only the mount
  needs a real node. Don't change the node image base from `ubuntu:24.04`
  without re-testing the in-container NFSv3 handshake.
- **HA.** The controller runs 2 replicas with `csi-provisioner` leader election;
  only the leader mutates the appliance. Don't disable leader election or run
  multiple provisioners against one UNAS.
- **TLS.** `insecure` defaults to `true` for the self-signed cert; set `false`
  once a trusted cert is installed.

## Container images

Published to `ghcr.io/iperka/unifi-drive-csi`, signed with cosign (keyless OIDC).
Tags: `vX.Y.Z` / `X.Y` / `latest` (releases), `edge` / `main-<sha>` (main).

```bash
cosign verify ghcr.io/iperka/unifi-drive-csi:latest \
  --certificate-identity-regexp '^https://github\.com/iperka/unifi-drive-storage-provider' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## The UniFi Drive API is unofficial

Ubiquiti publishes no official UniFi Drive share API
([feature request](https://community.ui.com/questions/c2a86784-b71c-435d-9ae7-c4f04ab43c6e)).
This driver uses the **local, reverse-engineered** UniFi OS API, verified
against UniFi Drive 4.2.6 / firmware 5.1.15. It lives behind the `UnifiClient`
interface (`pkg/unifi/client.go`); an official API would only change
`pkg/unifi/http_client.go`.

Verify the endpoints on your firmware with the bundled probe:

```bash
UNIFI_HOST=https://<unas> UNIFI_USERNAME=<u> UNIFI_PASSWORD=<p> \
  go run ./cmd/unifi-drive-probe        # sweep known endpoints
```

## Development

```bash
make vet test     # static checks + unit tests (in-memory fake; no appliance)
make build        # → bin/unifi-drive-csi
make vulncheck    # govulncheck (matches CI)
```

| Path | Purpose |
|---|---|
| `cmd/unifi-drive-csi` | Driver binary (`--mode=controller\|node\|all`). |
| `cmd/unifi-drive-probe` | API discovery / verification tool. |
| `pkg/driver` | CSI Identity / Controller / Node servers. |
| `pkg/unifi` | `UnifiClient` interface, HTTP impl, fake. |
| `deploy` | Kubernetes manifests. |

CI runs `gofmt`, `go vet`, `go test -race`, `govulncheck`, and a Docker build on
every push/PR. Tag `vX.Y.Z` to cut a signed multi-arch release via GoReleaser.


