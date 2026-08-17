# COSI class parameters

This page describes `s3.qumulo.com` COSI `BucketClass` and
`BucketAccessClass` parameters. The independent `file.qumulo.com` CSI
StorageClass parameters for NFS and SMB are documented in
[csi.md](csi.md#storageclass-parameters).

`endpoint` is a **hostname or IP**, optionally with an `https://` prefix.
Do not put `:8000` or `:9000` in `endpoint`. The driver always uses
`restPort` and `s3Port` for the actual sockets. A port in the URL is
ignored when those parameters are set (and they default to 8000 / 9000).

Accepted forms: `qumulo.apps.internal`, `https://qumulo.apps.internal`,
`192.0.2.10`.

## BucketClass

| Parameter | Default | Notes |
|---|---|---|
| `endpoint` | `QUMULO_ENDPOINT` | REST host. Also used as the S3 host reported to apps. **Required** unless the process default is set. |
| `restPort` | `8000` | REST API port. |
| `s3Port` | `9000` | Reported data-plane port. Qumulo's S3 listener is fixed at 9000. |
| `credentialsSecretName` | unset | Optional advanced override. The standard manifests deliberately do not expose a Kubernetes API token to the driver, so use the mounted `/etc/qumulo/credentials` Secret. A custom deployment must explicitly provide Kubernetes client credentials. |
| `credentialsSecretNamespace` | driver namespace | Applies only to the advanced class-Secret override. Extra namespaces require `--secret-namespaces`. |
| `basePath` | `/k8s-buckets` | Directory that holds bucket root dirs. |
| `bucketPrefix` | `""` | Optional prefix for derived bucket names (counts against the 63-character S3 limit). Deterministic per class; changing it on a class with live claims makes retried creates resolve to new names, so treat it as fixed once set. |
| `deleteRootDir` | `true` | `DELETE ?delete-root-dir=`. |
| `purgeOnDelete` | `false` | Resolves the bucket's **real** root from the cluster, unregisters, then `tree-delete`s that directory. Refuses buckets rooted at `/`. Dangerous; opt-in. |
| `quotaLimit` | unset | Bytes; directory quota on the bucket root. |
| `versioning` | unset | `Enabled` / `Suspended` / `Unversioned`. Requires Core ≥ 7.1. Leave unset for a first bring-up. |
| `objectLockEnabled` | `false` | Compliance mode only. Driver enables versioning first. Requires Core ≥ 7.2. |
| `region` | `us-east-1` | Reported to apps; Qumulo accepts any string. |
| `existingBucketName` | unset | Brownfield import: binds the claim to a pre-existing bucket instead of creating one. The handle is unmanaged — deletes unregister the bucket but always retain its data, and `purgeOnDelete` downgrades to retention. Class feature parameters (versioning, quota, lock) are not imposed on imported buckets. |
| `insecureSkipTLSVerify` | `false` | REST only. Logs a warning. Never touches `http.DefaultTransport`. |

All boolean parameters parse strictly: a malformed value (e.g.
`deleteRootDir: "flase"`) fails the request instead of silently taking the
default.

## BucketAccessClass

| Parameter | Default | Notes |
|---|---|---|
| `accessMode` | `rw` | `ro` / `rw` / `full` → S3 action sets (see `internal/policy/actions.go`). |
| `aclFallbackChmod` | `false` | Parses for compatibility (so replayed delete/revoke contexts never wedge), but grants with it enabled are rejected: the world-writable fallback stays disabled because a lost Grant response could prevent durable restoration of the original mode. |

`authenticationType` must be `Key`. `IAM` is rejected with `UNIMPLEMENTED`.

A grant writes three things: an access key for a dedicated local user, a
bucket-policy statement whose principal is the user's `auth_id`, and an
inheritable ACE on the bucket root directory. All three were verified live
end-to-end (SigV4 PUT/GET/DELETE) on Core 7.9.2.2.

## Driver credentials Secret

Keys the driver reads from the mounted `qumulo-cosi-creds` Secret (or, in a
custom deployment with explicitly supplied Kubernetes client credentials, a
class-referenced Secret):

| Key | Required | Notes |
|---|---|---|
| `token` | one of token or user/pass | Qumulo access token (preferred). Aliases: `accessToken`, `bearer`. |
| `username` | with `password` | Fallback session login. |
| `password` | with `username` | Fallback session login. |
| `ca.crt` | no | PEM bundle for REST TLS. Alias: `ca.pem`. |

## Credential Secret keys (what apps see)

The sidecar serializes driver credentials into `BucketInfo.spec.secretS3`:

| Key | Value |
|---|---|
| `accessKeyID` | Qumulo S3 access key |
| `accessSecretKey` | shown once at grant time |
| `endpoint` | `https://<host>:9000` |
| `region` | class `region` |

The credentials map key is `"s3"`. The usual mount path is `/cosi/BucketInfo`.

## CSI StorageClass summary

CSI requires `protocol: nfs` or `protocol: smb`, a restricted
`allowedNetworks` list (unless `allowAllHosts: "true"` is deliberately set),
and uses `/k8s-volumes` as its default base path. NFS defaults to version 4.1
with root squash to UID/GID 65534. SMB defaults to 3.1.1 transport, required
encryption, access-based enumeration, and requires an explicit trustee unless
`allowAllSMBUsers: "true"` is set. SMB node credentials are referenced with
the standard CSI node-publish Secret parameters.

See the [complete common, NFS, and SMB parameter tables](csi.md#storageclass-parameters).

## Application S3 client notes (Qumulo 7.9)

- Signature **v4** only; path-style addressing only.
- Recent AWS SDKs (boto3 1.36+, recent AWS CLI v2) must set request checksums
  to **when_required**. Default trailer checksums return
  `QumuloUnsupportedTrailerChecksumFormat`.
- Do not assume ETag is MD5.
- The default S3 listener uses a self-signed cert; mount a CA or disable
  verification in the **app**, not in the driver data-plane URL.
