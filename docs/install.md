# Install

This guide is the supported way to run **cosi-driver-qumulo** on Kubernetes.
The production path below is **Amazon EKS** talking to a Qumulo cluster that
exposes the REST API (TCP **8000**) and the S3 API (TCP **9000**). The same
steps work on any Kubernetes 1.25+ cluster that can reach those ports.

COSI is a Kubernetes **alpha** API (`objectstorage.k8s.io/v1alpha1`). This
walkthrough covers the `s3.qumulo.com` COSI service. The same project also
ships the separate `file.qumulo.com` CSI service for dynamic NFS and SMB;
follow [csi.md](csi.md) for that stack.

## What you need

| Piece | Why |
|---|---|
| A Kubernetes cluster (EKS is the path documented here) | Runs the COSI controller and this driver |
| A Qumulo cluster on Core **7.2+** (exercised on **7.9.2.x**) | Creates buckets, users, keys, and policies |
| Network path from **driver pods → Qumulo :8000** | Control plane (REST) |
| Network path from **application pods → Qumulo :9000** | Data plane (S3) |
| `kubectl` pointed at the cluster | Install and operate |
| Docker (or another OCI builder) | Build the driver image if you cannot pull GHCR |
| A Qumulo access token for the driver | Authenticate REST calls |

Workstation tools for the AWS path: [AWS CLI v2](https://docs.aws.amazon.com/cli/), Docker, and `kubectl`. Helm 3 is optional.

CI publishes the 0.2.0 multi-architecture image (**`linux/amd64` and
`linux/arm64`**). The
arm64 build is cross-compiled and has not yet been exercised on ARM
hardware; treat Graviton as untested rather than unsupported.

## How the pieces fit

```
EKS (or any Kubernetes)                         Qumulo
┌─────────────────────────────┐                 ┌──────────────────────────┐
│ COSI controller             │                 │ REST API  :8000  HTTPS   │
│                             │   TCP 8000      │  buckets, users, keys,   │
│ qumulo-cosi-driver + sidecar┼────────────────►│  policies, directory ACE │
│                             │                 │                          │
│ application pod (boto3/CLI) ┼────────────────►│ S3 API    :9000  HTTPS   │
└─────────────────────────────┘   TCP 9000      └──────────────────────────┘
        BucketClaim / BucketAccess
                │
                ▼
        Secret (BucketInfo)  →  path-style SigV4 client
```

The driver never proxies object data. Apps talk to Qumulo S3 directly.

## 1. Network (do this first on AWS)

Put EKS and Qumulo in the **same VPC**, or peer / connect the VPCs (Transit
Gateway, Direct Connect, or a VPN to an on-prem cluster). DNS names you put
in `BucketClass` must resolve **inside the cluster**, not only on your laptop.

Open security groups (or Network ACLs) so that:

| Source | Destination | Port | Purpose |
|---|---|---|---|
| EKS node security group (or pod security group) | Qumulo node / NLB security group | TCP **8000** | Driver REST |
| EKS node security group (or pod security group) | Qumulo node / NLB security group | TCP **9000** | Application S3 |

Qumulo Core listens for S3 on **9000**. That port is not configurable.

Point `kubectl` at EKS:

```bash
aws eks update-kubeconfig --name CLUSTER --region REGION
kubectl get nodes
```

## 2. Get the source

```bash
git clone https://github.com/rsnedigar-cell/cosi-driver-qumulo.git
cd cosi-driver-qumulo
```

## 3. Prepare Qumulo

On the cluster, as an administrator:

```bash
qq s3_modify_settings --enable
qq fs_create_dir --path / --name k8s-buckets
# Also required when installing the CSI NFS/SMB stack:
qq fs_create_dir --path / --name k8s-volumes
```

Create a least-privilege role and service user, then mint a long-lived access
token. Full privilege list: [qumulo-setup.md](qumulo-setup.md). Helper:

```bash
# from a host that can run qq against the cluster
./hack/create-driver-role.sh cosi-driver cosi-driver-role
qq auth_create_access_token local:cosi-driver
```

Keep the token. You will put it in a Kubernetes Secret in step 6.

A self-signed management certificate is the factory default. For a first
bring-up you can set `insecureSkipTLSVerify: "true"` on the `BucketClass`.
For production, install a real certificate (or mount `ca.crt` in the
credentials Secret) and leave TLS verification on.

## 4. Get the image

The published images are the default:

```bash
docker pull ghcr.io/rsnedigar-cell/cosi-driver-qumulo:0.2.0        # COSI (distroless)
docker pull ghcr.io/rsnedigar-cell/cosi-driver-qumulo:0.2.0-csi    # CSI (Alpine + mount helpers)
```

For air-gapped clusters, private registries, or source-verified builds,
build and push to Amazon ECR (or any registry) instead:

```bash
export AWS_REGION=us-east-1
export ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
export REPO="${ACCOUNT}.dkr.ecr.${AWS_REGION}.amazonaws.com/cosi-driver-qumulo"

aws ecr describe-repositories --repository-names cosi-driver-qumulo --region "$AWS_REGION" \
  >/dev/null 2>&1 || aws ecr create-repository --repository-name cosi-driver-qumulo --region "$AWS_REGION"

aws ecr get-login-password --region "$AWS_REGION" \
  | docker login --username AWS --password-stdin "${ACCOUNT}.dkr.ecr.${AWS_REGION}.amazonaws.com"

docker build --platform linux/amd64 --build-arg VERSION=0.2.0 -t "${REPO}:0.2.0" .
docker push "${REPO}:0.2.0"
```

Same-account EKS node roles can pull this image with the usual
`AmazonEC2ContainerRegistryReadOnly` policy. Cross-account ECR needs an
explicit repository policy or an `imagePullSecret`. When you build your own
image, point the manifests at your registry (`kustomize edit set image` or
the Helm `image.repository` value) and remember the CSI components use the
`-csi` build target/tag.

## 5. Install the COSI controller

```bash
kubectl apply -k github.com/kubernetes-sigs/container-object-storage-interface?ref=v0.2.2
kubectl -n container-object-storage-system rollout status deploy/container-object-storage-controller --timeout=180s
```

This installs the `objectstorage.k8s.io` CRDs and the v0.2.2 controller.
Confirm:

```bash
kubectl get crd | grep objectstorage.k8s.io
```

You should see `buckets`, `bucketclaims`, `bucketclasses`, `bucketaccesses`,
and `bucketaccessclasses`.

## 6. Store the Qumulo token

Create the namespace and Secret **before** deploying the driver. The
Deployment mounts `qumulo-cosi-creds`; without it the pod will not start.

Create the namespace, then choose exactly one Secret command. For the factory
self-signed certificate or another private CA, use the production form with
`ca.crt`:

```bash
kubectl create namespace qumulo-cosi

kubectl create secret generic qumulo-cosi-creds \
  --namespace qumulo-cosi \
  --from-literal=token='PASTE_THE_QUMULO_ACCESS_TOKEN' \
  --from-file=ca.crt=./qumulo-ca.crt
```

If the Qumulo REST certificate is already trusted by the image, omit the CA:

```bash
kubectl create secret generic qumulo-cosi-creds \
  --namespace qumulo-cosi \
  --from-literal=token='PASTE_THE_QUMULO_ACCESS_TOKEN'
```

Username and password are accepted as a fallback (`username` + `password`
keys). Prefer a token.

## 7. Install the driver

### Kustomize (default)

Point the manifest at your ECR image, replace `qumulo.example.internal` in
`deploy/kustomize/deployment.yaml` with the REST hostname reachable by the
pod, then apply:

```bash
# requires the kustomize CLI (or edit deploy/kustomize/deployment.yaml by hand)
cd deploy/kustomize
kustomize edit set image \
  ghcr.io/rsnedigar-cell/cosi-driver-qumulo="${REPO}:0.2.0"
cd ../..

kubectl apply -k deploy/kustomize
kubectl -n qumulo-cosi rollout status deploy/qumulo-cosi-driver --timeout=180s
```

You should see **2/2** containers ready (sidecar + driver).

### Helm

```bash
helm install qumulo-cosi deploy/helm/cosi-driver-qumulo \
  --namespace qumulo-cosi \
  --create-namespace \
  --set image.repository="${ACCOUNT}.dkr.ecr.${AWS_REGION}.amazonaws.com/cosi-driver-qumulo" \
  --set image.tag=0.2.0 \
  --set qumulo.endpoint="qumulo.apps.internal"
```

Helm does not create the credentials Secret. Complete step 6 first.

### Add NFS and SMB CSI

The release image also contains `/qumulo-csi-driver` and the NFS/CIFS mount
helpers. The Helm chart keeps CSI disabled until the endpoint, client
networks, handle-signing key, SMB trustee, and SMB node credentials are
configured. Enable it with `csi.enabled=true`, or install the standalone
[`deploy/csi`](../deploy/csi) manifests. Complete instructions and parameter
tables: [csi.md](csi.md).

## 8. Create the classes

Copy [deploy/examples/bucketclass.yaml](../deploy/examples/bucketclass.yaml).
Set `parameters.endpoint` to the same hostname or IP configured on the driver
process, or omit it to inherit the process endpoint. Do not put the port in
`endpoint`; use `restPort` / `s3Port`. A mismatch is rejected rather than
sending controller credentials to another cluster.

```bash
# example: edit endpoint, then
kubectl apply -f deploy/examples/bucketclass.yaml
```

Minimum fields that must be correct:

| Field | Value |
|---|---|
| `driverName` | `s3.qumulo.com` |
| `parameters.endpoint` | Qumulo REST host, e.g. `qumulo.apps.internal` |
| `parameters.restPort` | `8000` unless you front REST on another port |
| `parameters.credentialsSecretName` | Omit with the standard manifests; the driver reads the mounted `qumulo-cosi-creds` Secret |
| `parameters.credentialsSecretNamespace` | Omit with the standard manifests |
| `parameters.insecureSkipTLSVerify` | `"true"` only for a lab / self-signed cert |
| `authenticationType` (access class) | `Key` |

Leave `versioning` unset until you need it. All parameters:
[parameters.md](parameters.md).

## 9. Claim a bucket and grant access

```bash
kubectl create namespace app
kubectl apply -f deploy/examples/claim.yaml
```

Wait until both objects are ready:

```bash
kubectl get bucketclaim,bucketaccess,secret -n app
kubectl get bucket
```

A `Bucket` should appear cluster-wide. `BucketAccess` should become granted
and Secret `photos-s3-creds` should exist in `app`.

If either object sits pending, the driver logs are the next stop:

```bash
kubectl logs -n qumulo-cosi deploy/qumulo-cosi-driver -c qumulo-cosi-driver --tail=200
```

## 10. Run a workload

Applications must use **AWS Signature Version 4**, **path-style** addressing,
and must **not** send default AWS request-trailer checksums.

The tested smoke job is [deploy/examples/job-boto3.yaml](../deploy/examples/job-boto3.yaml):

```bash
kubectl apply -f deploy/examples/job-boto3.yaml
kubectl -n app wait --for=condition=complete job/photos-put-get --timeout=180s
kubectl -n app logs job/photos-put-get
```

A successful run prints `PUT/GET ok True`.

For your own code, mount Secret `photos-s3-creds` and read
`/cosi/BucketInfo`. The sidecar writes a COSI `BucketInfo` document. The S3
block is `spec.secretS3`:

| Key | Meaning |
|---|---|
| `accessKeyID` | Qumulo S3 access key |
| `accessSecretKey` | Shown once at grant time |
| `endpoint` | `https://<host>:9000` |
| `region` | Class `region` (Qumulo accepts any string) |

boto3 sketch (same settings as the smoke job):

```python
import boto3
from botocore.config import Config

client = boto3.client(
    "s3",
    endpoint_url=secret["endpoint"],
    aws_access_key_id=secret["accessKeyID"],
    aws_secret_access_key=secret["accessSecretKey"],
    region_name=secret.get("region") or "us-east-1",
    config=Config(
        s3={"addressing_style": "path"},
        signature_version="s3v4",
        request_checksum_calculation="when_required",
        response_checksum_validation="when_required",
    ),
)
```

AWS CLI equivalent:

```bash
export AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED
export AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED
aws --endpoint-url "$ENDPOINT" s3 --no-verify-ssl ls "s3://$BUCKET"
# drop --no-verify-ssl once the cluster cert is trusted
```

Configure path-style addressing for the profile
(`aws configure set s3.addressing_style path`).

## Verify

| Check | Expect |
|---|---|
| `kubectl get pods -n container-object-storage-system` | Controller Running |
| `kubectl get pods -n qumulo-cosi` | `qumulo-cosi-driver` **2/2** |
| `kubectl get bucketclass qumulo-gold` | Exists, `driverName: s3.qumulo.com` |
| `kubectl get bucketclaim -n app` | Bound / Ready |
| `kubectl get bucketaccess -n app` | Granted |
| `kubectl get secret photos-s3-creds -n app` | Present |
| Smoke job | `PUT/GET ok True` |

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `ImagePullBackOff` | GHCR/ECR image is private, the tag is wrong, or a locally built image does not match the node architecture |
| `CreateContainerConfigError` / missing volume | Secret `qumulo-cosi-creds` was not created in `qumulo-cosi` |
| `BucketClaim` never Ready; driver logs show dial/timeout | Security group or routing: pods cannot reach Qumulo **:8000** |
| `x509: certificate signed by unknown authority` | Set `insecureSkipTLSVerify: "true"` for a lab cert, or add `ca.crt` to the Secret |
| `UNAUTHENTICATED` / 401 | Token is wrong, expired, or the user lacks privileges |
| `BucketAccess` never granted | Same as above; also check that S3 is enabled and `/k8s-buckets` exists |
| App: `QumuloUnsupportedTrailerChecksumFormat` | SDK checksum trailers; set `when_required` / `WHEN_REQUIRED` |
| App: `AuthorizationHeaderMalformed` | Client used SigV2; force SigV4 |
| App: 403 on PUT after a successful Grant | Expected without a directory ACE; this driver fails the Grant unless it can install the ACE. Check driver logs for ACL errors |
| App: connection timeout to `:9000` | Security group: application pods need **9000**, not only the driver |
| Endpoint in the Secret is a name that does not resolve in the pod | `parameters.endpoint` must be cluster-reachable DNS or an IP |

## Uninstall

Deleting a `BucketClaim` with `deletionPolicy: Delete` asks the driver to
remove the Qumulo bucket (see deletion flags in [parameters.md](parameters.md)).
`Retain` leaves the bucket in place.

**CSI safety precondition:** if NFS/SMB CSI was ever enabled, first delete or
retain every PV according to your data policy and back up
`qumulo-csi-handle-key`. Do not delete the namespace, handle key, controller
credential, or SMB credential while any CSI PV exists. Losing the handle key
makes every existing signed volume handle unverifiable. The namespace deletion
below is only for a COSI-only installation; otherwise omit it and follow the
CSI lifecycle guidance in [csi.md](csi.md).

```bash
kubectl delete -f deploy/examples/claim.yaml --ignore-not-found
kubectl delete -f deploy/examples/bucketclass.yaml --ignore-not-found
kubectl delete -k deploy/kustomize --ignore-not-found
# helm uninstall qumulo-cosi -n qumulo-cosi
# COSI-only installation only (never while CSI PVs exist):
kubectl delete secret qumulo-cosi-creds -n qumulo-cosi --ignore-not-found
kubectl delete namespace qumulo-cosi --ignore-not-found
# optional: remove COSI itself
# kubectl delete -k github.com/kubernetes-sigs/container-object-storage-interface?ref=v0.2.2
```

## Laptop lab (kind)

The same manifests work on kind. Build the image, load it into kind instead
of pushing to ECR, and set `parameters.endpoint` to an address the **kind
node** can reach (not `localhost` on the host):

```bash
docker build --platform linux/amd64 --build-arg VERSION=0.2.0 -t ghcr.io/rsnedigar-cell/cosi-driver-qumulo:0.2.0 .
kind load docker-image ghcr.io/rsnedigar-cell/cosi-driver-qumulo:0.2.0 --name cosi
# then steps 5–10
```
