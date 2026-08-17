# Architecture

The release image contains two independent Go binaries and Kubernetes
services:

| Service | Driver name | Responsibility |
|---|---|---|
| `/qumulo-cosi-driver` | `s3.qumulo.com` | COSI `Identity` + `Provisioner` for S3 buckets and access keys |
| `/qumulo-csi-driver` | `file.qumulo.com` | CSI Identity, Controller, and Linux Node services for NFS/SMB volumes |

COSI is an **alpha** Kubernetes API
(`objectstorage.k8s.io/v1alpha1`). Its driver shares a UNIX socket with the
official v0.2.2 sidecar. The CSI deployment runs controller and node modes as
separate workloads, each with the standard Kubernetes CSI sidecars.

Both drivers are control-plane integrations. Object GET/PUT traffic goes
from the application to Qumulo S3 (`:9000`), while NFS/SMB traffic goes from
Linux worker nodes to the configured Qumulo data server. Neither data plane
passes through a controller pod.

## COSI connection resolution

COSI v1alpha1 does not pass per-request secrets. The standard deployment binds
all classes to the process `QUMULO_ENDPOINT` and mounted
`/etc/qumulo/credentials`; any class endpoint must match that process endpoint.
The Kubernetes API token is projected only into the COSI sidecar, never into
the Qumulo driver container. Class-referenced credential Secrets remain an
advanced option for custom deployments that explicitly provide the driver a
Kubernetes client credential; they are not available in the standard
manifests. Deploy a separate driver name and workload for each Qumulo cluster.

`DriverDeleteBucket`, Grant, and Revoke only receive opaque IDs plus their
contexts. Connection and immutable Qumulo object identity are encoded in
versioned bucket and account IDs. A managed `q3` bucket ID also carries an
explicit ownership marker; legacy chmod-fallback grants used a different
`q3` account-ID shape:

```
q3:qumulo:<endpoint>:<restPort>:<bucketName>:<rootPath>:<rootFileID>:managed
q2:qumulo:<endpoint>:<restPort>:<bucketName>:<rootPath>:<rootFileID> (legacy)
q2:qumulo:<endpoint>:<username>:<accessKeyPrefix>:<authID>
q3:qumulo:<endpoint>:<username>:<accessKeyPrefix>:<authID>:<restoreMode> (legacy)
```

Fields that can contain separators are URL-encoded. New buckets carry the
root path, immutable filesystem ID, and a driver-ownership bit so purge
retries remain safe after the S3 bucket registration is gone and normal
deletion never infers ownership from a user-controlled path name. New access
grants carry the immutable Qumulo authentication ID so revoke cannot mistake
a deleted-and-recreated same-named user for the original identity. New grants reject
`aclFallbackChmod`; the decoder retains `q3` support so an older grant can
still restore its recorded POSIX mode during revoke.

The decoder remains compatible with pre-0.2.0 `q1` IDs:

```
q1:qumulo:<endpoint>:<restPort>:<bucketName>
q1:qumulo:<endpoint>:<username>:<accessKeyPrefix>
```

Legacy IDs receive the safest behavior available from their older fields.
New bucket IDs are managed `q3`; new account IDs are `q2`. The decoder accepts
`q1`, `q2`, and both applicable `q3` shapes. A legacy bucket handle without
the ownership bit retains its root on normal delete. Secrets are never placed
in any format.

## COSI idempotency

The sidecar retries every non-OK status. Managed roots live at
`<basePath>/<bucketName>`; the bucket handle records the root's immutable
path and file ID at create time, and every later destructive operation
verifies both against the live cluster before acting. Create requests are
not blindly replayed after an ambiguous result; the client performs an
authoritative lookup and reconciles only a bucket whose name and path match.

| Situation | Result |
|---|---|
| Create, same name + exact parameters | success, existing bucket |
| Create, same name + changed class parameters | success; the bucket is reconciled to the requested properties |
| Delete, 404 | success |
| Delete, root not empty, no purge | `FAILED_PRECONDITION` |
| Delete, MPUs in progress | abort, retry once, else `FAILED_PRECONDITION` |
| Grant, user/keys exist | rotate keys, return fresh secret and immutable auth ID |
| Revoke, already gone | success; tombstoned or replacement identities are not deleted |
| 5xx / network | `UNAVAILABLE` |
| Auth failure | `UNAUTHENTICATED` |

Brownfield imports through `existingBucketName` bind a claim to an existing
bucket with an **unmanaged** handle: deletion unregisters the bucket but
always retains its data, and `purgeOnDelete` downgrades to retention —
destructive cleanup is reserved for roots the driver provably created.

## COSI access model

1. `username = "cosi-" + first32hex(sha256(bucket_id + ":" + access_name))`
2. Create the local user if missing (unusable random password;
   `primary_group` = local Users).
3. Delete existing keys for that user and mint a new pair (2-key limit).
4. Read-modify-write the bucket policy with `If-Match` ETag. Core 7.9.2+
   rejects `Resource`; statements are Principal + Action only.
5. Grant an inheritable ACE on the bucket directory. This fails closed if the
   ACL cannot be written; the former world-writable chmod fallback is
   rejected. Policy alone is not enough to PUT.
6. Return a `q2` `account_id` containing the immutable `auth_id`, plus `s3`
   credentials.

Revoke uses the account's immutable identity: delete keys → remove policy Sid
→ drop the ACE → delete the original `cosi-*` user. A missing original
identity is success. A different identity that later acquired the same name
or bucket path is preserved. Revoking a `q3` grant also restores the recorded
pre-grant directory mode after removing access.

## CSI provisioning and node path

The CSI controller is bound to one configured Qumulo REST endpoint and one
data server. StorageClass parameters cannot redirect it to another cluster.
For each request it creates or reconciles:

```
PVC → csi-provisioner → directory → optional directory quota
                                  ├─ NFS export → Linux node mount.nfs
                                  └─ SMB share  → Linux node mount.cifs
```

The backing directory is `<basePath>/<protocol>/<deterministic-name>`. The
controller does not retarget an existing export/share and refuses a name that
conflicts with another protocol or directory. Quota updates are monotonic:
create and expand preserve the greatest committed limit, so retries and
concurrent expansion cannot shrink storage. Expansion is rejected when quota
enforcement was disabled at volume creation.

The returned `qv1` volume handle is an HMAC-SHA256-authenticated opaque
record. It binds the endpoint, data server, protocol resource, immutable
directory ID, capacity, quota setting, and deletion behavior. A dedicated
key of at least 32 bytes is mounted in controller and node pods; Qumulo
administrative credentials remain controller-only. Handles that are tampered
with or belong to another configured endpoint/data server are rejected.

The node accepts targets only below its configured kubelet root, rejects
existing symlink components, and blocks mount options that could inject
credentials, redirect the source, or override controlled NFS/SMB security
settings. NFS supports versions 3 and 4.1. SMB is mounted as 3.1.1, using
node-publish credentials supplied by kubelet.

On deletion the controller removes the export/share first. If `deleteData`
was recorded as true, it verifies the path's current immutable directory ID
before queuing tree deletion. This prevents a stale PV from deleting a
replacement directory. See [csi.md](csi.md) for the complete lifecycle and
deployment model.

## Observability

- Both binaries use structured `slog` JSON. Secrets and tokens are never
  logged at info level.
- Both expose gRPC health. The COSI process also exposes HTTP `/healthz` and
  `/metrics` when configured.
- COSI counters include `qumulo_cosi_rpc_total` and
  `qumulo_cosi_api_errors_total{error_class}`.
