# Development utilities

This directory contains repository automation that is not part of the shipped
service:

- `contractsync/` synchronizes and verifies the temporary Jobman protocol
  snapshot.
- `docscheck/` validates repository-relative Markdown links.
- `homebrewformula/` creates a formula from a published release checksum
  manifest.
- `container-smoke.sh` validates the non-root runtime image and embedded
  release identity.
- `check-release-metadata.sh` aligns stable tags, changelog records, and
  citation metadata.
- `check-release.sh` verifies archives, native packages, checksums, and SBOMs.
- `package-smoke.sh` installs snapshot packages in pinned target
  distributions.
- `publish-cloudsmith-packages.sh` verifies and publishes already-public native
  packages without rebuilding them.
- `updates/` contains deterministic repository-maintenance scripts.

Run these through the Makefile so pinned versions and paths remain consistent.
Generators must be deterministic and have focused tests.
