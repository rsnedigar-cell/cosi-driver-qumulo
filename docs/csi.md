# NFS and SMB CSI driver

The `file.qumulo.com` CSI driver dynamically provisions Qumulo-backed
PersistentVolumes for Linux workloads. It is a separate service from the
`s3.qumulo.com` COSI driver: COSI manages S3 buckets, while CSI manages file
directories, quotas, NFS exports, SMB shares, and node mounts.

The release image contains two binaries:

| Binary | Purpose |
|---|---|
| `/qumulo-cosi-driver` | COSI Identity and Provisioner services for S3 |
| `/qumulo-csi-driver` | CSI Identity, Controller, and Node services for NFS/SMB |

`qumulo-csi-driver` accepts `--mode=controller`, `--mode=node`, or
`--mode=all`. The provided deployments run controller and node separately;
`all` is intended for tests or a deliberately combined deployment. Node mode
requires Linux, root privileges, the NFS/CIFS mount helpers in the release
image, and host mount propagation into the kubelet directory.

## Security boundaries and required Secrets

Use three separate Secrets. They serve different trust domains and must not
be combined:

| Secret | Mounted or supplied to | Required keys | Purpose |
|---|---|---|---|
| `qumulo-cosi-creds` | COSI driver and CSI controller; never CSI node | `token`, or `username` + `password`; optional `ca.crt` | Administrative Qumulo REST operations |
| `qumulo-csi-handle-key` | CSI controller and every CSI node | `key`, at least 32 bytes | HMAC authentication of CSI volume handles |
| `qumulo-smb-credentials` | Kubelet passes it only to SMB `NodePublishVolume` | `username` + `password`; optional `domain` | SMB data-plane mount authentication |

The controller REST identity should have only the Qumulo privileges required
to manage directories, quotas, NFS exports, and SMB shares. The CSI node
DaemonSet must not receive this administrative Secret. Conversely, SMB mount
credentials are not controller credentials and are not mounted globally into
the node pod. The node writes them to a mode-`0600` file in a size-limited,
in-memory directory for the mount call, then removes the file.

Create and back up a dedicated handle key before installation:

```bash
kubectl create namespace qumulo-cosi
kubectl create secret generic qumulo-csi-handle-key \
  --namespace qumulo-cosi \
  --from-literal=key="$(openssl rand -base64 48)"
```

The key file must contain at least 32 bytes and be identical on every
controller and node replica. Volume IDs are immutable, HMAC-SHA256-signed
handles that bind the protocol, endpoint, data server, Qumulo directory and
share/export identities, capacity, quota setting, and deletion policy.
Tampered handles and handles for another configured endpoint are rejected.

Back up this Secret with the cluster's persistent storage metadata. **Do not
rotate or replace it while any PV created with it exists.** Losing the key
makes existing handles unverifiable, so the driver cannot mount, expand, or
delete those volumes. Key rotation requires an explicit handle-migration
procedure, which this release does not provide.

Create the controller and SMB Secrets separately:

```bash
# Run on an administrative qq client. This prompts securely for the SMB
# password; use that same value in the Kubernetes Secret below.
qq auth_add_user --name k8s-smb --password

kubectl create secret generic qumulo-cosi-creds \
  --namespace qumulo-cosi \
  --from-literal=token='PASTE_THE_QUMULO_ACCESS_TOKEN' \
  --from-file=ca.crt=./qumulo-ca.crt

kubectl create secret generic qumulo-smb-credentials \
  --namespace qumulo-cosi \
  --from-literal=username='k8s-smb' \
  --from-literal=password='PASTE_THE_SMB_PASSWORD'
```

Production TLS verification is on by default. Because a factory Qumulo
management certificate is self-signed, include the issuing CA as `ca.crt` as
shown, or install a certificate trusted by the image. For a lab only, omit the
CA and set `QUMULO_INSECURE_SKIP_TLS_VERIFY: "true"` in the standalone
ConfigMap, or Helm `csi.insecureSkipTLSVerify=true`. Never use that bypass in
production.

