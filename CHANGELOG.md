# Changelog

## 0.2.0 - 2026-08-17

First public release. Adds the `file.qumulo.com` NFS/SMB CSI driver beside
the `s3.qumulo.com` COSI driver and hardens both.

### Added

- **NFS/SMB CSI driver** (`file.qumulo.com`): dynamic PersistentVolumes on
  Qumulo file storage. Controller and privileged Linux node plug-in,
  digest-pinned provisioner/resizer/registrar/liveness sidecars, NFS 3 and
  4.1 mounts (v3 with `nolock`), SMB 3.1.1 with optional required
  encryption and access-based enumeration, directory-quota capacity with
  online expansion, and full cleanup on delete (`deleteData`).
- **Two runtime images**: the COSI driver ships on
  `gcr.io/distroless/static:nonroot` (no shell or userland); the CSI driver
  ships as `<image>:<tag>-csi` on Alpine with the NFS/CIFS mount helpers it
  execs. Both are multi-arch (amd64/arm64) with SBOM and provenance.
- **Brownfield import** (`existingBucketName`): claims can bind to an
  existing bucket. Imported handles are unmanaged — deletion unregisters
  but always retains data, and `purgeOnDelete` downgrades to retention.
- **Live integration suites**: `TestLiveDriverFlow` (full COSI lifecycle)
  and `TestLiveFileProtocolControlPlane` (NFS/SMB control plane) run
  against a real cluster; see docs/status.md for exactly what they proved.
- Install guide for Amazon EKS (ECR path included), Helm chart with values
  schema and instance isolation, Kustomize overlays for both drivers.

### Security

- Bucket-policy principals are written as `auth_id:<id>`, never by name:
  on Core 7.9.2.2, name principals can canonicalize against a deleted
  same-named identity and silently match nobody.
- Filesystem-ACL grant failures fail closed (no silent world-writable
  fallback; the legacy `aclFallbackChmod` parameter parses for
  compatibility but grants with it enabled are rejected). ACL
  read-modify-write is serialized and always round-trips `flags`,
  `rights`, `posix_special_permissions`, and `control`.
- Destructive operations verify immutable identity: bucket handles record
  the root path, file ID, and a driver-ownership marker; purge and
  root-directory deletion act only on roots the driver provably created,
  and anything unproven is retained, including buckets registered at the
  filesystem root.
- CSI volume handles are HMAC-authenticated with a dedicated Secret; node
  pods receive neither the Qumulo administrative credential nor a
  Kubernetes API token. SMB mount credentials exist only in a size-limited
  in-memory volume for the duration of the mount call. SMB share rights
  are Read+Write (never CHANGE_PERMISSIONS).
- Deployment defaults: dropped capabilities, runtime-default seccomp,
  read-only root filesystems, digest-pinned images and CI actions,
  resource requests/limits, loopback health listeners, NFS root squash to
  65534, explicit SMB trustees.
- Toolchain current: Go 1.26, gRPC 1.83, k8s.io v0.36; `govulncheck`
  reports no known vulnerabilities and runs in CI with `gofmt` and the
  race detector.

### Fixed

- Upgrades from 0.1.0 converge: legacy bucket/account handles grant
  against the live root, delete with data retention, and revoke with
  name-scoped cleanup — retries always terminate. Replayed delete/revoke
  contexts parse leniently so an unknown stored parameter can never make
  cleanup unreachable.
- Retried creates reconcile every requested property (versioning, object
  lock, quota) instead of reporting success on bare existence; the
  legacy-Core deny-by-default fallback survives a crash between creation
  and lockdown.
- Grant survives orphaned identities: key listing prefers
  `?user=<auth_id>` with an owner-filtered fallback, and key rotation
  cleans up unverifiable credentials after ambiguous failures.
- Connection cache keys fingerprint the full credential set (password, CA
  bundle, TLS mode), so rotations take effect without a restart. Mounted
  credentials carrying both a token and username/password use the token
  with a warning instead of failing every RPC.
- Malformed boolean parameters are rejected as `INVALID_ARGUMENT` instead
  of silently taking defaults (`deleteRootDir: "flase"` cannot enable data
  deletion).
- CSI provisioning works with standard sidecar behavior: reserved
  `csi.storage.k8s.io/*` parameters are accepted, Kubernetes-added
  volume-context keys are tolerated, unpublish can proceed by validated
  target path when a handle no longer decodes, and SMB share
  reconciliation compares trustees by the specified identifier instead of
  a field-for-field comparison that never converges.

### Validated

Everything above was exercised against live Qumulo Core 7.9.2.2 and a
Kubernetes 1.36 cluster: full COSI claim → grant → application S3 PUT/GET,
real NFS 4.1, NFS 3, and encrypted SMB 3.1.1 pod mounts with IO, online
expansion, ordered teardown, and 0.1.0-object upgrade teardown.
docs/status.md is the authoritative record of what is and is not proven.

## 0.1.0

- COSI v1alpha1 Identity + Provisioner (`s3.qumulo.com`)
- Bucket create/delete: private-by-default, prefix, versioning, object lock, quota, purge, brownfield import
- Grant/revoke: per-access local user, key rotation, policy merge, filesystem ACE on the bucket root
- Version gate (floor 7.2.0, absolute 5.3.3) + feature detection
- Helm + Kustomize, Prometheus metrics, fake-Qumulo unit suite
- Core 7.9.2.x compatibility: `primary_group` on user create, no policy `Resource`, `%2F` file refs, access-key list without pagination when `user=` is set, flexible `uid` JSON
