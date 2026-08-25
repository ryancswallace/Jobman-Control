# Jobman contract snapshot

This directory is a generated pre-release snapshot of Jobman's
`protocol` package, canonical JSON schemas, and conformance fixtures for
`jobman/v1alpha1`.

- Source module: `github.com/ryancswallace/jobman`
- Source version: development snapshot preceding the first shared-mode release
- Canonical source directory: `protocol/`

Do not edit copied protocol, schema, or fixture files here. During pre-release
development, run `make contracts-sync JOBMAN_DIR=../jobman` and review the
result. Once Jobman publishes the contract in a tagged release, replace this
development snapshot with an explicit module dependency or a snapshot whose
source lock identifies that tag.

`checksums.txt` locks every copied source file, schema, and conformance fixture.
`make contracts-check` verifies that lock without a Jobman checkout;
`make contracts-source-check JOBMAN_DIR=../jobman` additionally compares every
copied byte with the canonical source tree.
