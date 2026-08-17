# Helm chart: cosi-driver-qumulo

Installs the Qumulo COSI driver (`s3.qumulo.com`) and the official COSI
v1alpha1 sidecar. The sidecar uses a Kubernetes Lease automatically, so
`replicaCount` can be increased for active/passive high availability.

This chart does **not** install the COSI CRDs or controller, and it does **not**
create the Qumulo credentials Secret.

Full steps (including Amazon EKS and ECR): [docs/install.md](../../../docs/install.md).

```bash
# 1. COSI v0.2.2
kubectl apply -k github.com/kubernetes-sigs/container-object-storage-interface?ref=v0.2.2

# 2. Credentials
kubectl create namespace qumulo-cosi
kubectl create secret generic qumulo-cosi-creds \
  -n qumulo-cosi \
  --from-literal=token='...' \
  --from-file=ca.crt=./qumulo-ca.crt

# 3. Driver
helm install qumulo-cosi . \
  --namespace qumulo-cosi \
  --set image.repository=ACCOUNT.dkr.ecr.REGION.amazonaws.com/cosi-driver-qumulo \
  --set image.tag=0.2.0 \
  --set qumulo.endpoint=qumulo.example.internal
```

REST TLS verification is enabled by default. Include the CA that issued the
Qumulo certificate, or use a publicly trusted certificate. The lab-only
escape hatch is `--set csi.insecureSkipTLSVerify=true` (and the corresponding
COSI class setting when COSI is also used).

Before enabling the bundled SMB StorageClass, create its separate data-plane
login with `qq auth_add_user --name k8s-smb --password` (interactive prompt),
then put that same password in `qumulo-smb-credentials`. An existing AD/LDAP
identity is also supported when its StorageClass trustee and login domain are
configured together; it must not inherit the controller REST role.

`qumulo.endpoint` is required for COSI and CSI. A BucketClass endpoint must
match it, or the class may omit `endpoint` to inherit the process setting.

Metrics and health checks are enabled on port `8081` by default. Change them
together with `--set metrics.port=9090`, or disable the metrics Service and
HTTP probes with `--set metrics.enabled=false`. For immutable deployments,
set `image.digest=sha256:...`; a digest takes precedence over `image.tag`.

## NFS and SMB CSI

The same image also contains the `file.qumulo.com` CSI driver and the NFS/CIFS
mount helpers. CSI is off by default because its controller must be bound to a
specific Qumulo endpoint and a restricted set of client networks. To enable
both StorageClasses, create an SMB mount credential Secret and supply those
settings explicitly:

```bash
kubectl create secret generic qumulo-smb-credentials \
  -n qumulo-cosi \
  --from-literal=username='k8s-smb' \
  --from-literal=password='...'

openssl rand -base64 32 | kubectl create secret generic qumulo-csi-handle-key \
  -n qumulo-cosi \
  --from-file=key=/dev/stdin

helm upgrade --install qumulo-cosi . \
  --namespace qumulo-cosi \
  --set image.repository=ACCOUNT.dkr.ecr.REGION.amazonaws.com/cosi-driver-qumulo \
  --set image.tag=0.2.0 \
  --set csi.enabled=true \
  --set qumulo.endpoint=qumulo.example.com \
  --set csi.dataServer=qumulo-data.example.com \
  --set-string 'csi.allowedNetworks[0]=10.0.0.0/8' \
  --set csi.storageClasses.smb.credentialsSecret=qumulo-smb-credentials
```

The image settings are repeated deliberately. When upgrading a release that
uses ECR or another private mirror, repeat its `image.repository`, `image.tag`,
and any `image.digest` override, or pass the same values file again. Do not
rely on an earlier one-off `--set image.repository=...` when it is omitted
from an upgrade. Use `--reuse-values` only when retaining every previous
release override is intentional.

This adds two leader-elected controller replicas, a privileged Linux node
DaemonSet with host mount propagation, `qumulo-nfs` and `qumulo-smb`
StorageClasses, and online quota expansion. The controller alone receives the
Qumulo REST credential mount. Automatic service-account token mounts are
disabled: only the provisioner and resizer receive an explicitly projected,
short-lived Kubernetes API token. The Qumulo driver, liveness containers, and
node pods receive no Kubernetes API token. SMB credentials are passed by
kubelet only for the requested node publish operation.

The handle-key Secret is deliberately separate from both credential Secrets.
Every CSI controller and node replica must share it because it authenticates
opaque volume IDs; back it up and restore the same key during disaster
recovery. Rotating it without a migration would invalidate existing PVs.

The StorageClass trustee `LOCAL/k8s-smb` is a Qumulo REST identity for the
share ACL; `LOCAL` is not the SMB client login domain. The mount Secret above
therefore omits `domain`. If authentication requires one, add the actual
cluster/NetBIOS or directory domain to that Secret.

The StorageClasses use mode `0777` so arbitrary pod UIDs can write newly
created volume roots (`fsGroupPolicy` is `None`). Access is still constrained
by the configured NFS host rules or SMB share policy and credentials. Override
the mode when your workload has a known identity model.

NFS exports enable root squashing by default and map root to anonymous UID/GID
`65534`. The SMB class grants its share to the example Qumulo trustee
`LOCAL/k8s-smb`; create that identity on Qumulo or replace
`smbTrusteeDomain`/`smbTrusteeName` with your intended trustee. You can instead
set `smbTrusteeAuthID`, which takes precedence over the name pair. The
`allowAllSMBUsers=true` escape hatch deliberately grants `WORLD/Everyone` and
should be used only when a shared identity boundary is intentional.

The default `reclaimPolicy: Delete` and `deleteData: true` remove the Qumulo
share/export and its directory when a PV is released. For preservation-first
workloads, set both StorageClasses to `reclaimPolicy=Retain`; alternatively,
set their `deleteData=false` values to remove only the protocol resource.
