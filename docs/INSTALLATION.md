# Installation

Jobman Control is pre-release. Build from a reviewed source commit or use a
verified draft artifact during evaluation; no stable production channel exists
yet.

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

The release pipeline is prepared to publish Linux amd64/arm64 OCI images and
portable archives for Linux, macOS, and Windows. It also creates DEB, RPM, and
APK packages with systemd and environment-file scaffolding. These channels are
not supported until the first release is published and verified.

Always verify the release checksum bundle and GitHub attestation before
installation. See [RELEASE.md](../RELEASE.md) and the
[deployment guide](DEPLOYMENT.md).
