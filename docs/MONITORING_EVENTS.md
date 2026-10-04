# Durable monitoring events

Control publishes a minimal, durable terminal-transition feed for separately
registered Dashboard services. This is a background ingestion capability, not a
user read or job polling shortcut. The feature is advertised as
`durable-monitoring-events` in `/v1/capabilities`.

## Service authority

Use the existing actual-mTLS, pinned Ed25519 `Jobman-Delegation` transport and
short-lived assertion checks from [delegation](DELEGATION.md), with this separate
claim shape:

- `operation` is `events.read`, `mode` is `worker`, and `sub` equals `iss` (the
  registered service ID).
- Omit `actor` entirely. A null, empty, or populated actor field is rejected.
- `namespaceIds` is an explicit, unique subset of the current service key's
  registered namespace UUID whitelist; no wildcard exists.
- The service key must explicitly permit `events.read`. No human role confers it.

Each feed read checks the current key, certificate binding, operation, namespace
policy, assertion expiry, and recovery issuance floor in its database snapshot.
The replay ledger accepts each assertion ID once across both identity shapes.
No directory account or user alias is invented for a service assertion. Ordinary
OIDC users and represented-user assertions cannot read this feed. Service-only
assertions cannot read `/me`, jobs, logs, artifacts, reports, or diagnostics, and
cannot mutate execution or administration.

Feed ingestion remains possible during a directory outage because it uses
independent operator service authority. Dashboard must perform current
represented-user authorization, including directory freshness, before matching
or delivering user-visible notifications or displaying event details.

## Checkpoint and page contract

`GET /v1/monitoring-events/checkpoint` accepts no query. Its response is
`apiVersion: jobman.control/v1alpha1`, `kind: MonitoringCheckpoint`, with:

| Field | Meaning |
| --- | --- |
| `controlInstanceId`, `recoveryEpoch` | Actual persistent source identity and restore epoch. |
| `asOf` | Database snapshot/source-clock activation boundary. |
| `headCursor` | Continuation after all currently committed feed publications. |
| `oldestCursor` | Continuation immediately before the retained feed range. |
| `retentionSeconds` | Independently configured feed retention. |
| `backlogCount` | Unpublished terminal outbox count in the asserted namespace set. |
| `oldestUnpublishedRecordedAt` | Earliest pending transition recording time in that set, when any. |

`GET /v1/monitoring-events?cursor=...&limit=100` returns
`kind: MonitoringEventList`, the same checkpoint fields, and `items`, `nextCursor`,
`hasMore`. The limit is 1–200. Cursor is required; duplicate, unknown, malformed,
or invalid parameters return 400. All int64 counters are decimal JSON strings.
Cursors are opaque, bounded, canonical tokens bound to Control instance, recovery
epoch, registered service ID, and the exact sorted assertion namespace set.
The payload has no credential or reusable authorization grant.

Each item contains `eventId` (the original stable UUID), `position`, `namespaceId`,
`jobId`, optional actual `runId`/`runNumber`, authoritative `ownerPrincipalId`,
`oldPhase`, `newPhase: terminal`, `outcome`, `jobRevision`, optional
`observedCompletedAt`, `recordedAt`, `imported`, and `reconciliation`. It contains
no job or namespace name, command, label, path, identity display name, or log data.
The observed completion value is the existing factual lifecycle timestamp and
may originate from a Control cancellation transition; `recordedAt` is the actual
statement time that recorded this transition. An absent run is not run zero.

Pages seek the `(namespace_id, position)` index separately for at most 320
asserted namespaces, with at most `limit + 1` candidates per namespace, then
merge their first bounded results. If fewer than the limit remain, `nextCursor`
advances to the committed global head, including when the visible page is empty.
Positions may have gaps from other namespaces. Position/cursor metadata is for
registered background services and must not be exposed as end-user counts.

These 409 errors require explicit recovery; the server never silently resets:

- `event_cursor_expired`: a continuation is below the retained prefix floor.
- `source_recovery_changed`: Control instance or restore epoch differs.
- `event_cursor_scope_changed`: service identity or the asserted namespace set
  differs. Adding namespaces requires a new checkpoint, even if the old subset
  remains authorized.

