# Jobman contract snapshot

This directory is a generated snapshot of Jobman's
`protocol` package, canonical JSON schemas, and conformance fixtures for
`jobman/v1alpha1`.

- Source module: `github.com/ryancswallace/jobman`
- Source version: `v1.7.0`
- Source commit: `f23993f51aa7eb4638d13cc4cd46c5d62aa20d27`
- Canonical source directory: `protocol/`
- Later verification: byte-identical in `v1.8.0` at
  `7afb3fcc951231a88766424a2d4d97c5690c1a63`

Do not edit copied protocol, schema, or fixture files here. The copied tree is
retained because it includes Go sources, JSON schemas, and conformance fixtures
that must be released and checked together. Run `make contracts-sync
JOBMAN_DIR=../jobman` only after the canonical Jobman source changes, review the
result, and update this source record to the exact tagged source release.

`checksums.txt` locks every copied source file, schema, and conformance fixture.
`make contracts-check` verifies that lock without a Jobman checkout;
`make contracts-source-check JOBMAN_DIR=../jobman` additionally compares every
copied byte with the canonical source tree.
