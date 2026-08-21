# Qumulo NFS and SMB CSI deployment

This directory installs the `file.qumulo.com` CSI controller, a privileged
Linux node plug-in, digest-pinned Kubernetes CSI sidecars, and separate NFS
and SMB StorageClasses. The node image contains `mount.nfs` and `mount.cifs`.

First complete the Qumulo setup in
[`docs/qumulo-setup.md`](../../docs/qumulo-setup.md). In particular, create
`/k8s-volumes` as an administrator and grant the controller service user
effective and inheritable access below that path. The least-privilege role
cannot create the top-level root from `/` by itself.

Volume snapshots are optional. Install the Kubernetes snapshot CRDs first,
then apply `deploy/csi/snapshots` or set Helm `csi.snapshots.enabled=true`.
See `docs/csi.md`.

Before applying it:

```bash
kubectl apply -f deploy/csi/namespace.yaml
```

1. Replace the endpoint, data server, and allowed client networks in
   `configmap.yaml`. Do not use an unrestricted network unless that exposure
   is intentional.
2. Create `qumulo-cosi-creds` in `qumulo-cosi` with the Qumulo REST token used
   by the controller.

   ```bash
   kubectl create secret generic qumulo-cosi-creds \
     -n qumulo-cosi \
     --from-literal=token='...' \
     --from-file=ca.crt=./qumulo-ca.crt
   ```

   TLS verification is enabled. Include the CA that issued the Qumulo REST
   certificate (the factory certificate is self-signed), or install a
   publicly trusted certificate. For a lab only, you may omit `ca.crt` and set
   `QUMULO_INSECURE_SKIP_TLS_VERIFY: "true"` in `configmap.yaml`.
3. Create `qumulo-smb-credentials` in `qumulo-cosi` with `username` and
   `password` keys for node-side SMB mounts. Create the example Qumulo trustee
   `LOCAL/k8s-smb` (or replace it in `storageclasses.yaml` with the intended
   domain/name or auth ID). NFS does not use this Secret.

   ```bash
   # On an administrative qq client; enter the password at the secure prompt.
   qq auth_add_user --name k8s-smb --password

   kubectl create secret generic qumulo-smb-credentials \
     -n qumulo-cosi \
     --from-literal=username='k8s-smb' \
     --from-literal=password='...'
   ```

   `LOCAL/k8s-smb` is the Qumulo REST trustee used on the share ACL; `LOCAL` is
   not the SMB client login domain. The example therefore leaves the mount
   credential unqualified. If authentication requires a domain, add the actual
   cluster/NetBIOS or directory domain to the Secret. An existing AD/LDAP
   identity may be used instead when the StorageClass trustee and Secret are
   updated together. Do not grant the controller REST role to this SMB-only
   user.
4. Create the dedicated volume-handle authentication key. It must remain
   stable and be shared by every controller and node replica:

   ```bash
   openssl rand -base64 32 | kubectl create secret generic qumulo-csi-handle-key \
     -n qumulo-cosi \
     --from-file=key=/dev/stdin
   ```

   Back this Secret up. Replacing it without migrating existing PV handles
   makes those handles unverifiable.
5. Confirm `/var/lib/kubelet` is the kubelet root on every Linux worker. Change
   the host paths and registrar path together when a distribution uses a
   different root.
6. If the release image is not pullable from GHCR, rewrite the image to the
   registry where you published it (or edit `kustomization.yaml` directly):

   ```bash
   cd deploy/csi
   kustomize edit set image \
     ghcr.io/rsnedigar-cell/cosi-driver-qumulo="${REPO}:0.2.0"
   cd ../..
   ```

Then render and apply:

```bash
kubectl kustomize deploy/csi
kubectl apply -k deploy/csi
```

The node DaemonSet is intentionally privileged: CSI mount operations must run
in the host's shared kubelet mount tree. Its API token is disabled and it does
not receive the Qumulo REST administration Secret. The controller mounts that
Secret directly. Automatic token mounts are disabled there too: only the
provisioner and resizer receive an explicitly projected, short-lived API token;
the Qumulo driver and liveness container do not. API permissions are limited to
provisioning, expansion, events, and a namespace-scoped leader-election Lease.

SMB mount credentials are written only to a size-limited in-memory volume for
the duration of the mount call and are never stored in the host filesystem.

Both StorageClasses permit online quota expansion and use mode `0777` because
`fsGroupPolicy` is `None` and arbitrary pod UIDs otherwise cannot write a new
admin-owned root. Network/export/share restrictions still form the access
boundary; set a narrower mode when workload identities are known.

NFS exports enable root squashing and anonymous UID/GID `65534`. SMB shares
grant access to `LOCAL/k8s-smb` rather than `WORLD/Everyone`; replace that
example trustee with an identity present on your Qumulo cluster. Broad SMB
access requires the explicit `allowAllSMBUsers: "true"` parameter.

The included StorageClasses use `reclaimPolicy: Delete` and `deleteData:
"true"`, so releasing a PV deletes its share/export and backing directory.
Change both classes to `reclaimPolicy: Retain` before use when data must be
preserved independently of the PVC lifecycle.
