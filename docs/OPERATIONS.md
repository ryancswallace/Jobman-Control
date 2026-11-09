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

Distributed tracing, automatic stale-work repair, and formal service-level
objectives remain future work. Control marks
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

Every binary embeds a checksummed ordered migration set. Back up the database
and stop writes or establish an application-consistent maintenance boundary.
Use the candidate binary and a dedicated migration login:

```sh
jobman-control migrate --database-url-file /run/jobman/migrator-url
```

Use a dedicated database or dedicated trusted schema. Restrict `CONNECT` to
deployment roles, remove database and schema `CREATE` and `TEMP` grants from
untrusted users and PUBLIC, and give only the migrator ownership of application
objects and `CREATE` in the application schema. For existing deployments,
transfer ownership before the upgrade. Give runtime and operator logins no
membership in the migrator role. Configure each login's search path to the
application schema and use protected regular URI files with mode `0600` or
`0400`. Use `sslmode=verify-full` and the deployment's trusted PostgreSQL root
for remote database connections.

After applying migrations, grant the runtime login schema usage, the DML and
sequence access required for application data, and read-only access to
`schema_migrations`. Grant it `SELECT, UPDATE (singleton)` on
`service_recovery_state` for the assignment and directory row locks. The
singleton constraint prevents changing the lock column. Omit all write
privileges on the remaining recovery-state columns and on
`service_operator_audit`. The operator login needs schema usage, SELECT on
`schema_migrations`, `service_recovery_state` and `service_operator_audit`, and
`EXECUTE` on `jobman_control_set_recovery(bigint, bigint, boolean, text)`.
This function starts with no PUBLIC execution grant. Use individual operator
logins or short-lived role-bound credentials so `session_user` identifies the
actor. Confirm the runtime cannot create application objects, change a hold,
or edit an operator audit row before returning traffic.

Set `migrateOnStart` to false in the service configuration (or
`JOBMAN_CONTROL_MIGRATE_ON_START=false` for legacy environment configuration).
Verify readiness with the runtime login, then deploy identical replicas. The
service rejects missing, changed, or unknown ledger entries.

Migrations are forward-only and commit with the ledger in one transaction. If
the command fails or loses its connection, check the ledger with the candidate
before continuing. Repeating the same candidate is safe. Rollback means
restoring the pre-upgrade database backup and matching binary together, with
replicas stopped. Never edit a migration already applied to a shared
environment.

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
before exposing the service, inspect the restored epoch using the operator
login. The new epoch must exceed both the restored value and the highest
epoch previously served to clients. Record the highest served epoch outside
the backed-up database so restoring a stale backup cannot reuse it. The
example advances restored epoch 1 past the last served epoch of 6.

```sh
jobman-control recovery status --database-url-file /run/jobman/operator-url
jobman-control recovery hold --database-url-file /run/jobman/operator-url \
  --expected-epoch 1 --new-epoch 7 --reason restore-drill-007
```

Reads, agent reconnects, and evidence delivery continue under the hold. The
coordinator can redeliver existing assignments with their original identities,
but cannot create new ones. Entering a hold waits for assignment transactions
already in flight. Verify the hold metric, reconnect every affected agent generation,
reconcile subprocess identities and Slurm accounting, check required artifact
manifests, and classify every job whose target-side state may be newer than the
database. Pause agent traffic during the isolated inventory if existing offers
must not start. Preserve the audit rows and supporting reconciliation evidence
with the external change record.

Only after that inventory is complete should the operator release assignment:

```sh
jobman-control recovery release --database-url-file /run/jobman/operator-url \
  --expected-epoch 7 --reason restore-drill-007
```

State and the authenticated actor, epoch and change reference in
`service_operator_audit` commit together. The epoch remains visible in metrics.
The operator supplies a short ticket or change ID in `--reason`, without
incident contents or secrets. If a command times out, returns a conflict or
loses its response, inspect status and audit before retrying. Leave the hold
in place when reconciliation is incomplete. A matched older binary and
backup that predate these commands require their own documented hold
procedure before reopening traffic.

For the disposable logical restore drill, point
`JOBMAN_CONTROL_TEST_DATABASE_URL` at a database reserved for integration tests.
Configure `pg_dump` and `pg_restore` to connect to that same PostgreSQL server
with disposable administration credentials. Set
`JOBMAN_CONTROL_TEST_RESTORE=1` and run
`go test -race -count=1 -run TestRecoveryRestoreDrillIntegration ./internal/store/postgres`.
The drill creates and removes its own schema and restore database.

## Lifecycle outbox export

The terminal monitoring publisher owns `monitoring.job_terminal.v1` and its
feed retention and reset protocol. The lifecycle export covers these separate
transactional records:

| Topic | Recorded evidence |
| --- | --- |
| `job.accepted` | Job identity, namespace, revision and digests |
| `collection.accepted` | Collection identity and item count |
| `graph.accepted` | Graph identity, node count and edge count |
| `history.imported` | Job and local source identities |
| `assignment.offered` | Execution, agent and delivery identities and effective digest |
| `execution.observation_stale` | Execution and agent identities |
| `target.generation_created` | Target and new generation identities and number |
| `target.state_changed` | Target identity and state |