The bundled SMB StorageClass uses `LOCAL/k8s-smb` as the Qumulo REST trustee
identity for the share ACL. That `LOCAL` trustee namespace is not the client
login domain. Omit `domain` in the mount Secret for an unqualified
Qumulo-local SMB login, as above. If client authentication requires a domain,
set it to the actual cluster/NetBIOS or directory domain. NFS mounts do not
use an SMB credential Secret. An existing AD/LDAP user can replace the local
user, but the StorageClass trustee and Secret login/domain must be changed
together. The SMB user does not need the controller's Qumulo REST role.

## Access-policy defaults

The driver refuses to create a volume unless `allowedNetworks` is supplied
on the StorageClass or `--allowed-networks` is configured for the process.
Set `allowAllHosts: "true"` only when an unrestricted storage network is an
intentional design decision. This guard applies to both NFS host restrictions
and SMB network permissions.

NFS defaults to root squash, mapping UID and GID 0 to anonymous UID/GID
`65534`. SMB defaults to required encryption and access-based enumeration.
An SMB share also requires one explicit trustee:

- `smbTrusteeAuthID`, or
- both `smbTrusteeDomain` and `smbTrusteeName`.

`allowAllSMBUsers: "true"` grants the share to `WORLD/Everyone` and is an
explicit, broader alternative. It cannot be combined with trustee fields.
The trustee controls the Qumulo share ACL; the node-publish Secret controls
the client identity used to mount the share. Configure them to describe the
same intended access boundary.

The bundled StorageClasses use directory mode `0777` because the CSIDriver's
`fsGroupPolicy` is `None` and arbitrary pod UIDs otherwise cannot write a
new, administrator-owned root. Host/network restrictions, NFS root squash,
SMB share ACLs, and SMB credentials remain the primary access boundaries.
Use a narrower mode when workload identities and permissions are managed
consistently.

## Install with Kustomize

Before creating Kubernetes objects, complete the Qumulo service-user and
filesystem setup in [qumulo-setup.md](qumulo-setup.md): create
`/k8s-volumes` as an administrator and grant the controller identity effective
and inheritable access below it. The least-privilege role cannot create this
top-level root from `/` by itself.

1. Edit [`deploy/csi/configmap.yaml`](../deploy/csi/configmap.yaml) and set:
   `QUMULO_ENDPOINT`, `QUMULO_DATA_SERVER`, and a restricted
   `QUMULO_CSI_ALLOWED_NETWORKS` value.
2. Edit [`deploy/csi/storageclasses.yaml`](../deploy/csi/storageclasses.yaml).
   Replace the example networks, and configure an SMB trustee or explicitly
   enable `allowAllSMBUsers`.
3. If the release image is not pullable from GHCR, point the standalone
   Kustomization at the registry you published in the main install guide:

   ```bash
   cd deploy/csi
   kustomize edit set image \
     ghcr.io/rsnedigar-cell/cosi-driver-qumulo="${REPO}:0.2.0"
   cd ../..
   ```

   You can edit `deploy/csi/kustomization.yaml` directly instead.
4. Create the three Secrets described above. If only NFS is enabled, the SMB
   Secret is not needed.
5. Confirm `/var/lib/kubelet` is the kubelet root on every node. If it is not,
   update the host paths, node `--kubelet-root`, and registrar
   `--kubelet-registration-path` together.
6. Render before applying:

```bash
kubectl kustomize deploy/csi
kubectl apply -k deploy/csi
kubectl -n qumulo-cosi rollout status deployment/qumulo-csi-controller
kubectl -n qumulo-cosi rollout status daemonset/qumulo-csi-node
```

The complete manifest notes are in
[`deploy/csi/README.md`](../deploy/csi/README.md).

## Install with Helm

CSI is disabled by default in the chart. Create the Secrets, then enable it
with an endpoint, a data server, at least one restricted network, and an SMB
mount Secret:

