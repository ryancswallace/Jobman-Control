# Installation

Install a verified [GitHub release artifact](https://github.com/ryancswallace/Jobman-Control/releases)
or build from a reviewed source commit. The
[compatibility reference](COMPATIBILITY.md) covers upgrade constraints for the
pre-v1 API.

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
packages with a systemd unit and example environment file. See the
[support policy](../SUPPORT.md) for the scope of release support.

Always verify the release checksum bundle and GitHub attestation before
installation. See [RELEASE.md](../RELEASE.md) and the
[deployment guide](DEPLOYMENT.md).
