# Bounded log and artifact metadata

The `bounded-log-manifests` and `bounded-artifact-metadata` capabilities identify
additive read surfaces for Dashboard and its independently authorized byte
broker. Legacy `/logs` and `/artifacts` responses remain compatible. Control
returns source metadata and never opens the referenced filesystem objects.

## Log range contract

Use `GET /v1/namespaces/{namespace}/jobs/{jobId}/log-chunks` with operation
`logs.read` for delegation. Query parameters are:

| Parameter | Meaning |
| --- | --- |
| `runNumber` | Positive decimal actual run number. Omit for latest actual run. |
| `executionId` | Optional UUID that must match the selected run's execution. |
| `stream` | `stdout` (default) or `stderr`. |
| `tailBytes` | Select the last 1–262144 bytes of the current contiguous prefix. |
| `fromOffset` | Nonnegative decimal logical byte offset, including a position inside a chunk. |
| `afterSequence` | Nonnegative decimal sequence cursor. |
| `limit` | 1–100 chunks; default 100. |

The three range selectors are mutually exclusive. Omit all to select the last
65536 bytes. Tail/offset reads seek directly through a byte-offset index; they
do not load or traverse all preceding chunks. Duplicate/unknown parameters,
malformed encoding, signed integers or integers outside base ten, and oversized bounds fail.

The `LogChunkList` envelope contains the canonical `namespace`, `namespaceId`,
`jobId`, database `asOf`, `recoveryEpoch`, current `authorizationVersion`, and
`authorizationCheckedAt`. Managed scopes include `authorizationExpiresAt` from
the older current account/namespace proof. Authorization and all metadata use
one read-only repeatable-read transaction. Every request repeats current actor,
service, namespace, role, and directory checks; cursors never confer authority.

Actual `runId`, decimal `runNumber`, `executionId`, and `targetGenerationId` are
included when they exist. No run or execution is fabricated for a job that has not started or an
imported job. An explicit run/execution mismatch returns not found. Missing log
streams return `state: not_captured`, `manifestRevision: "0"`, and empty chunks.
Stored streams have positive revisions and state `open` or `complete`; `open`
maps from the legacy `capturing` state.

Stream fields are decimal-string `manifestRevision`, `byteLength`,
`lastSequence`, `fromOffset`, boolean `truncated`, and ordered `chunks`.
Each chunk includes decimal-string `sequence`, `byteOffset`, `byteLength`,
`storeVersion`; `storeName`, immutable `objectKey`, SHA-256 `checksum`, source
`capturedAt`, and `complete`/`truncated` flags. A final zero-byte chunk is included
and must be validated even at EOF. Only the contiguous published prefix is
visible; out-of-order arrivals beyond a gap remain hidden.

`fromOffset` reports the logical selected start, which can be inside the first
returned chunk. `nextAfterSequence` means additional chunks exist beyond the
bounded page. A byte broker may stop inside a chunk and continue with
`fromOffset`, allowing that same immutable chunk to be independently revalidated.
Offsets beyond the current prefix and inconsistent stored metadata fail closed.

The fixed log key is
`namespaces/{canonical-name}/jobs/{job-uuid}/executions/{execution-uuid}/logs/{stream}/{sequence-padded-to-at-least-eight-digits}.chunk`.
Control checks it against the real authorized job/execution both on publication
and read, and verifies the immutable target generation's approved store/version.
An agent cannot publish another namespace's prefix for its own execution.
Chunk lengths are bounded at 256 KiB, with overflow checks. The broker must still
authorize the represented user, resolve an operator-approved storage mapping,
open safely, and verify the actual checksum/size; an object reference alone is
never permission to read bytes.

Migration 18 adds a persisted stream revision and the offset-seek index. Existing
streams receive a documented revision baseline of one. New committed chunks
advance the revision; byte-equivalent replay does not. Appending a new immutable
chunk does not invalidate a previously selected chunk: final broker checks pin
the same run/execution and compare authority/recovery identity plus the selected
chunk's immutable fields. They must not require the whole manifest revision to
remain unchanged during an ordinary append.

## Artifact metadata

`GET /v1/namespaces/{namespace}/jobs/{jobId}/artifact-metadata` uses
`artifacts.read`. Optional `runNumber` selects one actual run, `limit` is 1–100,
and `pageToken` is an opaque execution/name tuple cursor. `ArtifactList` includes
the same authority/read metadata, a complete filtered decimal-string `total`,
bounded `items`, and `nextPageToken` when needed. Each item carries actual run,
execution, target-generation identity, declared artifact name, approved
store/version, immutable object key, size, checksum, and source publication time.
Artifact destinations retain their declared workload semantics; this route
does not create artifact downloads or convert arbitrary object keys to paths.

Live pagination can observe later publications. Source qualification belongs to
the caller's verified Control deployment registry; these responses do not trust
a client-supplied deployment label as authority. Forward migration and rollback
follow the existing schema-ledger and controlled restore procedure.
