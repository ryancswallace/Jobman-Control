# Jobman Control documentation

Jobman Control releases use a pre-v1 API. The OpenAPI document and tested code
define the current `v1alpha1` contract.

- [Architecture](ARCHITECTURE.md): repository and service boundaries, data
  ownership, and request flows.
- [API](API.md): endpoint contract, authentication classes, and idempotency.
- [Configuration](CONFIGURATION.md): external service files, protected credential
  references, bounded tuning and compatibility environment settings.
- [Declarative resources](DECLARATIVE_CONFIGURATION.md): offline validation and
  identity/revision-checked administrative plan/apply.
- [Development](DEVELOPMENT.md): local PostgreSQL, tests, contracts, and tools.
- [Deployment](DEPLOYMENT.md): production topology, TLS, identity, containers,
  native packages, and startup.
- [Operations](OPERATIONS.md): migrations, readiness, backup, restore,
  upgrades, and incident handling.
- [Security model](SECURITY_MODEL.md): assets, principals, trust boundaries,
  and failure policy.
- [Production readiness](PRODUCTION_READINESS.md): implemented controls,
  historical evidence, and site acceptance exercises.
- [Compatibility](COMPATIBILITY.md): current platform, PostgreSQL, protocol,
  and stability matrix.

The long-term distributed-mode design remains canonical in the Jobman
repository's `docs/design/DISTRIBUTED_MODE.md`. This repository documents what
the Control service itself owns and implements.
