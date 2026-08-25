# Operations

## Health and observability

`GET /healthz` reports only that the HTTP process is serving. `GET /readyz`
performs a bounded repository readiness check and returns unavailable when the
database cannot serve the request. Neither endpoint authenticates or exposes
configuration.

Service logs are structured JSON on standard error. Current logs intentionally
avoid request bodies, database URLs, tokens, certificate details, workload
commands, environment values, process output, and artifact bytes. Collect and
retain them according to the sensitivity of job identifiers and namespace
activity.

`GET /metrics` exposes Prometheus text with fixed job-phase and agent-status
labels plus unpublished-outbox count, stale-execution count, oldest accepted
job age, recovery-hold state, and restore epoch. It is unauthenticated so a
scraper can use the same listener, but counts can reveal activity; restrict the
route at the network or reverse-proxy layer. Do not add namespace, job, target,
principal, command, or artifact-key labels.

Distributed tracing, outbox publication, automatic stale-work repair, and
formal service-level objectives remain future work. Control marks
accepted/running observations stale after the configured silence interval;
that is evidence degradation, not a terminal state. Operators should alert on
readiness failure, restart loops, PostgreSQL saturation and replication
health, certificate expiry, backup failure, recovery hold, sustained queue
age, unpublished outbox growth, and executions that remain nonterminal because
artifact manifests cannot complete.

## Namespace policy and audit export

Every namespace starts with 100 active jobs, 10,000 queued/nonterminal jobs,
10,000 collection items, 10,000 graph nodes, seven-day completed-idempotency
retention, and seven-day published-outbox retention. A namespace administrator
may replace these values through the policy API with the current revision
ETag. Retention intervals must be between one hour and 365 days. Admission
locks the policy row, so concurrent group submissions cannot each pass a stale
quota check. Active-job limits gate assignment, not Slurm allocation; native
partition and fair-share policy still applies afterward.

Operators and namespace administrators export audit pages in ascending ID
order. Persist the returned `nextAfterId` only after the page is safely stored,
then resume with `afterId`. Ordinary cleanup never deletes audit events. The
deployment still needs institutional retention, integrity protection, access
logging, and an export destination appropriate for potentially sensitive job
identifiers and activity.

## Migrations and upgrades

Every binary embeds a checksummed ordered migration set. Back up the database,
stop writes or establish an application-consistent maintenance boundary, apply
the candidate migrations, verify readiness with migration-on-start disabled,
then deploy identical replicas. The service rejects missing, changed, or
unknown ledger entries.

Migrations are forward-only. Rollback means restoring the pre-upgrade database
backup and matching binary together. Never edit a migration already applied to
a shared environment.

## Backup and restore

Back up the entire PostgreSQL database with a tool appropriate to the topology,
retain encrypted copies separately, and test restore regularly. A complete
recovery also needs the exact service release, server TLS configuration, agent
CA certificate and private key, agent token key, OIDC settings, and the
external local/NFS or S3 artifact stores referenced by manifests.

Database backup and artifact-store backup are not transactionally atomic.
Preserve immutable artifact objects at least as long as their manifests and
prefer capturing filesystem snapshots or S3 versioned backups after the
database recovery point. A
restored manifest that references a missing or changed object fails client
verification by design.

Restore into an isolated environment first. Verify the migration ledger, row
counts, namespace authorization, a sample job, agent certificate state, and
checksummed log objects before returning traffic.

### Post-restore assignment hold

A restored database can be older than native processes, Slurm jobs, and
artifact objects. Stop all Control replicas before restore. After restoring and
before exposing the service, use the privileged migration/operator database
role to enter a new recovery epoch:

```sql
BEGIN;
SELECT singleton, reconciliation_hold, restore_epoch
FROM service_recovery_state FOR UPDATE;
UPDATE service_recovery_state
SET reconciliation_hold = true,
    restore_epoch = restore_epoch + 1,
    reason = 'point-in-time restore reconciliation',
    updated_at = transaction_timestamp()
WHERE singleton;
COMMIT;
```

Starting Control while this hold is set is safe: reads, agent reconnects, and
evidence delivery continue, but the coordinator cannot create any new
assignment. Verify the hold metric, reconnect every affected agent generation,
reconcile subprocess identities and Slurm accounting, check required artifact
manifests, and classify every job whose target-side state may be newer than the
database. Preserve external change-control evidence for the direct database
operation.

Only after that inventory is complete should the operator release assignment:

```sql
BEGIN;
SELECT singleton, reconciliation_hold, restore_epoch
FROM service_recovery_state FOR UPDATE;
UPDATE service_recovery_state
SET reconciliation_hold = false,
    reason = NULL,
    updated_at = transaction_timestamp()
WHERE singleton AND reconciliation_hold;
COMMIT;
```

The restore epoch remains monotonic and visible in metrics. There is currently
no service-administrator API or automatic proof that reconciliation is
complete; the direct, externally audited database procedure and a tested
department runbook are production prerequisites.

## Operational retention

Each coordinator cycle removes at most a bounded batch of completed
idempotency records and already-published outbox events older than the owning
namespace's policy. Interrupted pruning is safe to repeat. It never removes
incomplete idempotency, unpublished delivery, audit, job/run/execution state,
artifact manifests, or external bytes. Plan separate state-aware retention for
those resources and monitor database growth; shortening these two operational
windows is not general user-data deletion.

## Incident handling

- For database loss or corruption, stop Control writes and preserve evidence
  before attempting restore.
- For suspected agent credential compromise, disable or replace the affected
  agent/target records through controlled database operations until an
  administrative revocation API exists; retain an audit record outside the
  service if direct intervention is necessary.
- For agent CA compromise, stop enrollment and execution APIs, rotate trust
  material, and re-enroll all agents. The current slice has no transparent CA
  rollover.
- For log-store failure, repair the approved mapping or storage permissions and
  allow durable agent metadata to retry. Do not force terminal state while
  required streams are incomplete.
- For S3 failure or credential loss, preserve bucket/version history and the
  agent spool, restore the exact configured prefix/owner/checksum semantics,
  and let immutable transfers replay. Never replace a conflicting key merely
  to advance a manifest.
- For a stale execution, first determine whether the agent, host, process, or
  scheduler object still exists. Do not reassign or edit it terminal merely
  because its confidence is stale; preserve the agent spool and Slurm evidence
  for reconciliation.
- To stop new placement without disrupting accepted work, move the target to
  `draining`. Use `disabled` for a stronger administrative stop. Retiring a
  target is terminal and does not erase historical generations or jobs.
- For OIDC outage, existing bearer authentication may fail as cached provider
  material expires. Do not enable development authentication as a production
  bypass.

Record exact times, versions, safe identifiers, and operator actions without
copying secrets or workload content into tickets.
