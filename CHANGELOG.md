# Changelog

All notable user-visible changes to `jobman-control` are documented here. The
format follows [Keep a Changelog], and releases use [Semantic Versioning].

## [Unreleased]

## [0.1.0] - 2026-08-29

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
  package, SBOM, signing, and tested-main release scaffolding, including
  semantic version selection after the v0.1.0 bootstrap, isolated SLSA
  provenance, exact-draft publication and recovery, and an assembled
  PostgreSQL-backed OIDC and agent mTLS release test.

### Known limitations

- This initial release is intended for controlled development and integration
  testing; it is not approved or recommended for production deployment.
- Jobman Control coordinates metadata and authorization but does not execute
  workloads or transfer artifact bytes. The compatible Jobman client and agent,
  introduced in Jobman v1.7.0, are required for target-side subprocess, Slurm,
  container, SSH bootstrap, NFS, and S3 effects.
- The API, portable workload snapshot, migrations, and operations contracts are
  pre-v1. Replicas must run the same version, migrations are forward-only, and
  rollback across a migration requires the matching database backup and binary.

[Unreleased]: https://github.com/ryancswallace/jobman-control/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ryancswallace/jobman-control/releases/tag/v0.1.0
[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
[Semantic Versioning]: https://semver.org/spec/v2.0.0.html
