# Releasing Jobman Control

After the one-time v0.1.0 bootstrap, releases are automated from tested commits
on `main`. Semantic-release chooses the next version and creates the
`vX.Y.Z` tag and draft GitHub release. GoReleaser then builds and signs the
artifacts, an isolated generator adds SLSA provenance, and the workflow
publishes only the exact verified draft.

## Release flow

1. Merge a pull request into `main` using Conventional Commit messages.
2. The `Test` workflow runs against the new `main` commit.
3. After it succeeds, the `Release` workflow previews semantic-release without
   entering the protected environment. If a release is required, it waits for
   Test, CodeQL, Fuzz, Docs links, OpenSSF Scorecard, and Jobman contract source
   to succeed on that exact commit.
4. After the protected `main` environment is approved, the workflow
   revalidates the source, PostgreSQL integration suite, release configuration,
   packages, and container.
5. Semantic-release creates the next tag and draft. GoReleaser checks out that
   exact tag and stages binaries, archives, native Linux packages, SBOMs,
   checksums, a checksum signature, and signed immutable version tags for the
   multi-platform GHCR image.
6. The SLSA generator uploads `jobman-control.intoto.jsonl` for every
   checksummed artifact. The workflow verifies the remote asset set, signature,
   provenance, source commit, image digest, and executable metadata before
   publishing the draft by numeric release ID.
7. Stable releases move the `latest` image alias to the already signed
   immutable image, then dispatch the independently repairable Homebrew and
   Cloudsmith publication workflows.
8. Repository maintenance renders the published tag into `CHANGELOG.md` and
   publishes a review branch. Merge that maintenance pull request before the
   next release.

Release jobs are serialized and never cancelled in progress. Tag pushes do not
independently start a release, which prevents duplicate publication when
semantic-release creates later tags.

## Bootstrap v0.1.0

Semantic-release intentionally chooses v1.0.0 for a repository without any
release tags, so v0.1.0 has one explicit bootstrap step. Do this only after the
release-preparation commit is on `main`.

From a clean checkout of the exact `origin/main` candidate, run:

```console
make setup
make RELEASE_TAG=v0.1.0 release-metadata-check
make check
make snapshot
make package-smoke
```

Also run the PostgreSQL integration suite against the supported PostgreSQL
version, exercise one OIDC client and one mTLS agent path, and confirm backup
and restore using [the operations guide](docs/OPERATIONS.md). The release must
not depend on a sibling Jobman checkout.

After all required GitHub workflows succeed on that exact commit:

```console
git fetch origin main
git switch main
git merge --ff-only origin/main
git tag -a v0.1.0 -m "Jobman Control v0.1.0"
git push origin v0.1.0
```

Then open **Actions → Release → Run workflow**, select `main`, enter
`v0.1.0` in the tag field, and approve the protected `main` environment
only after confirming the workflow selected the intended commit. The supplied
tag path revalidates the same exact-commit gates and otherwise follows the
normal build, provenance, verification, and publication flow.

After v0.1.0 is public, leave the tag field empty. Later releases are selected
automatically from Conventional Commits.

## Version selection

Semantic-release follows Conventional Commits:

| Commit | Release effect | Example |
| --- | --- | --- |
| `fix:` or `perf:` | Patch | `fix: preserve assignment evidence` |
| `feat:` | Minor | `feat: add an operator endpoint` |
| `BREAKING CHANGE:` footer or `!` | Major | `feat!: revise the control API` |
| `docs:`, `test:`, `ci:`, `chore:` | No release by themselves | `docs: clarify restore recovery` |

Use squash-merge titles that follow this convention. Commits on `main`, not
pull-request labels, determine the release.

The API, migrations, workload contract, and operations model remain pre-v1.
A v1 release requires an explicit breaking-change signal and a deliberate
compatibility review; do not approve it merely because semantic-release
calculated v1.0.0.

## Published artifacts

Each GitHub release contains:

- `.tar.gz` archives for Linux and macOS and `.zip` archives for Windows;
- `amd64`, `arm64`, and supported `386` binaries;
- DEB, RPM, and APK packages for Linux;
- SPDX JSON SBOMs for every archive and native package;
- a SHA-256 checksum manifest and keyless Sigstore bundle;
- `jobman-control.intoto.jsonl`, containing verifiable SLSA provenance for
  every checksummed artifact;
- GitHub build attestations for checksummed artifacts and container digests;
  and
- keyless signatures for the multi-platform container manifest and its
  platform images.

GoReleaser publishes Linux `amd64` and `arm64` images to:

