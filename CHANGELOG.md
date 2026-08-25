# Changelog

All notable user-visible changes to `jobman-control` are documented here. The
format follows [Keep a Changelog], and releases use [Semantic Versioning].

## [Unreleased]

### Added

- Added the pre-release shared control plane with PostgreSQL-backed namespaces,
  targets, immutable target generations, jobs, runs, executions, assignments,
  audit events, agent enrollment, mTLS identity, cancellation, and durable
  intent.
- Added the versioned Jobman workload snapshot and idempotent client and agent
  APIs.
- Added target-approved filesystem log manifests with immutable NFS-compatible
  objects, gap-safe delivery, authorization, and terminal log completeness.
- Added ordered Slurm submission/observation/completion evidence, a forward-only
  scheduler-state migration, atomic lifecycle projection, native scheduler
  status in job reads, and PostgreSQL integration coverage.
- Added backend-aware workload admission so unsupported first-slice features
  fail placement before an agent assignment is created.
- Added revision-guarded target drain/disable/retire transitions, rotating agent
  sessions, immutable capability/liveness snapshots, freshness-gated
  assignment, and conservative stale-observation confidence reconciliation.
- Added target-approved regular-file artifact mappings, effective store-version
  pinning, transactional terminal output metadata, authorized artifact manifest
  reads, and PostgreSQL integration coverage.
- Added on-premises/AWS ParallelCluster provider facts and revision-checked
  immutable target-generation rollover while preserving earlier job and agent
  generation bindings.
- Added atomic explicit collections, ordered independent child jobs,
  collection concurrency and fail-fast policy, aggregate reads, and
  `never`/`prefer`/`require` Slurm-array task bindings.
- Added local/NFS or S3 logical artifact admission and native/container runtime
  admission; target-side byte transfer and runtime effects remain owned by the
  Jobman agent.
- Added immutable dependency graphs with transactional multi-target admission,
  cycle rejection, explicit outcome predicates, bounded cross-target
  readiness, unsatisfied-branch dispositions, aggregate reads, and graph
  cancellation.
- Added namespace active/queued/group quotas, revision-checked policy updates,
  namespace round-robin dispatch, bounded Prometheus metrics, role-protected
  append-only audit export, and state-aware idempotency/outbox retention.
- Added a persistent recovery epoch and assignment hold for post-restore
  reconciliation, with an operator runbook.
- Added dry-run-capable import of quiescent completed SQLite history metadata
  with new shared IDs, provenance uniqueness, audit/outbox evidence, and no
  runnable state or bulk-byte migration.
- Added reproducible development, documentation, security, coverage, container,
  package, SBOM, signing, and release scaffolding.

[Unreleased]: https://github.com/ryancswallace/jobman-control/compare/main...HEAD
[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
[Semantic Versioning]: https://semver.org/spec/v2.0.0.html
