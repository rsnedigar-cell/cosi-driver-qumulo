# Qumulo-side setup

The COSI and CSI controllers authenticate to the Qumulo REST API (TCP
**8000**) as a service user. Prefer a long-lived **access token** over
embedding an admin password in Kubernetes. CSI node pods do not need and
must not receive this administrative REST credential.

S3 application traffic uses TCP **9000** and never goes through the driver.
Qumulo Core does not let you change that port.

## 1. Enable S3

```bash
qq s3_modify_settings --enable
qq s3_get_settings
```

Every node then accepts S3 API traffic on **9000** (HTTPS, self-signed
certificate by default). REST stays on **8000**.

## 2. Least-privilege role

The helper validates every required privilege against
`qq auth_list_privileges` before making changes and fails closed when the Core
version is incompatible. Do not skip a missing privilege.

| Privilege | Why |
|---|---|
| `PRIVILEGE_S3_BUCKETS_READ` / `_WRITE` | Bucket CRUD + policy |
| `PRIVILEGE_S3_SETTINGS_READ` | Verify that the S3 service is enabled before each operation |
| `PRIVILEGE_S3_CREDENTIALS_READ` / `_WRITE` | Access keys |
| `PRIVILEGE_LOCAL_USER_READ` / `_WRITE` and `PRIVILEGE_LOCAL_GROUP_READ` | Per-access local users and their default group |
| `PRIVILEGE_FS_ATTRIBUTES_READ` | Filesystem metadata needed during reconciliation |
| `PRIVILEGE_QUOTA_READ` / `_WRITE` | Optional per-bucket quota |
| `PRIVILEGE_FS_DELETE_TREE_READ` / `_WRITE` | Poll and submit COSI `purgeOnDelete` and CSI `deleteData` jobs. **WRITE bypasses filesystem permissions and can delete anywhere on the cluster.** |
| `PRIVILEGE_S3_UPLOADS_READ` / `_WRITE` | List and abort multipart uploads before bucket deletion |
| `PRIVILEGE_NFS_EXPORT_READ` / `_WRITE` | CSI NFS export reconciliation |
| `PRIVILEGE_SMB_SHARE_READ` / `_WRITE` | CSI SMB share reconciliation |

`PRIVILEGE_ACCESS_TOKENS_WRITE` remains admin-only. Use an administrator once
to mint the driver's token; do not add that privilege to the driver role.

Helper script: [hack/create-driver-role.sh](../hack/create-driver-role.sh).

A cluster-admin identity works for a lab. It is **not** the intended
production identity; the least-privilege role above is.

## 3. Service user + access token

```bash
qq auth_add_user --name cosi-driver
# role + assignment: see the helper script
qq auth_create_access_token local:cosi-driver
```

Store the token in Kubernetes Secret `qumulo-cosi-creds`, key `token`.
See [install.md](install.md) step 6.

Username and password are supported as a fallback (the driver re-logins on
401). Prefer the token.

### SMB data-plane identity

The CSI controller service user above is not the SMB mount identity. For the
bundled SMB StorageClass, create a separate local user and set its password at
Qumulo's interactive prompt (do not place the password on the command line):

```bash
qq auth_add_user --name k8s-smb --password
```

Use the same password in the Kubernetes `qumulo-smb-credentials` Secret. The
StorageClass trustee `LOCAL/k8s-smb` grants this identity access to each share;
the local SMB login is normally sent without a domain. You may instead use an
existing AD or LDAP identity—change both the StorageClass trustee and the
node-publish Secret, including the real login domain when required. Do not
assign the controller REST role to the SMB-only user.

## 4. Filesystem base path

Create both default controller roots as an administrator, then grant the
service user effective and inheritable access only below those paths. This is
the path-scoped alternative to the unsafe cluster-wide
`PRIVILEGE_FILE_FULL_ACCESS` privilege for ordinary filesystem operations.
It does not constrain `PRIVILEGE_FS_DELETE_TREE_WRITE`: Qumulo defines that
privilege as permission to delete any file or directory cluster-wide,
regardless of filesystem permissions. The packaged CSI classes need it while
`deleteData: "true"`; COSI needs it only for `purgeOnDelete: "true"`. Protect
the controller token as a destructive cluster-wide credential. For a
preservation-first deployment, set CSI `reclaimPolicy: Retain` and
`deleteData: "false"`, keep COSI purge disabled, and remove both tree-delete
privileges from a tailored role.

```bash
qq fs_create_dir --path / --name k8s-buckets
qq fs_create_dir --path / --name k8s-volumes

qq fs_modify_acl --path /k8s-buckets add_entry \
  --trustee local:cosi-driver --type Allowed --rights All \
  --flags 'Container inherit' 'Object inherit'
qq fs_modify_acl --path /k8s-volumes add_entry \
  --trustee local:cosi-driver --type Allowed --rights All \
  --flags 'Container inherit' 'Object inherit'
```

