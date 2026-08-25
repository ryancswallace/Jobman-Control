# Jobman Control documentation

Jobman Control is pre-release. Documentation distinguishes implemented
behavior from planned behavior; the OpenAPI document and tested code remain
the executable contract for the current `v1alpha1` slice.

- [Architecture](ARCHITECTURE.md): repository and service boundaries, data
  ownership, and request flows.
- [API](API.md): endpoint contract, authentication classes, and idempotency.
- [Configuration](CONFIGURATION.md): every supported environment setting.
- [Development](DEVELOPMENT.md): local PostgreSQL, tests, contracts, and tools.
- [Deployment](DEPLOYMENT.md): production topology, TLS, identity, containers,
  native packages, and startup.
- [Operations](OPERATIONS.md): migrations, readiness, backup, restore,
  upgrades, and incident handling.
- [Security model](SECURITY_MODEL.md): assets, principals, trust boundaries,
  and failure policy.
- [Production readiness](PRODUCTION_READINESS.md): initial security review,
  implemented controls, evidence, and remaining release blockers.
- [Compatibility](COMPATIBILITY.md): current platform, PostgreSQL, protocol,
  and stability matrix.
- [Repository scaffolding](REPOSITORY_SCAFFOLDING.md): shared infrastructure
  and intentional service-specific differences.

The long-term distributed-mode design remains canonical in the Jobman
repository's `docs/design/DISTRIBUTED_MODE.md`. This repository documents what
the Control service itself owns and implements.