```bash
helm upgrade --install qumulo-cosi deploy/helm/cosi-driver-qumulo \
  --namespace qumulo-cosi \
  --create-namespace \
  --set image.tag=0.2.0 \
  --set csi.enabled=true \
  --set qumulo.endpoint=qumulo-rest.example.com \
  --set csi.dataServer=qumulo-data.example.com \
  --set-string 'csi.allowedNetworks[0]=10.20.0.0/16' \
  --set csi.storageClasses.smb.smbTrusteeDomain=LOCAL \
  --set csi.storageClasses.smb.smbTrusteeName=k8s-smb \
  --set csi.storageClasses.smb.credentialsSecret=qumulo-smb-credentials
```

If this command upgrades a release installed from ECR or another private
mirror, repeat its `image.repository`, `image.tag`, and any `image.digest`
override, or pass the same values file again. Do not rely on an earlier
one-off `--set image.repository=...` remaining in effect when it is omitted
from an upgrade. Use `--reuse-values` only when retaining every previous
release override is intentional.

The chart uses `qumulo.existingSecret` for the controller REST credential and
`csi.handleKeySecret` for the handle key. The defaults are
`qumulo-cosi-creds` and `qumulo-csi-handle-key`. See the chart
[`values.yaml`](../deploy/helm/cosi-driver-qumulo/values.yaml) and
[`README.md`](../deploy/helm/cosi-driver-qumulo/README.md).

## StorageClass parameters

All values are strings in Kubernetes `StorageClass.parameters`. Boolean
values are parsed strictly; misspellings fail provisioning.

### Common parameters

| Parameter | Default | Notes |
|---|---|---|
| `protocol` | none | Required: `nfs` or `smb`. |
| `endpoint` | process endpoint | Optional, but when set it must match the controller's configured endpoint. Arbitrary per-volume endpoints are rejected. |
| `basePath` | `/k8s-volumes` | Absolute, non-root Qumulo path. Directories are placed below `<basePath>/<protocol>/`. |
| `allowedNetworks` | process default | Comma- or semicolon-separated client network rules. Required unless `allowAllHosts=true`. |
| `allowAllHosts` | `false` | Omits protocol network restrictions. Use only for an intentionally unrestricted storage network. |
| `directoryMode` | `0777` | Four-digit octal mode from `0000` through `0777`. |
| `quotaEnabled` | `true` | Creates a directory quota using the requested PVC capacity. Keep enabled for expandable volumes; expansion is rejected when quota enforcement was disabled at creation. |
| `deleteData` | `true` | After deleting the export/share, delete the backing directory by immutable file ID. Set `false` to retain data. |
| `tenantID` | unsupported | Tenant-aware preview APIs are deliberately rejected; this release uses stable Qumulo v2 NFS/SMB APIs. |

### NFS parameters

