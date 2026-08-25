# Contributing

Thanks for helping improve Jobman Control. Bug fixes, tests, migrations,
authorization hardening, API contract work, operational documentation, and
deployment improvements are welcome. Discuss large schema, identity,
coordinator, or compatibility changes in an issue before investing in an
implementation; focused fixes can go directly to a pull request.

By participating, you agree to follow the [code of conduct](CODE_OF_CONDUCT.md).
Report vulnerabilities through [SECURITY.md](SECURITY.md), not a public issue.

## Development setup

Use the exact Go patch recorded in `go.version`. The devcontainer is the
supported reproducible environment; a matching local toolchain also works.

```console
git clone https://github.com/ryancswallace/jobman-control.git
cd jobman-control
make setup
docker compose up -d postgres
set -a
. ./.env.example
set +a
make run
```

The example database credential is local-only. Never reuse it outside the
development Compose service. Client and agent execution endpoints require the
additional certificate setup described in [the development guide](docs/DEVELOPMENT.md).

## Verification

Run focused tests while iterating, then the complete gate before submission:

```console
make format
make check
```

Useful narrower targets include:

- `make test` for race-enabled unit tests;
- `make integration-test` with an explicit test database URL;
- `make coverage-check` for the current coverage ratchet;
- `make fuzz` for the portable request decoder;
- `make docs` for spelling and relative-link validation;
- `make docker-smoke` for the non-root runtime image; and
- `make snapshot` for archives, native packages, container metadata, checksums,
  and SBOMs without publishing or signing.

PostgreSQL integration tests create and remove a uniquely named schema. They
must target a disposable database selected by
`JOBMAN_CONTROL_TEST_DATABASE_URL`; never point that setting at shared or
production state.

## Compatibility and security

Every API resource is namespace-scoped, authorization belongs in repository
queries, mutations are idempotent, and external I/O must not occur inside
database transactions. PostgreSQL holds metadata and intent, never bulk log or
artifact bytes. Client and agent processes never receive database credentials.

Migrations are forward-only. Changes to an existing migration are acceptable
only before it has landed on the default branch; otherwise add a new migration
and document upgrade and rollback implications. Changes to the copied Jobman
contract must originate in Jobman's canonical `protocol/` directory and be
synchronized with `make contracts-sync JOBMAN_DIR=../jobman`.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/) with prefixes
such as `fix:`, `feat:`, `docs:`, `test:`, `ci:`, or `chore:`. Keep pull
requests focused. Explain the problem, approach, compatibility and migration
impact, security boundaries, and verification performed. Update documentation
and `CHANGELOG.md` for notable behavior.

Contributions are accepted under the [MIT License](LICENSE).
