# Release process

Jobman Control releases are maintainer-triggered semantic-version tags staged
as private drafts. This is deliberate while the API, migration, and operations
contracts remain pre-v1. The first planned release is v0.1.0.

## Candidate validation

From a clean checkout using the exact Go toolchain:

```console
make setup
make check
make snapshot
make package-smoke
```

Run the PostgreSQL integration suite against the supported PostgreSQL version,
exercise upgrade from the previous release when one exists, and validate one
OIDC client plus one mTLS agent path. Confirm backup and restore procedures in
[the operations guide](docs/OPERATIONS.md). A release must not depend on a
sibling Jobman checkout; the checked-in contract snapshot and source record
must be internally consistent.

## Publish

Create an annotated `vMAJOR.MINOR.PATCH` tag only after the candidate gates
pass, sign it when a signing identity is available, and push it to GitHub. The
release workflow:

- rejects malformed tags and tags outside `main`;
- repeats source, security, documentation, test, and release checks;
- builds CGO-free Linux, macOS, and Windows archives;
- builds Linux `.deb`, `.rpm`, and `.apk` packages;
- builds a non-root multi-platform OCI image;
- emits SHA-256 checksums and SPDX SBOMs;
- signs checksums and image digests keylessly with Sigstore; and
- records GitHub build attestations.

Inspect the retained draft before publishing. Verify archive and package
contents, migrations, executable version, image user and labels, checksums,
SBOMs, signatures, and attestations. Use the separately dispatched staged
release workflow to publish the exact verified draft; never rebuild or replace
artifacts under an existing tag.

Post-release Homebrew and Cloudsmith publication is optional infrastructure.
Enable those workflows only after the repository's protected `main`
environment has narrowly scoped `HOMEBREW_TAP_TOKEN` and
`CLOUDSMITH_API_KEY` secrets. Container publication uses the workflow's GitHub
token and GHCR permissions.

## Verify published artifacts

Download an artifact, checksum manifest, and Sigstore bundle from the same
release. Verify the checksum bundle against the exact release workflow
identity, then verify the artifact's GitHub attestation and embedded version.
The concrete commands and repository identity are recorded in each release's
notes.

After publication, perform a clean installation, migrate a restored test
database, enroll a disposable agent, run one job, verify both log streams, and
exercise graceful shutdown. Record any release caveat in `CHANGELOG.md`.
