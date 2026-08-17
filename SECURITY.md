# Security

## Reporting

Report vulnerabilities privately through GitHub Security Advisories on this
repository. Do not open a public issue for an unreleased flaw, and do not
paste access tokens, passwords, or `BucketInfo` secrets into issues or
pull requests.

## What this driver stores

- Cluster credentials live in a Kubernetes Secret (`token` preferred). The driver process reads them; they are never written back, never placed in `bucket_id` / `account_id`, and never logged.
- Application S3 secrets are minted at Grant time. Qumulo shows the secret access key **once**. A retried Grant rotates keys so the sidecar always receives a usable pair.
- TLS to the REST API is verified by default. `insecureSkipTLSVerify` is an explicit escape hatch that logs a warning. `http.DefaultTransport` is never mutated.

## Privilege

The driver service account on Qumulo should use the least-privilege role and
path-scoped base-directory ACLs in `docs/qumulo-setup.md`. Tree deletion needs
both `PRIVILEGE_FS_DELETE_TREE_READ` and `_WRITE` so the driver can poll the
job it submitted. Qumulo's tree-delete WRITE privilege is not path scoped: it
bypasses filesystem permissions and can delete any file or directory on the
cluster. Treat the controller token as a destructive cluster-wide credential,
or disable COSI purge and CSI data deletion and remove both tree-delete
privileges from a tailored role. Safe bucket deletion also needs
`PRIVILEGE_S3_UPLOADS_READ` and `_WRITE` to list and abort multipart uploads.
The connection preflight requires `PRIVILEGE_S3_SETTINGS_READ`. Grant writes
a filesystem ACE on the bucket root.

## COSI IAM

`authenticationType: IAM` is rejected. This driver ships local users + access keys only.
