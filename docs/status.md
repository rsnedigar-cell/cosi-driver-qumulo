# Project status

Independent Kubernetes storage integration for Qumulo. Release 0.2.0 includes
the `s3.qumulo.com` COSI driver and the separate `file.qumulo.com` NFS/SMB CSI
driver. It is not a Qumulo product and is not a fork of `csi-driver-qumulo`.

## COSI validation

Proven against **Qumulo Core 7.9.2.x** on a disposable lab cluster, plus
the fake-Qumulo unit suite:

- `DriverGetInfo`, `CreateBucket`, `DeleteBucket`, `GrantBucketAccess`, `RevokeBucketAccess`
- Private bucket create at `<basePath>/<name>` (the live-proven layout; an
  earlier request-fingerprinted path variant was removed in review)
- versioned bucket/account IDs with immutable directory/auth identities and a
  managed-root ownership marker; legacy `q1` handles converge: grant resolves
  the root by name, delete unregisters with data retention, revoke performs
  name-scoped best-effort cleanup and always terminates
- Brownfield `existingBucketName` import (unmanaged handle: deletes retain
  data, purge downgrades to retention) and `bucketPrefix`
- Per-`BucketAccess` local user + access key + `auth_id` bucket-policy
  statement + inheritable filesystem ACE — the live default path uses **no
  `0777` fallback** (the `aclFallbackChmod` parameter parses for
  compatibility but grants with it enabled are rejected)
- kind + COSI v0.2.2 controller + this driver: `BucketClaim` → Ready `Bucket`
  → granted Secret → application PUT/GET (path-style SigV4)

The full COSI lifecycle including every behavior above was re-locked live on
Core **7.9.2.2** (2026-08-17) via
`go test -tags=integration -run TestLiveDriverFlow ./test/integration`
(create → retry reconciliation → grant → key rotation → idempotent revoke →
purge via the recorded managed root → legacy root-bucket purge converging by
retention), and end-to-end in kind with the released distroless image:
claim/grant/Secret/boto3 PUT-GET, ordered teardown, and deletion of
BucketAccess/BucketClaim objects created by an earlier build (upgrade-path
convergence).

Operational note: the COSI controller caches `BucketInfo` per
`credentialsSecretName`. Reuse of the same secret name across a
force-finalized (manually unstuck) BucketAccess can resurface stale contents;
normal ordered deletion does not have this problem. Prefer fresh secret names
when recreating access objects after manual intervention, and prefer
long-lived access tokens over session bearer tokens in the driver Secret —
session tokens expire and wedge every RPC until replaced.

## CSI implementation and validation

The 0.2.0 CSI stack implements and unit-tests:

- CSI Identity, Controller, and Linux Node services under `file.qumulo.com`;
- dynamic directory, directory-quota, NFS-export, and SMB-share lifecycle;
- idempotent create reconciliation without retargeting an existing resource;
- NFS 3 and 4.1 mounts and SMB 3.1.1 mounts;
- root-squashed NFS rules, encrypted/ABE SMB shares, explicit trustees, and
  restricted client networks by default;
- controller-only Qumulo REST credentials and node-publish-only SMB
  credentials;
- immutable HMAC-signed volume handles shared between controller and node;
- online, non-shrinking quota expansion; and
- deletion guarded by the original immutable Qumulo directory ID.

The Qumulo NFS/SMB v2 API clients, CSI RPC behavior, mount command
construction, secret-file handling, handle authentication, and Kubernetes
manifest rendering are covered by automated tests. The default suite passes:

```bash
go test ./...
```

Live-proven on Core **7.9.2.2** (2026-08-17):

- `TestLiveFileProtocolControlPlane` (opt-in destructive control-plane
  suite): NFS export and SMB share lifecycle against the real v2 APIs.
- Full end-to-end in kind (Kubernetes 1.36) with the released `-csi` image:
  dynamic provisioning of the example PVCs, a pod writing and reading
  through a real **NFS 4.1** mount (root-squashed to 65534), a real
  **NFS 3** mount (`vers=3,nolock` negotiated, no client rpc.statd needed),
  and a real **SMB 3.1.1 encrypted** mount (`k8s-smb` trustee +
  node-publish Secret), online expansion 10Gi → 12Gi through the resizer
  and directory quota, and ordered teardown that removed the exports,
  shares, and backing directories (`deleteData: "true"`).

CSI volume snapshots (create/delete/restore-copy) are **unit-tested against
the fake server only**. They are not live-verified. Assumed REST shapes
pending a live lock:

| Surface | Assumed shape |
|---|---|
| `POST /v3/snapshots/` | `{name_suffix, source_file_id, expiration:""}` → snapshot object; visible name `<id>_<suffix>` |
| `GET /v3/snapshots/` | Required `filter` query; `{entries, paging}`; `id` is a JSON number |
| `DELETE /v3/snapshots/{id}` | 202; 404 treated as success |
| Directory listing | `GET /v1/files/{ref}/entries/?snapshot=<id>` |
| File restore | `POST /v1/files/{dest}/copy-chunk` with `source_path`/`source_id` + `source_snapshot` |
| Feature floor | Directory snapshots gated at Core 5.3.3 (pending confirmation of the v3 introducing version) |

