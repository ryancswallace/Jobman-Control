# Repository scaffolding

Jobman Control follows the maintained repository baseline established by
Jobman and Jobman Diagnose, adapted to a network service with durable shared
state. This document records that baseline so apparent differences can be
reviewed intentionally instead of copied mechanically.

## Shared baseline

The repository includes the same classes of supporting material as its sibling
projects:

- editor, Git, devcontainer, Docker, spelling, lint, and coverage configuration;
- contribution, conduct, support, security, citation, changelog, license, and
  third-party notice documents;
- pinned tool installation, formatting, lint, vulnerability, workflow, shell,
  documentation, race, fuzz, cross-build, and release checks;
- issue and pull-request templates, ownership and label metadata, Dependabot,
  settings-as-code, CodeQL, dependency review, Scorecard, and maintenance CI;
- CGO-free release archives, native Linux packages, SBOMs, checksums, Sigstore
  signatures, GitHub attestations, and a non-root multi-platform image; and
- tested-main semantic release automation, isolated SLSA provenance,
  retained-draft recovery, and optional Homebrew and Cloudsmith distribution
  workflows.

## Control-specific additions

The service baseline adds a pinned PostgreSQL development container, real
PostgreSQL CI coverage, OpenAPI and protocol-contract verification, deployment
and operations guides, a hardened systemd unit, environment-file examples, and
documentation for OIDC, TLS, agent mTLS, migrations, backup, and restore.

The package does not enable its systemd unit automatically. Operators must
provide PostgreSQL, identity, token, and certificate configuration before the
service can start. The systemd unit uses a dynamic identity and systemd
credentials for TLS and CA files. Container and Homebrew installations also
leave service-manager configuration to the operator rather than embedding
secrets or unsafe development defaults.

## Intentional differences

Some sibling-repository components do not belong in Control:

- Cobra man pages, shell completions, terminal recordings, and a command
  reference site are omitted because the current executable has only version
  reporting and otherwise runs the HTTP service.
- Jobman's detached-process, signal, and daemonless lifecycle tests remain in
  Jobman, which owns target-host execution. Control tests its API, coordinator,
  PostgreSQL state, identity, and agent protocol instead.
- Jobman Diagnose's report schemas, example captures, evaluation corpus, and
  diagnostic fixtures remain in the read-only diagnostic product.
- The initial v0.1.0 tag is a one-time maintainer bootstrap because
  semantic-release otherwise initializes an untagged repository at v1.0.0.
  Later versions are calculated from Conventional Commits on tested `main`,
  with protected-environment approval retained because the API and
  forward-only database migrations are pre-v1.
- A GitHub Pages site is deferred until Control has a stable operator-facing
  command or API surface that benefits from independently published reference
  material. Canonical documentation currently stays beside the code and
  OpenAPI document.

Revisit these decisions when the CLI gains operational subcommands, the API
stabilizes, or the repository adopts independently hosted documentation. Do
not duplicate execution engines, diagnostic interpretation, or protocol
definitions here merely for superficial file parity.
