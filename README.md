<!-- markdownlint-disable MD041 -->
<!-- markdownlint-disable MD033 -->
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-control.svg">
  <img alt="Jobman Control" src="assets/logo-dark-control.svg" width="420">
</picture>
<!-- markdownlint-enable MD033 -->

[![Test](https://github.com/ryancswallace/jobman-control/actions/workflows/test.yml/badge.svg)](https://github.com/ryancswallace/jobman-control/actions/workflows/test.yml)
[![CodeQL](https://github.com/ryancswallace/jobman-control/actions/workflows/codeql.yml/badge.svg)](https://github.com/ryancswallace/jobman-control/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/ryancswallace/jobman-control/badge)](https://securityscorecards.dev/viewer/?uri=github.com/ryancswallace/jobman-control)
[![codecov](https://codecov.io/gh/ryancswallace/jobman-control/graph/badge.svg)](https://codecov.io/gh/ryancswallace/jobman-control)

Jobman Control is the pre-release shared control plane for
[Jobman](https://github.com/ryancswallace/jobman). This repository is
pre-release and currently implements named-host subprocess, Slurm,
ParallelCluster target generations, S3/container admission, and transactional
collection/array and cross-target dependency-graph control-plane slices, plus
the first quota, fairness, metrics, audit-export, retention, restore-hold, and
completed-history-import pass.
It is not ready for production use.

## What this slice implements

- A versioned `jobman/v1alpha1` portable workload and job-request contract,
  copied from Jobman's canonical source with schemas, conformance fixtures,
  and a checksum lock.
- A PostgreSQL migration stream for principals, namespaces, memberships,
  immutable target generations, partitions, agents, rotating sessions,
  workload revisions, jobs, runs, executions, inert assignments, idempotency,
  audit events, agent certificates, ordered execution events, durable desired
  actions, capability snapshots, filesystem-log streams and chunks, immutable
  output-artifact metadata, collections and ordered child bindings, and a
  transactional outbox. Later migrations add immutable graphs and edges,
  namespace policy, persistent recovery state, and completed-history
  provenance.
  Composite foreign keys enforce the
  namespace path between related control resources.
- OIDC discovery and signed JWT validation with exact issuer, audience, expiry,
  and stable subject checks. Namespace roles are enforced at repository
  boundaries; namespace administrators can provision other memberships.
- Namespace target registration for host/subprocess and Slurm/Slurm policy,
  native/container runtime allowlists, platform requirements, capabilities,
  configured partitions, an optional approved logical log-store mapping, and
  approved logical artifact-store mappings.
- Revision-checked immutable target-generation rollover, including explicit
  `on-prem` or `aws-parallelcluster` provider facts. New jobs resolve the
  current generation while existing jobs and agents remain pinned to the
  generation they accepted.
- `POST /v1/namespaces/{namespace}/jobs`, with principal/namespace-scoped
  idempotency, target capability validation, default-partition resolution, and
  immutable target-generation pinning. Admission also rejects portable
  features that the selected first-slice backend cannot execute: resource
  requests are Slurm-only, temporary storage is not yet supported, and both
  backends require direct native or target-approved container commands without
  profiles, secrets, extensions, or retries. Declared regular-file artifacts
  may use one approved local/NFS or S3 logical store.
- Transactional collection submission and authorized aggregate reads. All
  children and target bindings are accepted or none are; `maxActive` bounds
  ordinary dispatch, fail-fast creates durable sibling cancellation, and
  compatible `prefer`/`require` Slurm collections carry an immutable array
  task mapping while retaining independent child jobs and outcomes.
- Transactional immutable dependency-graph submission and authorized aggregate
  reads. Control rejects cycles and invalid references, resolves every node's
  placement before committing, gates nodes on explicit terminal-outcome
  predicates across targets, enforces graph concurrency, terminalizes
  unsatisfied branches according to policy, and applies ordinary cancellation
  semantics to every nonterminal node.
- Per-namespace active, queued, collection-size, and graph-size limits with
  serialized admission; revision-checked administrator policy replacement;
  and namespace round-robin assignment selection with FIFO ordering inside
  each namespace. Slurm retains its native scheduling and fair-share policy.
- A bounded-cardinality Prometheus endpoint, append-only ascending audit export
  for operators and namespace administrators, and policy-driven pruning of
  completed idempotency records and published outbox events only. Audit and
  active lifecycle evidence are never age-pruned by this path.
- A persistent recovery epoch and reconciliation hold that stops all new
  assignment after a restore until an operator reconciles target-side facts.
- Explicit dry-run-capable import of quiescent completed SQLite metadata. Each
  record receives a new shared ID and retained source provenance without
  creating a run, execution, or assignment; active work and bulk bytes are
  rejected from this migration boundary.
- `GET /v1/namespaces/{namespace}/jobs/{jobId}`, with namespace membership
  enforced in the read query.
- `GET /v1/namespaces/{namespace}/jobs`, with membership-protected phase
  filtering and stable newest-first keyset pagination.
- Liveness and database/migration-aware readiness endpoints.
- Short-lived, single-use agent enrollment tokens, local-key CSR proof,
  operator-CA-issued mTLS client certificates, atomic certificate rotation,
  database-backed revocation, and legacy inert opaque sessions. Agent private
  keys never leave their host.
- Replay-safe rotating agent sessions, immutable capability/liveness snapshots,
  target active/draining/disabled/retired transitions, assignment freshness
  checks, and conservative stale-observation reconciliation. Silence never
  implies that an accepted execution is safe to reassign.
- A PostgreSQL-coordinated reconciler that creates one run, one execution, and
  one stable, redeliverable assignment envelope for each eligible job/agent.
  Array children are affinity-bound to one generation-specific agent and may
  be offered together while Slurm enforces the collection concurrency bound.
- Durable compare-and-swap assignment acceptance. Receipt of an offer is
  inert; only the replay-stable launch authorization returned after acceptance
  permits an agent to start target-side work.
- Monotonic, idempotent process and scheduler events that atomically advance
  execution, run, and job snapshots. Slurm events preserve submitted,
  uncertain, observed, and terminal scheduler evidence; job reads expose the
  latest native job ID, normalized state, cluster, reason, and observation
  time.
- Idempotent stdout/stderr chunk manifests with gap-safe out-of-order delivery.
  Agents place immutable, checksummed bytes in the target-approved local/NFS or
  S3 store before committing metadata; PostgreSQL exposes only each contiguous
  source prefix and never stores or proxies log bytes.
- Membership-authorized job log manifests for clients that resolve logical
  store/version/key references through their own platform-specific mounts.
- Target-approved regular-file input/output mappings pinned into each effective
  execution, transactional validation of terminal output metadata, and
  membership-authorized job artifact manifests. PostgreSQL never stores or
  proxies artifact bytes or physical paths.
- Idempotent job cancellation: unaccepted work is withdrawn without launch;
  accepted work receives a redeliverable desired action with a separate
  durable acknowledgement.
- An explicitly enabled, loopback-only development identity for local testing.

This repository remains the control-plane service: it never starts a
subprocess, invokes Slurm, opens SSH, or executes workload content. The
host-local `jobman-agent`, subprocess runner, and Slurm CLI adapter live in the
Jobman repository and consume these APIs. Jobman v1.7.0 introduced the
compatible `jobman shared` client for target inspection, idempotent submission,
paginated job inspection, wait, cancellation, verified filesystem-log
read/follow, artifact discovery, collection submit/show, graph
submit/show/cancel, completed-history import, and client-side recovery of an
uncertain single-job submission.

```text
Jobman request -> OIDC/development identity -> namespace authorization
               -> target + generation capability resolution
               -> accepted job transaction
               -> coordinator claim (FOR UPDATE SKIP LOCKED)
                  ├── run + planned execution
                  ├── immutable effective execution
                  ├── inert assignment offer
                  ├── system audit event
                  └── durable outbox event
               -> agent journals offer -> atomic acceptance
               -> launch authorization -> ordered process or scheduler events
               -> immutable log objects -> ordered metadata commits
               -> immutable outputs -> transactional artifact metadata
               -> terminal job outcome
```

## Documentation

Start with the [documentation index](docs/README.md). Separate guides cover the
[architecture](docs/ARCHITECTURE.md), [API](docs/API.md),
[configuration](docs/CONFIGURATION.md), [development](docs/DEVELOPMENT.md),
[deployment](docs/DEPLOYMENT.md), [operations](docs/OPERATIONS.md),
[security model](docs/SECURITY_MODEL.md),
[production readiness](docs/PRODUCTION_READINESS.md), and
[compatibility](docs/COMPATIBILITY.md).

## Development quick start

The service currently exercises PostgreSQL 17.6 in CI. Start an isolated local
database using the pinned Compose definition:

```sh
docker compose up -d postgres
```

In another terminal:

```sh
export JOBMAN_CONTROL_DATABASE_URL='postgres://postgres:jobman-control-development@127.0.0.1:5432/jobman_control?sslmode=disable'
export JOBMAN_CONTROL_AUTH_MODE=development
export JOBMAN_CONTROL_DEVELOPMENT_AUTH=true
export JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE=research
make run
```

Register the target named by the locked job-request fixture:

```sh
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: target-workstation-a-001' \
  --data-binary '{
    "apiVersion":"jobman.control/v1alpha1",
    "kind":"Target",
    "metadata":{"name":"workstation-a"},
    "spec":{"kind":"host","executionBackend":"subprocess",
      "runtimes":["native"],"operatingSystems":["linux"],
      "architectures":["amd64"],
      "logStore":{"name":"department-nfs","version":1}}
  }' \
  http://127.0.0.1:8080/v1/namespaces/research/targets
```

Submit the locked example request:

```sh
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: example-submit-001' \
  --data-binary @contracts/jobman/v1alpha1/conformance/valid/job-request-minimal.json \
  http://127.0.0.1:8080/v1/namespaces/research/jobs
```

Resending the same canonical request with the same key returns the same job and
`Idempotency-Replayed: true`. Reusing that key for different request intent
returns HTTP 409. The response's `Location` header identifies the GET endpoint.

The complete first-slice API is described by
[`api/openapi-v1alpha1.yaml`](api/openapi-v1alpha1.yaml).
The Compose database is disposable development infrastructure; never reuse its
credential outside this checkout.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `JOBMAN_CONTROL_DATABASE_URL` | required | PostgreSQL connection string; never returned or intentionally logged |
| `JOBMAN_CONTROL_AUTH_MODE` | required unless development compatibility flag is set | `development` or `oidc` |
| `JOBMAN_CONTROL_DEVELOPMENT_AUTH` | `false` | Compatibility switch selecting loopback-only `development` mode |
| `JOBMAN_CONTROL_LISTEN` | `127.0.0.1:8080` | HTTP/TLS listener; development mode is loopback-only |
| `JOBMAN_CONTROL_DEVELOPMENT_ISSUER` | `jobman-control-development` | Fixed development principal issuer |
| `JOBMAN_CONTROL_DEVELOPMENT_SUBJECT` | `local-developer` | Fixed development principal subject |
| `JOBMAN_CONTROL_DEVELOPMENT_NAME` | `local-developer` | Development principal display name |
| `JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE` | `default` | Namespace created for the development principal |
| `JOBMAN_CONTROL_OIDC_ISSUER` | required in OIDC mode | Exact HTTPS issuer used for discovery and token validation |
| `JOBMAN_CONTROL_OIDC_AUDIENCE` | required in OIDC mode | Required JWT audience |
| `JOBMAN_CONTROL_AGENT_TOKEN_KEY` | required in OIDC mode | Unpadded base64url encoding of at least 32 secret bytes used to derive agent credentials; never logged |
| `JOBMAN_CONTROL_BOOTSTRAP_SUBJECT` | unset | Optional initial OIDC administrator subject; set with bootstrap name and namespace |
| `JOBMAN_CONTROL_BOOTSTRAP_NAME` | unset | Display name for the optional initial administrator |
| `JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE` | unset | Namespace for the optional initial administrator |
| `JOBMAN_CONTROL_TLS_CERT_FILE` | unset | TLS certificate chain; required with the key on a non-loopback OIDC listener |
| `JOBMAN_CONTROL_TLS_KEY_FILE` | unset | TLS private-key file; required with the certificate |
| `JOBMAN_CONTROL_AGENT_CA_CERT_FILE` | unset | Agent client-certificate CA; enables execution APIs and requires server TLS |
| `JOBMAN_CONTROL_AGENT_CA_KEY_FILE` | unset | Private key for the agent CA; keep readable only by the service identity |
| `JOBMAN_CONTROL_AGENT_STALE_AFTER` | `2m` | Silence interval before accepted/running observations are marked stale |
| `JOBMAN_CONTROL_MIGRATE_ON_START` | `true` | Apply embedded forward-only migrations before serving |

Development mode defaults to no TLS or token authentication. Its loopback
restriction is a safety boundary, and it uses a fixed development-only
agent-token key. Exercising the agent execution slice still requires an HTTPS
server certificate and the agent CA pair, even in development mode.
OIDC mode validates bearer JWTs using provider discovery and requires TLS for a
non-loopback listener. The optional bootstrap identity is operator-controlled;
normal membership changes use the audited API. Clients and agents must never
receive PostgreSQL credentials.

Opaque agent sessions are intentionally restricted to compatibility operations
and cannot poll assignments or authorize execution. Execution endpoints require
a CA-verified client certificate whose agent, key digest, expiry, revocation,
target, and target generation are rechecked in PostgreSQL.

See [the configuration guide](docs/CONFIGURATION.md) for validation rules and
production secret-handling requirements.

## Contract ownership

Jobman's `protocol/` directory is canonical. This repository carries a
mechanically copied, checksummed snapshot under
`contracts/jobman/v1alpha1/`, pinned to Jobman v1.7.0 and verified unchanged in
v1.8.0. The source lock is recorded in
`contracts/jobman/v1alpha1/SOURCE.md`.

```sh
make contracts-source-check JOBMAN_DIR=../jobman
make contracts-sync JOBMAN_DIR=../jobman
```

`contracts-source-check` detects drift from a neighboring Jobman checkout;
`contracts-check` verifies the snapshot against its checked-in
`checksums.txt` without requiring that checkout. Do not edit copied contract
source, schemas, or fixtures in this repository. A refresh must originate in
Jobman and record the exact tagged source version and commit.

## Validation

```sh
make setup
make quick-check
```

PostgreSQL integration tests create and remove a uniquely named schema in an
explicit test database. They are skipped unless the following setting is
present:

```sh
export JOBMAN_CONTROL_TEST_DATABASE_URL='postgres://postgres:password@127.0.0.1:5432/jobman_control_test?sslmode=disable'
make integration-test
```

`make check` adds coverage, workflow and shell validation, documentation,
reachable-vulnerability analysis, cross-platform release builds, and container
and release-configuration checks.

## Deliberate limitations

- One configured OIDC issuer and JWT bearer tokens are supported; CLI browser
  login/device flow, multiple issuers, service principals, revocation feeds,
  and proxy-terminated TLS policy remain future work.
- Certificate revocation is currently exercised by rotation and agent/target
  state checks. Administrative emergency revocation and CA rotation APIs and
  runbooks remain future work.
- Target generations can roll over immutable policy with revision and
  idempotency checks. Control records ParallelCluster region/name facts but
  does not provision, inspect, resize, or validate AWS infrastructure;
  identity mappings and administrative agent revocation remain future work.
- This slice supports one execution per child job, process or normalized Slurm
  scheduler lifecycle facts, local/NFS or S3-backed stdout/stderr metadata, one
  logical regular-file artifact store per workload, and explicit collections
  with optional Slurm-array compilation. SSH bootstrap, containers, S3 byte
  transfer, and native array submission live in the Jobman agent repository;
  Control never performs those effects. Standalone Slurm, cross-user
  filesystem ACL provisioning, per-execution cloud credential brokering,
  retries, and collection-level cancellation remain future work. Graph
  cancellation is implemented; native Slurm dependencies are intentionally not
  authoritative.
- For a target with `logStore` configured, Control deliberately rejects the
  terminal process or scheduler event until both stdout and stderr streams have complete,
  contiguous manifests. A missing or misconfigured agent artifact store
  therefore leaves the execution nonterminal for retry and operator repair
  instead of silently losing its logs.
- The coordinator uses bounded periodic scans, namespace round-robin claims,
  and FIFO ordering within each namespace. It marks silent accepted/running
  observations stale without guessing terminal state. Priority classes,
  target-level weighted fairness, outbox publication, and automatic destructive
  repair/reassignment policy remain future work.
- Collection aggregation is derived from child jobs. Native arrays require one
  compatible target generation, partition, agent, and portable resource shape;
  large-array scale/chaos behavior and real Slurm/ParallelCluster acceptance
  remain unverified.
- Opaque agent credential derivation depends on the configured token key; a
  production rotation and recovery runbook is not yet defined.
- Completed idempotency records and published outbox events use bounded
  per-namespace retention. Audit, jobs, executions, unpublished delivery, and
  artifact bytes require separate institutional retention procedures.
- Automatic migrations are convenient for this development slice. A separate
  least-privilege migration role and deployment workflow are required before
  production use.
- The PostgreSQL version shown above is the current CI target, not yet a
  published support policy.
- Production load/chaos, backup/restore, on-premises Slurm, and AWS
  ParallelCluster acceptance have not been run in the department environment;
  see the production-readiness review before treating this initial pass as a
  supported service.
