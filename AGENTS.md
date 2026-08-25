# AGENTS.md

Jobman Control is the shared control plane for Jobman. It is a Go service with
PostgreSQL-backed state. The project is pre-release: documentation describes
target behavior only when it says so explicitly.

## Boundaries

- Keep the client and agent protocol dependency-light and versioned.
- Clients and agents never receive PostgreSQL credentials.
- PostgreSQL stores metadata, manifests, audit records, and durable intent; it
  does not store bulk artifact or log bytes.
- External I/O never occurs inside a database transaction.
- Every shared resource is namespace scoped and every lookup is authorized.
- Mutating APIs and worker messages are idempotent. Never promise exactly-once
  external execution.
- This repository owns the control API, coordinator policy, PostgreSQL
  migrations, identity/authorization, target registry, audit, and service
  operations. Target-side execution remains in the Jobman repository.

## Working conventions

- Read relevant code and tests and check `git status` before editing.
- Preserve unrelated and developer-local work. Do not commit, push, publish,
  or change repository settings unless explicitly requested.
- Use `context.Context` for blocking work and wrap errors with operation
  context. Keep errors lowercase and avoid ordinary panics and `os.Exit` away
  from `main`.
- Keep SQL transactions short, use explicit constraints and revisions, and
  make migrations forward-only and deterministic.
- Never read or print secrets, environment files, database URLs, tokens,
  workload environments, command contents, or artifact contents.
- Add narrow maintained dependencies only when the standard library is
  insufficient. Run `go mod tidy` rather than editing `go.sum`.
- Format with `make format`; run focused tests while iterating and `make check`
  before handoff when the environment permits.
- Unit tests must not use developer homes, external networks, real credentials,
  wall-clock sleeps, or shared ports. PostgreSQL integration tests use the
  explicit `JOBMAN_CONTROL_TEST_DATABASE_URL` test setting and otherwise skip.

## Repository map

- `main.go` is the executable boundary and owns signal handling, version
  output, safe error presentation, and process status.
- `internal/httpapi/` owns the HTTP contract and authentication dispatch.
- `internal/domain/` owns service policy and repository interfaces.
- `internal/store/postgres/` owns authorized persistence, transactions,
  reconciliation, and embedded forward-only migrations.
- `internal/auth/` and `internal/agentca/` own OIDC and agent certificate
  identity boundaries.
- `contracts/jobman/v1alpha1/` is a mechanically copied, checksummed
  pre-release snapshot. Never edit it directly.
- `api/` contains the authored OpenAPI contract. Keep it synchronized with
  handlers, errors, and examples.
- `docs/`, `etc/jobman-control/`, Dockerfiles, `.goreleaser.yml`, and release
  workflows form the operations and distribution contract.

## PostgreSQL and API changes

- Authorize every namespace-scoped lookup in the same repository operation
  that reads or mutates it. Do not rely on an HTTP pre-check alone.
- Prefer database constraints for invariants shared by multiple writers and
  explicit row locks for state transitions. Keep network and filesystem I/O
  outside transactions.
- Once a migration reaches `main`, never edit or reorder it. Add a new
  migration and document upgrade, compatibility, backup, and rollback impact.
- Requests and worker messages need bounded decoding, stable identities,
  canonical digests, and byte-equivalent replay behavior. Preserve coarse,
  non-disclosing error classifications.
- Agent assignment is intent, not launch authority. Artifact manifests are
  metadata, not proof that Control possesses or can authorize the bytes.

## Documentation, containers, and releases

- Update `README.md`, `docs/`, `api/openapi-v1alpha1.yaml`, and `CHANGELOG.md`
  together for user-visible behavior. Do not claim planned behavior exists.
- Runtime containers must remain non-root, signal-correct, secret-free, and
  useful with a read-only root filesystem. Keep `Dockerfile` and
  `Dockerfile.goreleaser` aligned.
- Scripts under `devel/` must be deterministic, non-interactive, bounded, and
  safe to rerun. Validate them with `make shellcheck`.
- Keep release archives, native packages, OCI images, SBOMs, checksums,
  signatures, and documentation aligned. Snapshot builds must never publish.
- Do not commit, push, tag, publish, open a pull request, enable workflows, or
  apply `.github/settings.yml` without explicit user authorization.

Useful gates are:

```sh
make quick-check
make integration-test
make docs
make docker-smoke
make release-build
make check
```

`make check` requires network access for tools and vulnerability data plus a
running Docker daemon. Report exact skipped or failed commands and unverified
scope at handoff.
