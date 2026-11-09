# Development utilities

This directory contains repository automation that is not part of the shipped
service:

- `contractsync/` synchronizes and verifies the tagged Jobman protocol
  snapshot.
- `docscheck/` validates repository-relative Markdown links.
- `homebrewformula/` creates a formula from a published release checksum
  manifest.
- `container-smoke.sh` validates the non-root runtime image and embedded
  release identity.
- `check-release-metadata.sh` aligns stable tags and changelog records.
- `prepare-release-changelog.sh` renders a tag-aware changelog for release
  archives without modifying the tracked source.
- `check-release.sh` verifies archives, native packages, checksums, and SBOMs.
- `make gen-notices` uses Python 3 and the pinned Go toolchain to refresh
  `THIRD_PARTY_NOTICES.md` after dependency changes. `make notices-check`
  verifies the result before packaging.
- `package-smoke.sh` installs snapshot packages in pinned target
  distributions.
- `publish-cloudsmith-packages.sh` verifies and publishes already-public native
  packages without rebuilding them.
- `verify-publish-release.sh` publishes one exact retained draft only after its
  signatures, provenance, assets, image, and source commit agree.
- `updates/` contains deterministic repository-maintenance scripts.

Run these through the Makefile so pinned versions and paths remain consistent.
Generators must be deterministic and have focused tests.