If a root already exists, skip only its `fs_create_dir` command. If you change
`basePath`, apply the ACE to the configured path instead. Imported bucket paths
must also grant this service user direct `Read ACL` and `Write ACL` access.

The COSI driver creates each bucket's root directory at
`<basePath>/<bucketName>` with `create_fs_path: true` and records the root's
immutable path and file ID in the bucket handle. Change `basePath` on the
`BucketClass` if you use a different directory.

## 5. Certificates

Out of the box the REST and S3 listeners use a self-signed certificate.

- **Driver (REST).** Either mount the CA as `ca.crt` in `qumulo-cosi-creds`,
  or set `parameters.insecureSkipTLSVerify: "true"` on the `BucketClass`
  for a lab. The flag logs a warning and does not touch
  `http.DefaultTransport`.
- **Applications (S3).** The driver reports `https://<host>:9000` and never
  silently disables TLS on the data plane. Mount a CA in the app, install a
  real certificate on the cluster, or disable verification **in the
  application** (the smoke job does this for a lab).

## 6. Network from Kubernetes

The hostname you put in `BucketClass.parameters.endpoint` must resolve and
connect from **inside the cluster**:

| Traffic | Port | Who |
|---|---|---|
| REST (HTTPS) | 8000 | Driver pods |
| S3 (HTTPS) | 9000 | Application pods |

On AWS, that usually means the same VPC as Cloud Native Qumulo, or a peered
VPC / Direct Connect / VPN path, plus security-group rules for those two
ports. Details: [install.md](install.md).

## CSI file-protocol setup

The `file.qumulo.com` CSI service manages NFS and SMB independently from the
S3/COSI objects above. It uses stable Qumulo v2 NFS-export and SMB-share APIs
and creates volume directories below `/k8s-volumes` by default. Qumulo's API
references describe the managed [NFS v2 exports](https://docs.qumulo.com/rest-api-guide/nfs-methods-v2/v2_nfs_exports_ref.html)
and [SMB v2 shares](https://docs.qumulo.com/rest-api-guide/smb-shares-methods-v2/v2_smb_shares_ref.html).

Give the CSI controller service role the least-privilege equivalents for:

- reading and creating filesystem directories and attributes;
- reading, creating, and updating directory quotas;
- listing, reading, creating, modifying, and deleting NFS exports;
- listing, reading, creating, modifying, and deleting SMB shares; and
- tree deletion, only when a StorageClass uses `deleteData: "true"`.

Privilege names vary by Qumulo Core version. Confirm the exact names with
`qq auth_list_privileges` on the target cluster. A cluster-admin token is
acceptable for initial validation, but the node DaemonSet must never mount
it and production should use a constrained service identity.

Configure a process-level REST endpoint and data-server name. Worker nodes,
not controller pods, originate file traffic:

| Traffic | Typical port | Who |
|---|---|---|
| Qumulo REST HTTPS | TCP 8000 | CSI controller |
| NFS 4.1 | TCP 2049 | Linux worker nodes |
| NFS 3 | Site/Qumulo NFS 3 RPC service ports | Linux worker nodes |
| SMB | TCP 445 | Linux worker nodes |

The CSI driver supports NFS 3 and 4.1 and mounts SMB as 3.1.1. Verify the
selected protocol is enabled and reachable on every Qumulo data endpoint
used by `QUMULO_DATA_SERVER`, including after failover.

**NFS 4.1 requires cluster-side enablement.** Qumulo Core ships with NFSv4
disabled; the default `nfsVersion: "4.1"` StorageClass then fails every
mount with `mount.nfs4: Protocol not supported`. Additionally, Core refuses
to enable v4 while any NFS export path is a prefix of another — the factory
`/` export conflicts with every driver-managed export, so remove or narrow
it first:

```bash
# Remove the factory root export (or re-point it below a narrower path),
# then enable v4. Both require an administrative session.
qq nfs_delete_export --export-path /
qq nfs_modify_settings --enable-v4
```

Plan this as a maintenance step: removing `/` affects any existing NFSv3
clients that mount the root export, and enabling v4 is cluster-wide.
(Live-verified against Core 7.9.2.2, 2026-08-17.)

Every StorageClass must constrain its clients with `allowedNetworks`, unless
`allowAllHosts: "true"` is explicitly chosen. NFS defaults to root squash
with anonymous UID/GID 65534. SMB defaults to encryption and access-based
enumeration and requires a trustee authentication ID or trustee domain/name;
`allowAllSMBUsers: "true"` is the explicit broad-access escape hatch.

The Qumulo SMB share trustee and the `qumulo-smb-credentials` node-publish
Secret are related but distinct: the trustee defines authorization on the
share, while the Secret authenticates the Linux CIFS client. Make sure that
credential resolves to an identity allowed by the configured trustee.

CSI also requires a dedicated HMAC handle-key Secret shared by controller
and node replicas. It is not a Qumulo credential. Back it up and never rotate
it while its PVs exist. Complete setup: [csi.md](csi.md).
