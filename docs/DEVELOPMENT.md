# Development

## Bootstrap

Use the exact Go patch in `go.version`, or open the devcontainer. Repository
tools install under ignored `bin/`.

```console
make setup
docker compose up -d postgres
set -a
. ./.env.example
set +a
make run
```

The Compose project starts only PostgreSQL. Running the service directly keeps
development authentication on a true loopback listener, as required by its
security boundary. Stop the database with `docker compose down`; add `--volumes`
only when you intentionally want to delete the disposable development state.

## Tests

`make quick-check` is the normal edit loop. `make check` adds coverage,
workflows, shell, vulnerabilities, documentation, release builds, and container
checks. PostgreSQL integration tests require an explicit disposable URL:

```console
export JOBMAN_CONTROL_TEST_DATABASE_URL='postgres://postgres:jobman-control-development@127.0.0.1:5432/jobman_control?sslmode=disable'
make integration-test
```

Integration tests create and drop a unique schema but still require an
isolated database. Unit tests must not use developer homes, shared ports,
external identity providers, or production credentials.

The initial aggregate coverage floor is 30 percent because application startup
and most PostgreSQL behavior require assembled infrastructure. It is a ratchet,
not a target: new behavior needs focused tests, and the floor should rise as
integration and end-to-end coverage become part of the merged profile.

## Contracts

The local snapshot is always checked with `make contracts-check`. When Jobman's
canonical protocol changes, use sibling checkouts:

```console
make contracts-source-check JOBMAN_DIR=../jobman
make contracts-sync JOBMAN_DIR=../jobman
```

Review every copied byte and checksum change. Never edit copied schemas or
fixtures directly.

## Generated and release artifacts

`make docs` checks relative links and spelling. `make docker-smoke` builds and
inspects the non-root runtime image. `make release-build` compiles the complete
GoReleaser matrix. `make snapshot` additionally creates archives, packages,
checksums, SBOMs, and local image metadata without publishing or signing.

Do not commit `bin/`, `dist/`, coverage profiles, `.env` files, certificates,
keys, database dumps, or agent credentials.
