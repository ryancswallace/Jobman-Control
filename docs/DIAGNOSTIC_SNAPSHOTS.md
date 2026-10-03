# Shared diagnostic snapshots

The `shared-diagnostic-snapshots` feature is enabled only when the operator sets
`JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID` to the immutable Dashboard registry
source UUID. Preserve this UUID across Control replicas and Dashboard registry
updates. The actual Control instance UUID still comes from the database. A
caller cannot relabel evidence as another deployment, instance or namespace.

`GET /v1/namespaces/{namespace}/jobs/{jobUUID}/diagnostic-snapshot` requires
`deploymentId`, `controlInstanceId` and `namespaceId` UUID query pins. Optional
`expectedJobRevision` is a positive decimal bigint; optional `runId` selects an
actual run UUID belonging to the job. Unknown, duplicate or malformed parameters
are rejected. Source/revision mismatch returns 409, missing job/run returns 404,
and unavailable configuration returns 501. Ordinary OIDC reads and delegated
`evidence.read` both require current namespace capability and account/grant
freshness. A snapshot never grants access to subsequent byte reads or reports.

`DiagnosticSnapshot` contains the usual `apiVersion`, `kind`, namespace and
namespace UUID, database `asOf`, recovery epoch, authorization revision/decision
and expiry fields, plus `snapshot`. The latter is the public core
`diagnostic.SharedSnapshot` contract, with its exact snake_case names and decimal
run, job revision and manifest counters. Its capture time and authority come
from one read-only repeatable-read transaction. No external I/O occurs there.

The snapshot records actual Control job phase, desired state, outcome, revision,
submission and retained lifecycle observations; actual run UUID/number and
execution UUID when available; recorded exit code/signal and normalized
scheduler observations; incoming dependency decisions using Control's existing
predicate definition; and durable execution lifecycle event UUIDs and observed
versus recorded timestamps. It does not substitute `updated_at` for missing
execution times or fabricate a run revision that Control does not store.
Imported history preserves the missing original submission/execution evidence.
Unstructured scheduler reasons and signals are excluded with explicit omissions.

Without `runId`, select the latest 32 actual runs, returned in ascending actual
run-number order. Older runs are disclosed through `history_truncated`; an
explicit run may select one outside that default window. At most 256 lifecycle
events and 128 incoming dependencies are read, with explicit truncation
omissions. Migration 20 adds an execution/source-sequence index so each selected
execution reads a bounded event prefix before global ordering. The entire public
snapshot is checked against the core contract's 1,024-item and 2 MiB limits.

Log references contain only opaque IDs, real run/execution/stream identity,
actual manifest revision, the contiguous published byte length, and completion.
They contain no filesystem path or URL. Incomplete prefixes and empty completed
streams remain factual zero-byte references; truncation has an explicit omission.
A reference means metadata exists, not that Control accessed or verified the
stored bytes. Byte collection occurs separately through the authorized log
broker and must match every reference identity and revision. Neither commands,
paths, environment contents nor log bytes are part of this endpoint. The core
collector's separate opt-in log path requires configured value-aware redaction.

The response records the actual linked core module version when build metadata
is available, otherwise `unknown`; it does not claim the build host is an agent
or invent a local SQLite store. Control's build version, platform, instance,
namespace, source UUID, run identities and selected revisions form provenance.
Consumers must validate/seal with the public core collector and maintain report
freshness independently from this immutable capture.

The module pin is an approved, immutable development dependency at core commit
`62ac89b14547a5fc9c1b9e2852832d66498361c2`, not an upstream release. It requires no
sibling checkout. Production release acceptance still requires approved upstream
release tags and consumer updates. Migration 20 is additive; rollback follows
the existing matching-binary/database restore and recovery-epoch procedure.