Invalid future positions return 400. Removed namespaces or disabled keys fail
current authority checks rather than exposing the old cursor's contents.

## Transaction and replay guarantees

Migration 21 adds an `AFTER UPDATE OF phase` trigger to the existing jobs table.
Every nonterminal-to-terminal transition writes a versioned
`monitoring.job_terminal.v1` outbox record in the same transaction. The existing
lifecycle trigger supplies completion facts first. Repeating a terminal phase,
changing terminal metadata, inserting completed history, or updating a
collection/graph wrapper emits no extra terminal event. A transition observed
while the restore reconciliation hold is active is marked `reconciliation`;
consumers exclude these bootstrap/recovery observations from new alerts.

The publisher locks one feed-state row, reads only committed eligible outbox
rows, appends the original event UUID and payload, acknowledges those rows, and
updates its counter in one transaction. A later-started transaction may finish
before an earlier job transaction: the uncommitted job has no feed position yet
and remains eligible after it commits. No cursor can pass an allocated but
uncommitted feed append. This guarantee does not rely on sequence allocation,
outbox UUID order, HTTP delivery, or wall-clock ordering of job transactions.
Multiple Control replicas can run the publisher safely. Publication and retention
perform no external I/O inside their transactions.

Crash before commit rolls back the append, counter, and acknowledgement. Crash
after commit leaves all three durable. Replaying an acknowledgement for an event
already retained does not allocate another position. Source restore preserves
retained event UUIDs; changing the recovery epoch invalidates old continuations
and pre-recovery assertions. The feed cannot reconstruct transactions absent
from the restored backup. Consumers must record a monitoring gap and deduplicate
by deployment UUID, Control instance UUID, and original event UUID, preserving
sufficient tombstones for their replay/recovery policy.

At subscription activation, capture both `headCursor` and `asOf`. Match subsequent
eligible feed events whose `recordedAt` is after that source-clock boundary.
An older unpublished backlog is then excluded, while an earlier observed
completion first recorded after activation can qualify. Exclude imported and
reconciliation events. A failed checkpoint leaves activation pending. Recovery
is explicit and must not generate synthetic historical alert floods.

## Retention and operations

`JOBMAN_CONTROL_MONITORING_FEED_RETENTION` defaults to `720h` (30 days), accepts
whole-second durations from `24h` to `8760h`, and must match across replicas.
It is independent of namespace published-outbox cleanup and Dashboard inbox
retention. Retention is measured from durable publication so an old unpublished
backlog gets a full replay window. The publisher runs once per second in bounded
batches of 256; retention removes at most 1,000 oldest rows per cycle.
Publication timestamps cannot move backward within a source, and pruning shares
the publication lock. A snapshot sees both prefix deletion and its advanced
retention floor, or neither. Reducing retention can expire old consumers and is
an operator policy change requiring a backup and consumer-lag check.

The existing operational metrics add:

- `jobman_control_monitoring_backlog`
- `jobman_control_monitoring_backlog_oldest_seconds`
- `jobman_control_monitoring_retained_oldest_seconds`
- `jobman_control_monitoring_retention_seconds`

Alert on sustained publication backlog (for example, oldest pending transition
above five minutes), worker errors, and consumer checkpoint age approaching
retention. The oldest retained age alone is not consumer lag; Dashboard must
report its own oldest unprocessed/checkpoint age per source. No service ID,
namespace, job, or user is a metric label.

## Migration and compatibility

Migration 21 is additive: feed tables/indexes, terminal outbox trigger, and an
explicit disjoint service-only assertion shape. Existing actor rows remain
valid, existing APIs retain their behavior, and no historical terminal jobs are
backfilled. Take the normal PostgreSQL backup and review service registrations
before upgrading. Old binaries fail exact schema compatibility checks after the
forward migration. Roll back binaries only with the coordinated database restore
procedure; advance recovery epoch and explicitly reconcile all consumers. Do not
edit an applied migration or pretend a restored snapshot retained lost events.
