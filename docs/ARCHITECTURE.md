# Architecture

Jobman Control is the shared control plane for Jobman. It coordinates durable
intent and authorized metadata; it does not execute workloads or proxy bulk
log and artifact bytes.

```text
Jobman client ---- OIDC/TLS ----> Jobman Control ---- TLS ----> PostgreSQL
                                      ^   |
                                      |   | assignments, actions, metadata
                                      |   v
                              mTLS Jobman agent
                                      |
                                      v
                           subprocess or Slurm backend
                                      |
                                      v
                      local/NFS or S3 artifact object store
```

## Repository boundaries

The Jobman repository owns the portable workload and agent protocol, user CLI,
agent, target-side execution, runtime adapters, artifact bytes, and client-side
artifact path mapping. Jobman Control owns the HTTP control API, OIDC and mTLS
authentication, namespaces and authorization, target generations, placement
policy, PostgreSQL schemas and migrations, reconciliation, audit records,
desired actions, and artifact manifests.

Jobman Diagnose remains a read-only interpretation companion. Control must not
gain diagnostic model providers or target mutation through that project.

The copied `contracts/jobman/v1alpha1` tree is a temporary, checksummed
pre-release snapshot. Source changes originate in Jobman's `protocol/`
directory. Control projects validated wire data into domain types so protocol
implementation details do not spread into persistence and policy.

## State and consistency

PostgreSQL is authoritative for principals, namespace membership and policy,
targets, agents, workloads, collections and child bindings, immutable graphs
and edges, jobs, runs, executions, assignments, desired actions, events, audit
history, idempotency records, recovery state, completed-history provenance,
and artifact manifests. Mutations
use constraints, row locks, compare-and-swap transitions, and request digests.
The service promises replay-safe intent, not exactly-once external execution.

The coordinator uses bounded claims and PostgreSQL locking, so more than one
service instance may reconcile safely. HTTP state is database-backed. All
replicas must use the same service version, agent token key, agent CA, OIDC
configuration, and migration set.

Migrations are embedded, forward-only, checksummed, serialized with a
PostgreSQL advisory transaction lock, and applied atomically. With automatic
migration disabled, startup requires the database ledger to exactly match the
binary; an older binary fails closed against a newer database.

For Slurm executions, immutable ordered events are the historical record while
the execution row projects the latest native job ID, normalized scheduler
state, cluster, reason, and observation time. Queueing does not advance the
portable lifecycle to `running`; a normalized scheduler `running` observation
does. Terminal scheduler evidence and its portable process result advance the
execution, run, and job in the same transaction.

Each accepted or running execution carries observation confidence. Periodic
reconciliation changes fresh evidence to `stale` after agent silence, but does
not infer a failure, release ownership, or assign a duplicate execution.
Scheduler ambiguity and runner loss remain distinct `uncertain` and `lost`
facts.

Collections are accepted in one transaction after every child resolves a
target generation. Ordinary children use collection-row locking to enforce
`maxActive`. Compatible Slurm collections instead carry a signed task
index/count/concurrency binding in each effective execution; one agent receives
the complete group and performs the array optimization. PostgreSQL continues to
track every child, scheduler task ID, cancellation intent, and outcome
independently. Aggregate state is derived from those child jobs.

Graphs use the same independently observable job rows as single submissions
and collections. Submission seals each workload, resolves every target
generation, inserts nodes and edges, and records audit/outbox evidence in one
transaction. The coordinator dispatches a node only when every upstream is
terminal and satisfies its explicit predicate and the graph remains below
`maxActive`. Terminal completion propagates unsatisfied downstream branches to
durable skipped, canceled, or blocked dispositions in the same transaction.
This central model is what permits dependencies across unrelated hosts and
Slurm clusters; native scheduler dependencies may be added only as an
optimization.

Namespace policy rows are locked during admission and assignment. They bound
queued expansion and active work without attempting to replace Slurm resource
scheduling. The coordinator selects the least-recently dispatched eligible
namespace, then preserves stable FIFO order within it. A singleton recovery
row gates all assignment after restore. Operational cleanup deletes only
completed idempotency records and published outbox events older than each
namespace policy; it never deletes audit or active lifecycle state.

Completed-history import is a separate quiescent path. It validates the same
portable workload and target placement but writes only a terminal job with a
new ID and immutable SQLite provenance. It creates no runnable run or
execution, and Control never copies the standalone database or artifact bytes.

## Identity and authorization

Client requests authenticate through one configured OIDC issuer and audience.
The stable issuer/subject pair identifies a principal. Namespace membership is
checked in repository queries, normally returning not-found across namespace
boundaries to avoid existence disclosure.

Agents enroll with short-lived single-use tokens, prove possession of a local
private key, and use CA-issued mTLS certificates. Certificate serial, key
digest, expiry, revocation state, target, and pinned target generation are
rechecked against PostgreSQL. A generation rollover does not invalidate agents
that must finish earlier accepted work; those agents cannot receive work
resolved to the new generation. The rotating opaque session is not execution
authority but
must remain current for new assignment eligibility. Immutable capability and
liveness snapshots are intersected with target policy. Target draining stops
new assignment while allowing already accepted work, events, logs, and desired
actions to finish. Agents may report only their assigned executions and the
target-approved artifact store.

Development identity is a loopback-only convenience and is not a production
authentication mode.

## Logs and artifacts

Agents write immutable stdout and stderr chunks to the target-approved logical
local/NFS or S3 store before committing metadata. Control validates the exact
namespace/job/execution/stream/sequence key, store name and mapping version,
offset continuity, length bounds, and SHA-256 digest format. It exposes only a
contiguous prefix. Filesystem clients resolve the logical mapping through their
local Jobman profile and verify every object before display; the current client
returns metadata only for S3-backed logs.

For a log-enabled target, terminal execution state requires complete stdout
and stderr manifests. A broken store mapping therefore leaves durable work for
retry or operator repair rather than silently accepting log loss.

Portable regular-file artifacts use logical `artifact://STORE/KEY`,
`inputs:/...`, and `outputs:/...` references. Admission pins one approved store
mapping into the effective execution. The agent verifies and stages inputs,
then atomically publishes outputs. Control validates terminal metadata against
the declared name, destination key, mapping version, size, and digest and
stores it in the same transaction as the terminal event. Authorized clients
can list those logical results, but bytes remain outside Control.

## Current process model

The service process owns one bounded PostgreSQL pool, one HTTP server, and a
periodic assignment reconciler. Shutdown stops new HTTP work, cancels the
coordinator, drains requests within the configured internal timeout, and
closes the database pool. Liveness is process-local; readiness checks the
repository with a bounded context. The same bounded coordinator cycle also
marks stale evidence and prunes a small batch of eligible operational records;
durable rows remain authoritative if a cycle is interrupted.
