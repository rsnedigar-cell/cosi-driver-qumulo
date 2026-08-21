# cosi-driver-qumulo

Kubernetes storage drivers for Qumulo:

- A [Container Object Storage Interface](https://container-object-storage-interface.sigs.k8s.io/) (COSI) driver for provisioning Qumulo S3 buckets and scoped access keys.
- A separate [Container Storage Interface](https://github.com/container-storage-interface/spec) (CSI) driver for dynamically provisioning Qumulo NFS and SMB volumes on Linux nodes.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![COSI](https://img.shields.io/badge/COSI-v0.2.2-informational.svg)](https://github.com/kubernetes-sigs/container-object-storage-interface/releases/tag/v0.2.2)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev/)

| | |
|---|---|
| COSI driver name | `s3.qumulo.com` |
| CSI driver name | `file.qumulo.com` |
| COSI API | `objectstorage.k8s.io/v1alpha1` (alpha) |
| COSI release | [v0.2.2](https://github.com/kubernetes-sigs/container-object-storage-interface/releases/tag/v0.2.2) |
| Qumulo Core | Floor 7.2.0 (S3 GA 5.3.3); exercised on 7.9.2.x |
| Build toolchain | Go 1.26 (module toolchain 1.26.6) |
| COSI authentication | Key only (`IAM` is rejected) |
| Image | `ghcr.io/rsnedigar-cell/cosi-driver-qumulo:0.2.0` (`linux/amd64`, `linux/arm64`) |
| License | [Apache-2.0](LICENSE) |

This is an **independent** project. It is not a Qumulo product and is not a
fork of `csi-driver-qumulo`. Source:
https://github.com/rsnedigar-cell/cosi-driver-qumulo

## Install in brief

The S3/COSI walkthrough, including AWS networking, ECR, and a smoke test, is
in **[docs/install.md](docs/install.md)**. The NFS/SMB CSI deployment and its
security model are in **[docs/csi.md](docs/csi.md)**.

1. Open **TCP 8000** (driver → Qumulo REST) and **TCP 9000** (apps → Qumulo S3) from the EKS nodes to the Qumulo cluster.
2. On Qumulo: enable S3, create `/k8s-buckets`, mint an access token. See [docs/qumulo-setup.md](docs/qumulo-setup.md).
3. Pull the 0.2.0 multi-architecture image, or build it and push it to your registry.
4. Install COSI v0.2.2.
5. Create the `qumulo-cosi-creds` Secret, then install this driver.
6. Configure the driver process endpoint, then apply a `BucketClass` whose
   `parameters.endpoint` matches it (or omit the class endpoint to inherit it).
7. Apply a `BucketClaim` + `BucketAccess`. Mount the issued Secret and speak path-style SigV4.

```bash
# COSI controller + CRDs
kubectl apply -k github.com/kubernetes-sigs/container-object-storage-interface?ref=v0.2.2

# Driver (after the credentials Secret exists — see docs/install.md)
# First replace qumulo.example.internal in deploy/kustomize/deployment.yaml.
kubectl apply -k deploy/kustomize
# or
helm install qumulo-cosi deploy/helm/cosi-driver-qumulo -n qumulo-cosi \
  --create-namespace --set qumulo.endpoint=qumulo.example.internal
```

For NFS and SMB, either apply [`deploy/csi`](deploy/csi) or enable the CSI
stack in the Helm chart with `--set csi.enabled=true`. CSI requires a
dedicated handle-signing key and restricted client networks; see
[docs/csi.md](docs/csi.md) before enabling it.

## How it works

```
BucketClaim  →  objectstorage-controller  →  Bucket
                                              │
                         sidecar ──gRPC──►  qumulo-cosi-driver
                                              │
                                    Qumulo REST :8000
                                    S3 data plane :9000
BucketAccess →  sidecar ──Grant──►  local user + access key + bucket policy + FS ACE
                 Secret (BucketInfo) ──►  application pod
```

Each `BucketAccess` gets an isolated local user (`cosi-<hash>`), a rotated key pair (the secret is shown once), a bucket-policy statement for that user, and a filesystem ACE on the bucket directory. Buckets are created `private: true`.

The CSI controller creates a directory, a non-shrinking directory quota by
default, and either an NFS export or SMB share. Linux node plug-ins mount that
data path directly; file traffic does not pass through the controller. NFS
versions 3 and 4.1 are supported. SMB mounts are fixed to SMB 3.1.1.
CSI volume snapshots (create/delete/restore-into-new-volume) are implemented
and unit-tested **(unreleased; not yet live-verified)**.

## Use

```yaml
apiVersion: objectstorage.k8s.io/v1alpha1
kind: BucketClaim
metadata:
  name: photos
  namespace: app
spec:
  bucketClassName: qumulo-gold
  protocols: ["s3"]
---
apiVersion: objectstorage.k8s.io/v1alpha1
kind: BucketAccess
metadata:
  name: photos-access
  namespace: app
spec:
  bucketClaimName: photos
  bucketAccessClassName: qumulo-rw
  credentialsSecretName: photos-s3-creds
  protocol: s3
```

The sidecar writes a `BucketInfo` Secret. Applications must:

- Use **AWS Signature v4** (SigV2 returns `400 AuthorizationHeaderMalformed`)
- Use **path-style** addressing (`s3ForcePathStyle` / `addressing_style: path`)
- Talk to `https://<cluster>:9000`
- Not assume ETags are MD5
- Disable optional AWS request-trailer checksums (`when_required`); Qumulo rejects CRC trailers

Worked example: [deploy/examples/job-boto3.yaml](deploy/examples/job-boto3.yaml).
Parameters: [docs/parameters.md](docs/parameters.md).

### Brownfield import

Set `existingBucketName` on a BucketClass to bind claims to a pre-existing
bucket instead of creating one. Imported buckets get an **unmanaged** handle:
deleting the claim unregisters the bucket but always retains its data, and
`purgeOnDelete` downgrades to retention, because the driver cannot prove it
owns an imported root. Class feature parameters (versioning, quota, object
lock) are not imposed on imported buckets.

### Deletion

| Setting | Effect |
|---|---|
| `deletionPolicy: Retain` | COSI never calls the driver; the Qumulo bucket stays |
| `deleteRootDir: "true"` (default) | Empty-bucket delete also removes the directory |
| `deleteRootDir: "false"` | Unregisters the bucket, leaves data |
| `purgeOnDelete: "true"` | Unregister then `tree-delete` a non-empty root. **Off by default.** |

Managed roots live at `<basePath>/<bucketName>`; the driver records the
root's immutable path and file ID in the bucket handle and verifies both
before any destructive action. Unmarked pre-0.2.0 or brownfield roots are
unregistered but never unlinked by delete or purge — retention is forced for
any root the driver cannot prove it created.

## S3 facts this driver will not hide

Qumulo S3 (Core 7.9.x) does **not** implement: bucket ACL writes, lifecycle, notifications, server access logging, storage classes, static web hosting, STS, SSE configuration control, or CRC32/CRC32C checksums. Object Lock is **compliance mode only** and requires versioning first. Limits: 5 GiB single PUT, 48.8 TiB multipart, 16,000 buckets/cluster, object keys ≤ 1,530 characters.

## Documentation

| Document | Contents |
|---|---|
| [docs/install.md](docs/install.md) | EKS/AWS install, ECR, smoke test, troubleshooting |
| [docs/csi.md](docs/csi.md) | NFS/SMB CSI install, secrets, StorageClass parameters, lifecycle |
| [docs/qumulo-setup.md](docs/qumulo-setup.md) | Enable S3, role, token, base path, TLS |
| [docs/parameters.md](docs/parameters.md) | `BucketClass` / `BucketAccessClass` parameters |
| [docs/architecture.md](docs/architecture.md) | Connection IDs, idempotency, access model |
| [docs/status.md](docs/status.md) | What is proven, what is not |
| [SECURITY.md](SECURITY.md) | Vulnerability reporting |
| [CHANGELOG.md](CHANGELOG.md) | Release notes |

## Develop

Source builds require Go 1.26; the module selects the 1.26.6 toolchain.

```bash
make test
make build
# against a real cluster
QUMULO_HOST=https://cluster:8000 QUMULO_TOKEN=... make integration
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

v0.2.0 includes S3 COSI plus Linux NFS/SMB CSI. Both drivers were validated
end-to-end against live Qumulo Core 7.9.2.2: the full COSI lifecycle with
application S3 PUT/GET, and CSI pods doing real IO through NFS 4.1, NFS 3,
and encrypted SMB 3.1.1 mounts, including online expansion and full teardown.
Remaining gaps are listed plainly in [docs/status.md](docs/status.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