Remaining validation gaps, stated plainly: AD/LDAP SMB trustees were not
exercised (local trustee only; explicitly deprioritized by the project
owner); the arm64 image has never run on ARM hardware; and the
least-privilege Qumulo role has not yet carried a live pass (admin token
used). Do not claim those proven.

## Core 7.9.2 API locks (do not “fix” without re-testing live)

These are live cluster behaviors, not guesses:

| Surface | Behavior |
|---|---|
| `POST /v1/users/` | Requires `primary_group` as the numeric id string (local Users is typically `"513"`). Password must meet complexity (3 of lower/upper/digit/special). |
| Bucket policy PUT | `Resource` is rejected (`ResourceSpecified`). Working statements are `Principal` + `Action` only. |
| Policy principals | Must be `auth_id:<id>`. A `local:<name>` principal is resolved at PUT time and, after a same-named user was deleted and recreated, canonicalizes against the REMOVED identity — stored as `local:` (empty), matching nobody; every S3 request 403s. (Verified 2026-08-17.) |
| S3 after Grant | Policy Allow is not enough; the bucket directory must be writable by the new user. The driver writes an inheritable ACE and fails closed if that cannot be done; the former `0777` fallback is rejected. |
| ACL PUT | Every ACE must include `flags` — omitting it fails the whole `aces` array with `decode_error`, and GET may return ACEs without it. Send `flags`, `rights`, `posix_special_permissions`, `control` always. (Verified 2026-08-17.) |
| Identity tombstones | Deleting a local user does NOT delete its S3 access keys. Name-based lookups (`?user=<name>`) can then 400 with `cred_invalid_local_user_error` against the removed identity. Use `?user=<auth_id>`, fall back to full listing filtered by owner. (Verified 2026-08-17.) |
| `POST /v1/s3/buckets/` | When `path` is set, `create_fs_path` must be present too, else `decode_error: field 'create_fs_path' not found`. (Verified 2026-08-17.) |
| `GET /v1/s3/buckets/{name}` | 405 (DELETE/PATCH only) whether or not the bucket exists. Client lists and filters; after an authoritative listing without the bucket, the client synthesizes not-found (never surfaces the 405 — delete idempotency keys off it). |
| `GET .../policy` | Returns the **bare** policy document; the ETag is only in the response header (no `{policy, etag}` wrapper). |
| File refs | One encoded path including the leading slash: `/v1/files/%2Fbase%2Fname/info/attributes`. |
| `GET /v1/users/` | Bare JSON array (not always `{users:[…]}`). `uid` may be a string, including `""`. |
| `GET /v1/s3/access-keys/?user=` | Must not also send `limit` / `after`. |
| Empty-bucket delete | `delete-root-dir=true` 409s if a prefix directory remains after object delete. |
| AWS SDK checksums | Default trailer checksums fail; use `when_required`. |
| REST JSON bodies | Rejected if they carry a UTF-8 BOM (`MalformedBucketPolicy("expected value at line 1 column 1")`). Relevant to hand-driven `curl` from Windows, not the driver. |
| `GET /v2/nfs/exports/` | Returns a **bare JSON array** (no `{entries}` envelope). `restrictions[].required_authentication_mode` is a real v2 field — Core returns it (e.g. `AUTHENTICATION_MODE_NONE`). (Verified 2026-08-17.) |
| NFSv4 enablement | Core ships with `v4_enabled: false`; `mount.nfs4` then fails with `Protocol not supported`. Enabling v4 (`PATCH /v2/nfs/settings`) is refused with `nfs_export_tree_name_is_prefix_error` while any export path is a prefix of another — the factory `/` export conflicts with every driver export and must be removed or narrowed first. (Verified 2026-08-17.) |
| Collection POST paths | `POST /v1/users` (no trailing slash) is 404; the collection is `/v1/users/`. The same trailing-slash rule applies to the other v1 collections. |

## Known limitations and operational notes

1. **Least-privilege Qumulo user** is documented and scripted; live
   validation so far used an admin identity.
2. **Real-cluster and live mount e2e are not in CI.** Unit/API tests, fake
   control-plane tests, manifest rendering/schema checks, `gofmt`, the race
   detector, and `govulncheck` are.
3. **Optional class features:** quota and `purgeOnDelete` were re-locked live
   on 7.9.2.2 (2026-08-17); versioning and object lock have unit coverage
   and API-shape verification only.
4. **The arm64 image is cross-compiled in CI** and has not been executed on
   ARM hardware.
5. **CSI handle key durability is operationally critical.** Back up
   `qumulo-csi-handle-key` and do not rotate it while any associated PV
   exists. See [csi.md](csi.md).

Module: `github.com/rsnedigar-cell/cosi-driver-qumulo`.
Image: `ghcr.io/rsnedigar-cell/cosi-driver-qumulo`.