```text
ghcr.io/ryancswallace/jobman-control:<version>
ghcr.io/ryancswallace/jobman-control:v<version>
ghcr.io/ryancswallace/jobman-control:latest
```

The `latest` alias moves only after a stable GitHub release is public.
Immutable version tags can be visible while the matching release remains a
draft, so verify the published release and digest signature before use. The
runtime image is non-root and requires external PostgreSQL, identity, TLS, and
agent-CA configuration.

The macOS executables are not Apple Developer ID signed or notarized, and the
Windows executables are not Authenticode signed. Checksums, Sigstore bundles,
attestations, and SLSA provenance authenticate the release bytes but do not
replace platform publisher signatures.

## Repository configuration

The release workflow uses GitHub's short-lived token for tags, releases,
attestations, workflow dispatch, and GHCR. It uses the following protected
`main` environment secrets only in post-release distribution workflows:

- `HOMEBREW_TAP_TOKEN`, restricted to Contents and Pull requests read/write
  on `ryancswallace/homebrew-tap`; and
- `CLOUDSMITH_API_KEY`, restricted to publishing in the public
  `jobman/stable` repository.

Keep default workflow permissions read-only. The workflows grant narrowly
scoped write permissions per job:

- `contents: write` for semantic tags, drafts, assets, and publication;
- `packages: write` for GHCR and stable-alias promotion;
- `id-token: write` for keyless signing and isolated provenance;
- `attestations: write` and `artifact-metadata: write` for GitHub
  attestations; and
- `actions: write` only where a verified post-release or maintenance workflow
  must be dispatched.

Ensure this repository's workflow can write the `jobman-control` GHCR package
and make that package public. Protect existing `v*` tags from update and
deletion. Permit authorized maintainers and the release workflow to create new
tags without granting a bypass that can rewrite existing release tags.

The release and recovery workflows reject dispatches outside `main`; retain
the protected environment's required reviewers. The certificate identity used
for checksum and container verification is:

```text
https://github.com/ryancswallace/jobman-control/.github/workflows/release.yml@refs/heads/main
```

The SLSA generator is referenced by the exact `v2.1.0` release tag because its
verifier does not trust provenance produced from a commit-SHA reusable-workflow
reference. This is the same deliberate exception used by Jobman.

## Local validation

Install the exact Go toolchain in `go.version`, GoReleaser 2.17, Syft, Docker
with Buildx, and QEMU/binfmt. Then run:

```console
make check
make snapshot
make package-smoke
```

Snapshot mode never publishes and skips keyless signing. It generates the same
tag-aware changelog used in release archives without modifying tracked
`CHANGELOG.md`. Before v0.1.0 exists, pass
`RELEASE_TAG=v0.1.0` to `release-metadata-check` to validate the prepared
bootstrap metadata.

Run the required PostgreSQL suite with an explicit disposable database:

```console
JOBMAN_CONTROL_TEST_DATABASE_URL='postgres://postgres:password@127.0.0.1:5432/jobman_control_test?sslmode=disable' \
  make integration-test
```

## Recovery

Use the ordinary **Release** workflow with an existing tag when the tag still
identifies current `main` and the release is missing or remains a draft. This
is the v0.1.0 bootstrap path and can also recover an interruption before a
complete draft was staged.

Use **Publish staged release** only when the complete draft already contains the
signed checksum manifest, all checksummed assets, SLSA provenance, and the
immutable image. That workflow never rebuilds or replaces artifacts. It
rechecks exact-commit CI, verifies the retained remote asset set, and publishes
the exact draft. It intentionally does not move the mutable `latest` alias;
after a recovered stable publication, run **Repair stable container alias**
with the published tag.

If Homebrew or Cloudsmith publication fails after GitHub publication, rerun the
corresponding workflow from `main` with the existing stable tag. Those
workflows verify public checksums, signatures, and attestations and publish only
the retained release bytes.

Never rebuild or replace artifacts under a published tag. Published releases
and versioned images are immutable; fix a defect with a new patch release.

## Verify a published release

Download one archive, its checksum manifest, Sigstore bundle, and
`jobman-control.intoto.jsonl` from the same GitHub release. Verify the checksum
bundle against the `main` release-workflow identity above, verify the
artifact's GitHub attestation with source ref `refs/heads/main`, and verify
SLSA provenance against source URI
`github.com/ryancswallace/jobman-control`.

Then perform a clean installation, migrate a restored disposable database,
enroll a disposable agent, run one job, verify stdout and stderr manifests, and
exercise graceful shutdown. Record any caveat in `CHANGELOG.md`.
