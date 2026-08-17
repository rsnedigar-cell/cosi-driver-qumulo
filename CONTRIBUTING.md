# Contributing

## Scope

This repository contains the Qumulo S3 COSI driver (`s3.qumulo.com`) and the
separate Qumulo NFS/SMB CSI driver (`file.qumulo.com`). Contributions to both
drivers are in scope. Keep their interfaces separate: object-storage changes
belong in the COSI service, while NFS and SMB volume changes belong in the CSI
service.

## Develop

You need Go 1.26.6+ and Docker.

```bash
make test
make vet
make build
```

Integration tests talk to a real cluster and are opt-in:

```bash
QUMULO_HOST=https://cluster:8000 QUMULO_TOKEN=... make integration
```

Live Core 7.9.2 behaviors that look like bugs are listed in
[docs/status.md](docs/status.md). Do not “fix” those without re-testing
against a real cluster.

## Docs

User-facing install steps live in [docs/install.md](docs/install.md). If you
change how the driver is deployed, an image is published, or an application
must call S3, update that guide in the same change.

## License

Contributions are accepted under the Apache License 2.0.
