<!-- markdownlint-disable MD041 -->
<!-- markdownlint-disable MD033 -->
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-control.svg">
  <img alt="Jobman Control" src="assets/logo-dark-control.svg" width="420">
</picture>
<!-- markdownlint-enable MD033 -->

[![Test](https://github.com/ryancswallace/jobman-control/actions/workflows/test.yml/badge.svg)](https://github.com/ryancswallace/jobman-control/actions/workflows/test.yml)
[![Codecov](https://codecov.io/gh/ryancswallace/jobman-control/branch/main/graph/badge.svg)](https://codecov.io/gh/ryancswallace/jobman-control)
[![CodeQL](https://github.com/ryancswallace/jobman-control/actions/workflows/codeql.yml/badge.svg)](https://github.com/ryancswallace/jobman-control/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/ryancswallace/jobman-control/badge)](https://scorecard.dev/viewer/?uri=github.com/ryancswallace/jobman-control)
[![Latest release](https://img.shields.io/github/v/release/ryancswallace/jobman-control?sort=semver)](https://github.com/ryancswallace/jobman-control/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/ryancswallace/jobman-control)](https://github.com/ryancswallace/jobman-control/blob/main/go.mod)
[![Go Reference](https://pkg.go.dev/badge/github.com/ryancswallace/jobman-control.svg)](https://pkg.go.dev/github.com/ryancswallace/jobman-control)
[![Documentation](https://img.shields.io/badge/docs-Jobman_Control-blue)](docs/README.md)
[![OSS hosting by Cloudsmith](https://img.shields.io/badge/OSS%20hosting%20by-Cloudsmith-blue?logo=cloudsmith)](https://cloudsmith.com/)

Jobman Control is the shared control plane for
[Jobman](https://github.com/ryancswallace/jobman). It gives workstations,
on-premises Slurm clusters, and AWS ParallelCluster deployments one place to
submit work, enforce policy, and track results.

> [!WARNING]
> Jobman Control is pre-release evaluation software. The initial implementation
> is ready for development and integration testing, but it has not completed
> the department's production acceptance exercises.

## What it provides

| Capability | Jobman Control provides... |
| --- | --- |
| Shared state | PostgreSQL-backed jobs, runs, assignments, target generations, policy, and audit history |
| Placement | Named hosts, on-premises Slurm partitions, and AWS ParallelCluster target records |
| Admission | Portable workload validation against target, runtime, platform, resource, log, and artifact capabilities |
| Coordination | Idempotent submission, namespace quotas, fair dispatch, cancellation, collections, Slurm arrays, and dependency graphs |
| Agent trust | One-time enrollment, mTLS certificates, rotating sessions, generation pinning, and replay-safe assignment acceptance |
| Identity | OIDC users, namespace memberships, and repository-level authorization checks |
| Results | Ordered execution events plus checksummed log and artifact manifests for local filesystems, NFS, and S3 |
| Operations | Readiness, bounded Prometheus metrics, audit export, retention policy, restore holds, and completed-history import |

## How it fits

```mermaid
flowchart LR
    client["Jobman clients"] -->|"OIDC + HTTPS"| control["Jobman Control"]
    control -->|"durable metadata + intent"| postgres[("PostgreSQL")]
    control -->|"mTLS assignments"| agent1["Agent: workstation"]
    control -->|"mTLS assignments"| agent2["Agent: Slurm submit host"]
    agent1 --> process["Subprocess / container"]
    agent2 --> slurm["Slurm / ParallelCluster"]
    agent1 --> stores["Local / NFS / S3 bytes"]
    agent2 --> stores
```

The split is deliberate:

| Component | Owns |
| --- | --- |
| Jobman Control | API, PostgreSQL state, authorization, placement policy, coordination, audit, and recovery controls |
| `jobman-agent` | Target-side execution, Slurm CLI calls, runtime adapters, durable local journals, and byte transfer |
| `jobman shared` | User-facing submission, inspection, waiting, cancellation, logs, artifacts, collections, and graphs |
| Department infrastructure | PostgreSQL availability, OIDC, TLS, NFS/S3 policy, Slurm configuration, AWS infrastructure, and backups |

Control never starts a process, invokes Slurm, opens SSH, or proxies workload
bytes. PostgreSQL holds metadata and durable intent; log and artifact bytes
stay in the target-approved store.

## Supported model

| Layer | Supported in the initial implementation |
| --- | --- |
| State | PostgreSQL for shared mode; standalone Jobman continues to use local SQLite |
| Execution | Subprocess and Slurm, performed by `jobman-agent` |
| Control path | Agent API for steady-state work; SSH bootstrap and Slurm CLI remain target-side Jobman features |
| Runtime | Native commands plus target-approved Docker, Podman, or Apptainer execution |
| Artifacts and logs | Local filesystem, NFS, or S3 object references with bounded checksummed manifests |
| Placement | Named host, Slurm cluster/partition, or ParallelCluster generation |
| Grouping | Single jobs, collections, compatible Slurm arrays, and cross-target dependency graphs |
| Isolation | OIDC principals, namespaces, memberships, roles, quotas, and immutable target generations |

The portable `jobman/v1alpha1` workload contract keeps these choices
composable. A request describes work and requirements without embedding a
particular transport, scheduler command, mount point, or cloud deployment.

## Try it locally

You need Docker and the Go version pinned in [`go.version`](go.version).

```console
make setup
docker compose up -d postgres
```

Start Control with its loopback-only development identity:

```console
export JOBMAN_CONTROL_DATABASE_URL='postgres://postgres:jobman-control-development@127.0.0.1:5432/jobman_control?sslmode=disable'
export JOBMAN_CONTROL_AUTH_MODE=development
export JOBMAN_CONTROL_DEVELOPMENT_AUTH=true
export JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE=research
make run
```

Check the process and database separately:

```console
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

Development authentication is intentionally restricted to loopback. For
target registration, job submission, and agent enrollment examples, continue
with the [API guide] and [development guide].

## Configuration at a glance

| Need | Settings |
| --- | --- |
| PostgreSQL | `JOBMAN_CONTROL_DATABASE_URL`, `JOBMAN_CONTROL_MIGRATE_ON_START` |
| Listener | `JOBMAN_CONTROL_LISTEN`, `JOBMAN_CONTROL_TLS_CERT_FILE`, `JOBMAN_CONTROL_TLS_KEY_FILE` |
| User identity | `JOBMAN_CONTROL_AUTH_MODE`, `JOBMAN_CONTROL_OIDC_ISSUER`, `JOBMAN_CONTROL_OIDC_AUDIENCE` |
| Initial OIDC administrator | `JOBMAN_CONTROL_BOOTSTRAP_SUBJECT`, `JOBMAN_CONTROL_BOOTSTRAP_NAME`, `JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE` |
| Agent identity | `JOBMAN_CONTROL_AGENT_TOKEN_KEY`, `JOBMAN_CONTROL_AGENT_CA_CERT_FILE`, `JOBMAN_CONTROL_AGENT_CA_KEY_FILE` |
| Reconciliation | `JOBMAN_CONTROL_AGENT_STALE_AFTER` |

Clients and agents never receive PostgreSQL credentials. Production OIDC and
agent execution require the TLS and key-handling rules in the
[configuration guide] and [security model].

## Installation

Jobman Control v0.1.x is an evaluation channel. Build from a reviewed source
commit or install a verified release artifact when one is available.

| Distribution | Intended use |
| --- | --- |
| Portable archives | Linux, macOS, and Windows evaluation installs |
| Native packages | DEB, RPM, and APK installs with service scaffolding |
| OCI image | Non-root Linux service deployments on amd64 or arm64 |
| Source build | Development and reviewed internal builds |

Release artifacts include checksums, signatures, provenance, and SBOMs. See
the [installation guide], [deployment guide], and [release guide] before using
them.

## Documentation

| Topic | Resource |
| --- | --- |
| API and examples | [API guide] and [OpenAPI contract] |
| Configuration | [Configuration guide][configuration guide] |
| Deployment and containers | [Deployment guide][deployment guide] and [container guide] |
| Day-two operations | [Operations guide] |
| Identity and trust boundaries | [Security model][security model] |
| Compatibility | [Compatibility reference] |
| Production gaps and acceptance work | [Production-readiness review] |
| Repository and service boundaries | [Architecture guide] |
| Releases and artifact verification | [Release guide][release guide] |

Use the [issue tracker] for reproducible bugs and feature proposals. Report
suspected vulnerabilities privately according to the [security policy].

## Current boundaries

| Area | Current boundary |
| --- | --- |
| Production status | On-premises Slurm, ParallelCluster, restore, failure, load, and security exercises remain to be completed in the department environment |
| Cloud and scheduler management | Control records approved placement facts; it does not provision, resize, or validate Slurm or AWS infrastructure |
| Delivery semantics | Operations are idempotent and replay-safe, but external execution is not promised to be exactly once |
| Credentials | Per-execution secret and cloud-credential brokering is not implemented |
| Filesystems | Cross-user filesystem ownership and ACL provisioning remain an operator responsibility |
| Recovery | Restore holds and reconciliation state exist; automated convergence proof and emergency CA rollover APIs do not |
| Scheduling | Namespace round-robin and FIFO dispatch are implemented; priority classes and target-weighted fairness are not |
| Database operations | Embedded migrations are convenient for evaluation; production still needs a separately controlled least-privilege migration procedure |

The [production-readiness review] is the authoritative checklist. Passing CI
does not replace deployment review or site acceptance.

## Development

Use the included devcontainer or a local Go installation:

```console
make setup
make quick-check
make check
```

`make help` lists test, documentation, contract, container, and release
targets. PostgreSQL integration tests use only the explicit
`JOBMAN_CONTROL_TEST_DATABASE_URL` test setting and otherwise skip.

Jobman's `protocol/` directory is the canonical source for the portable
contract. This repository carries a checksummed snapshot under
`contracts/jobman/v1alpha1/`:

```console
make contracts-check
make contracts-source-check JOBMAN_DIR=../jobman
```

Do not edit copied schemas or fixtures here. Contract changes begin in Jobman
and are synchronized from an exact tagged source commit.

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution requirements.

[API guide]: docs/API.md
[architecture guide]: docs/ARCHITECTURE.md
[compatibility reference]: docs/COMPATIBILITY.md
[configuration guide]: docs/CONFIGURATION.md
[container guide]: docs/CONTAINERS.md
[deployment guide]: docs/DEPLOYMENT.md
[development guide]: docs/DEVELOPMENT.md
[installation guide]: docs/INSTALLATION.md
[issue tracker]: https://github.com/ryancswallace/jobman-control/issues
[OpenAPI contract]: api/openapi-v1alpha1.yaml
[operations guide]: docs/OPERATIONS.md
[production-readiness review]: docs/PRODUCTION_READINESS.md
[release guide]: RELEASE.md
[security model]: docs/SECURITY_MODEL.md
[security policy]: SECURITY.md
