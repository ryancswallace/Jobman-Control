# Bounded target monitoring

The `target-catalogs` feature provides additive, authorized target discovery by
immutable UUID. Legacy `/targets` name-based routes remain unchanged; Dashboard
uses the complete-count bounded catalog instead of the legacy 1000-row list.

`GET /v1/namespaces/{namespace}/target-catalog` accepts `limit` from 1 to 200
(default 100), optional RFC3339 `createdBefore`, and opaque `pageToken`. The first
page defaults the inclusive creation cutoff to its real source transaction time.
The cursor preserves that cutoff and the last emitted `(createdAt, id)` tuple.
Rows use descending timestamp/UUID ordering, backed by migration 19's namespace
creation index. A changed explicit cutoff with an existing cursor is rejected.
Names, current generation and state never determine cursor position.

`TargetCatalog` returns `namespace`, immutable `namespaceId`, database `asOf`,
`recoveryEpoch`, current `authorizationVersion`, `authorizationCheckedAt`, and
managed-scope `authorizationExpiresAt`. It also returns the effective
`createdBefore`, the complete filtered `total`, bounded `items`, and
`nextPageToken` when more items exist. Count and metadata come from one read-only
repeatable-read transaction. The item array is capped at 2 MiB of encoded JSON;
a byte-limited page can contain fewer than `limit` items and still have a cursor.
Each request reauthorizes the represented user and service; cursor possession
never grants access. Live configuration can change between pages, while the
creation cutoff excludes later targets.

Each item includes actual target `id`, `name`, `kind`, `state`, decimal `revision`,
`createdAt`, `updatedAt`, and the selected immutable `generation`. Generation
fields are actual `id`, decimal `number`, `executionBackend`, `transport`,
`runtimes`, `operatingSystems`, `architectures`, `capabilities`, `partitions`,
optional `logStore`, `artifactStores`, and `provider`. Store versions are decimal
strings. Empty arrays remain arrays. Provider fields describe approved target
configuration; they are not cloud credentials or a cloud API authorization.
`partitions` is a preview of at most 200 non-retired partitions in byte order by
name, with exact decimal `partitionCount` and `partitionsTruncated`. This bound
also applies to the detail response. The catalog does not infer live CPU, GPU,
memory or cluster capacity.

`GET /v1/namespaces/{namespace}/target-catalog/{targetUUID}/partitions` returns
`TargetPartitionList` with the same authority fields, `targetId`, `generationId`,
complete decimal `total`, bounded `items` (`name`, `isDefault`), and optional
`nextPageToken`. Start with the detail's `generationId`; `limit` is 1–200
(default 100). Continuation tokens preserve the generation and last emitted
partition name, with explicit PostgreSQL `C` collation and a matching index.
An optional explicit generation on continuation must match the cursor. Every
page requires that generation to still be the target's current generation;
otherwise HTTP 409 requires the client to refresh target detail and restart.
Cross-namespace target UUIDs return 404, and revoked authorization denies every
page. The full count and generation check share the page's read transaction.

`GET /v1/namespaces/{namespace}/target-catalog/{targetUUID}` returns
`TargetSnapshot` with the same authority fields and one `target`. The UUID is
resolved inside the authorized namespace; cross-namespace IDs return not found.
All three new routes require delegated operation `targets.read`; execution/admin
operations remain unavailable to delegated actors.

Migration 19 is additive. Existing APIs and target identity/state transitions
remain compatible. The write path explicitly binds log-store versions as bigint,
so a valid 64-bit mapping version cannot be accidentally narrowed by SQL literal
type inference. Restore/rollback follows the existing controlled schema and
recovery-epoch procedure.
