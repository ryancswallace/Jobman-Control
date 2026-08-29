# Installation

Jobman Control v0.1.x is evaluation software, not a stable production channel.
Build from a reviewed source commit or install a verified GitHub release
artifact.

## Source build

Install the exact Go patch in `go.version`, then:

```console
make setup
make check
make build
./bin/jobman-control --version
```

The binary is CGO-free. Copy it with `LICENSE`, `THIRD_PARTY_NOTICES.md`, the
OpenAPI document, and operations documentation. Configuration and PostgreSQL
remain external.

## Container and native packages

The release pipeline publishes Linux amd64/arm64 OCI images and portable
archives for Linux, macOS, and Windows. It also creates DEB, RPM, and APK
packages with systemd and environment-file scaffolding. These are evaluation
channels under the v0.1.x best-effort support policy.

Always verify the release checksum bundle and GitHub attestation before
installation. See [RELEASE.md](../RELEASE.md) and the
[deployment guide](DEPLOYMENT.md).