| Parameter | Default | Notes |
|---|---|---|
| `nfsVersion` | `4.1` | Supported values: `3`, `4.1`. The node controls the mount version and uses `sec=sys`. **`4.1` requires cluster-side NFSv4 enablement first** — Qumulo Core ships with v4 disabled, and enabling it conflicts with the factory `/` export; see [qumulo-setup.md](qumulo-setup.md#csi-file-protocol-setup) before using the default. `3` needs no cluster preparation (mounted with `nolock`). |
| `nfsExportPrefix` | `/k8s` | Absolute Qumulo NFS export-path prefix. |
| `nfsRequirePrivilegedPort` | `false` | Require clients to use a privileged source port. |
| `nfsRootSquash` | `true` | Maps root to the configured anonymous UID/GID. |
| `nfsAnonymousUID` | `65534` | Unsigned 32-bit UID used by root squash. |
| `nfsAnonymousGID` | `65534` | Unsigned 32-bit GID used by root squash. |

### SMB parameters

| Parameter | Default | Notes |
|---|---|---|
| `smbRequireEncryption` | `true` | Requires share encryption; node mounts use SMB 3.1.1 and `seal`. |
| `smbAccessBasedEnumeration` | `true` | Hides entries the SMB identity cannot access. |
| `smbTrusteeAuthID` | none | Qumulo immutable authentication ID for the allowed share trustee. Mutually exclusive with domain/name. |
| `smbTrusteeDomain` | none | Trustee domain; requires `smbTrusteeName`. |
| `smbTrusteeName` | none | Trustee name; requires `smbTrusteeDomain`. |
| `allowAllSMBUsers` | `false` | Uses `WORLD/Everyone`; cannot be combined with trustee parameters. |
| `csi.storage.k8s.io/node-publish-secret-name` | none | Required for SMB: Secret containing `username`, `password`, and optional `domain`. |
| `csi.storage.k8s.io/node-publish-secret-namespace` | none | Namespace containing the SMB mount Secret. |

Unknown StorageClass parameters are rejected so a misspelled deletion or
security setting cannot silently take its default. `basePath` and
`nfsExportPrefix` must be clean, absolute, non-root paths without control
characters. The exact original parameter set is part of the durable create
identity; changing explicit settings under an existing CSI name returns
`AlreadyExists` rather than mutating that volume.

StorageClass-supplied mount flags may add ordinary NFS/CIFS options, but the
driver rejects credentials, bind/remount options, protocol downgrades, and
other options that would override its security controls. SMB is always
mounted as 3.1.1. NFS is mounted as the version recorded in the signed
handle. Both use `nodev` and `nosuid`.

## Provisioning, expansion, and deletion

For each CSI volume, the controller:

1. Derives a deterministic resource name from the CSI request name.
2. Creates or reconciles a Qumulo directory under
   `<basePath>/<protocol>/<resource-name>`.
3. Creates a directory quota at the PVC's requested capacity when enabled.
4. Creates or reconciles an NFS export or SMB share without ever retargeting
   an existing protocol resource to another directory.
5. Returns a signed, endpoint-bound volume handle to Kubernetes.

Retries are idempotent. A name already used with a conflicting protocol,
export path, or backing directory fails instead of being silently reused.
Online expansion grows the directory quota and never shrinks an existing
quota. The controller reports the actual committed quota when it is already
larger than the request. A volume created with `quotaEnabled: "false"` fails
expansion clearly because the requested capacity could not be enforced.
Network filesystems do not need node-side filesystem expansion.

With `reclaimPolicy: Delete`, `DeleteVolume` first removes the export/share.
When the signed handle records `deleteData: true`, it then verifies that the
path still refers to the original immutable Qumulo directory ID before
queuing tree deletion. If the path has been replaced, deletion fails safely.
With `deleteData: false`, the directory and its data remain after the
protocol resource is removed. `reclaimPolicy: Retain` prevents Kubernetes
from asking the driver to delete the volume at all.

Snapshots, cloning, raw block volumes, Windows node mounting, tenant-aware
preview APIs, and volume shrinking are not supported in this release.

## Network requirements

| Source | Destination | Traffic |
|---|---|---|
| CSI controller | Qumulo REST endpoint | HTTPS, normally TCP 8000 |
| Linux CSI nodes | Qumulo data server | NFS 4.1 on TCP 2049, or the site's NFS 3 service ports |
| Linux CSI nodes | Qumulo data server | SMB on TCP 445 |

The REST endpoint and data server may be different names, but both are
process-level configuration. A PV cannot substitute arbitrary hosts in its
signed handle. Configure firewalls and Qumulo host/network restrictions for
the worker-node addresses that actually originate mount traffic.

## Uninstall safely

Resolve every PVC/PV first. For `Delete` volumes, wait for Kubernetes and the
Qumulo tree-delete job to finish; for data you are retaining, record the
backing directory and share/export before removing the PV object. Only then
remove the standalone stack with `kubectl delete -k deploy/csi` or disable it
through the Helm release. Preserve `qumulo-csi-handle-key`, the controller
credential, and any SMB credential while even one CSI PV remains. Never delete
the shared `qumulo-cosi` namespace as a cleanup shortcut while CSI volumes
exist.

## Verify

```bash
kubectl get csidriver file.qumulo.com
kubectl get storageclass qumulo-nfs qumulo-smb
kubectl apply -f deploy/csi/example-pvcs.yaml
kubectl get pvc,pv
```

The repository contains an opt-in live Qumulo CSI control-plane integration
test, but it is not run by the default unit suite. In this release's
development environment, unit/API-client tests passed; the CSI live test and
Linux NFS/SMB mount path were not live-verified. Validate both protocols with
representative pods and your actual identity, network, and failover design
before production rollout.
