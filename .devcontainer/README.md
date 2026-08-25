# Devcontainer

The devcontainer is the reproducible contributor environment for Jobman
Control. It provides Go 1.26.6, pinned Go editor and debugger tools, GitHub CLI,
PostgreSQL client utilities, ShellCheck, and Docker-outside-of-Docker for the
development database, integration tests, and release-container checks.

Run `docker compose up -d postgres` after opening the repository, copy the safe
values from `.env.example` into your shell, and use `make run`. The container
does not mount a sibling Jobman checkout; `make contracts-source-check` is the
only optional operation that needs one.

Docker-outside-of-Docker exposes the host Docker socket, so treat this as a
trusted development environment. Keep credentials in editor or Codespaces
secret storage, or an ignored `.devcontainer/.env.local`. Never add database
credentials, OIDC secrets, private keys, certificates, or agent tokens to the
shared configuration.