Provision a separate archive login with schema usage and only EXECUTE on
`jobman_control_export_lifecycle(integer)` and
`jobman_control_ack_lifecycle(bigint, uuid, text)`. Restrict this
cross-namespace export to a trusted operations boundary. Use an encrypted,
access-controlled destination and protect the payload as operational evidence.
Assign one archive consumer per database lineage and an on-call owner for
delivery and storage. Supply the destination and retention policy before
enabling that consumer.

Pull at most 100 records per request in a short read-committed statement.
The order is deterministic within a batch. Pending records can appear late
because source transactions commit concurrently, so avoid a high-water cursor
based on creation time. Deduplicate by database lineage and event UUID, across
all restore epochs. Store the record and a stable receipt durably before
acknowledging it with the epoch from the batch.

```sql
SELECT jobman_control_export_lifecycle(100);
-- Bind the observed epoch, event UUID and durable receipt reference.
SELECT jobman_control_ack_lifecycle($1, $2, $3);
```

An acknowledgement marks one known lifecycle event published and audits the
login, event, epoch and receipt reference atomically. Repeating the same receipt
returns false after a committed acknowledgement, including after ordinary
pruning. A different receipt conflicts. A hold or epoch change rejects the
acknowledgement. Pull again after reconciliation, then reuse the durable receipt
if the same UUID is present. Leave pending records intact when a consumer is
unavailable. Unknown topics remain pending for explicit review.

Scrape `jobman_control_lifecycle_backlog` and
`jobman_control_lifecycle_backlog_oldest_seconds` separately from the monitoring
feed metrics. The [example rules](examples/lifecycle-alerts.yml) page the
operator when pending records age beyond 15 minutes or exceed 10,000 for five
minutes. Use `promtool test rules lifecycle-alerts.test.yml` from the examples
directory to exercise consumer outage and recovery. Configure a separate
scrape-health alert. During an outage, inspect consumer reachability and its
last durable receipt, check database capacity, and resume with the same
deduplication key. If headroom is threatened, pause admission under change
control and preserve pending evidence. Set the deployment thresholds and
capacity budget to match the approved delivery window.

## Operational retention

Each coordinator cycle removes at most a bounded batch of completed
idempotency records and already-published outbox events older than the owning
namespace's policy. Interrupted pruning is safe to repeat. It never removes
incomplete idempotency, unpublished delivery, audit, job/run/execution state,
artifact manifests, or external bytes. Plan separate state-aware retention for
those resources and monitor database growth; shortening these two operational
windows is not general user-data deletion.

## Agent CA rollover and offboarding

Keep the HTTPS server identity and its client trust configuration separate
from agent client-certificate issuance. Record the incident or change ID,
the fingerprints of both agent CA certificates, the expected replica set,
the affected agent IDs and their target generations, and a cutoff time.
Inventory accepted/running executions and last known observations before
changing trust. Agents generate their own leaf keys.

When both CAs remain trusted, stage the public replacement certificate as
`agentOverlapCAFile` everywhere. Verify each replica accepts both roots before
the signing cutover. Deliver the replacement CA key to the controlled replica
secret mounts with the same restricted access used for the original. Switch
all replicas to sign with the new CA and verify the old CA through the
overlap setting. Prove new credentials on every replica. Existing leaf
certificates remain usable until renewed or expired. Agent renewal keeps the
same enrolled agent and generation and rotates its leaf key. Prior leaf
credentials are revoked in PostgreSQL. Allow no more than one normal leaf
lifetime for this overlap, or set a shorter change window and confirm the
required renewals before closing it. At cutoff, remove the old root and its
key from every replica and replace those processes so existing connections
and TLS session tickets cannot retain the old trust. Reverify health and
request behavior against each replica, then remove staged old key copies.

If the old CA key is suspected compromised, remove its trust immediately.
Stop or remove **all** old-trust replicas from service and close their
connections before reopening with the replacement root. Keep normal TLS
verification enabled and keep assignment under a recovery hold until
affected executions and offers are inventoried. Agents with unexpired leaves
from a removed CA cannot renew through that CA. Reconcile their durable local
spools and native scheduler evidence separately, then enroll clean agents
under the replacement CA. Keep original execution IDs and any unresolved
outcomes. Do not use the single-agent expired-certificate recovery procedure
as a way to re-trust an old CA. A rollback to the old issuer is allowed only
when it remains trusted and a reviewed replica configuration still verifies
both roots. After compromise, recover using only the clean trust set.

To offboard one agent, use an authorized PostgreSQL operator login with
`EXECUTE` on `jobman_control_retire_agent(uuid, uuid, text)`. Apply a short
statement deadline and bind the exact enrolled agent UUID, its immutable
target generation UUID and a change reference.

```sql
SELECT jobman_control_retire_agent($1, $2, $3);
```

Retirement and revocation of that agent's sessions and leaves are atomic.
The operator audit records the authenticated login and exact generation.
A repeat for the same retired agent returns false. An unknown pairing fails.
Retirement preserves assignments, observations and results. Allow already
authenticated requests to drain, or close the replicas' connections during
a compromise cutover. Verify the retired credentials fail against every
replica, including a replay of enrollment or recovery. Track the remaining
native processes separately and record any unknown result as unresolved.
Retirement is irreversible through the operator interface. Re-enroll with
fresh authorization after reconciliation and use a clean agent state root.

## Incident handling

- For database loss or corruption, stop Control writes and preserve evidence
  before attempting restore.
- For a suspected agent or CA compromise, follow the audited retirement and
  CA cutover procedures above. Use the same change reference in the separate
  native-work reconciliation record.
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
