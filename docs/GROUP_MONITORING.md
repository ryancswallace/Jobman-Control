# Bounded group monitoring

The original collection and graph GET routes continue to return their complete
v1alpha1 documents for existing clients. Dashboard clients should use the bounded
routes below. Every read requires current namespace membership, uses a read-only
repeatable-read transaction, and returns its database snapshot time as `asOf`.
This timestamp is not proof of directory synchronization or execution freshness.

## Catalogs and summaries

- `GET /v1/namespaces/{namespace}/collections`
- `GET /v1/namespaces/{namespace}/graphs`
- `GET /v1/namespaces/{namespace}/collections/{id}/summary`
- `GET /v1/namespaces/{namespace}/graphs/{id}/summary`

Catalogs take `limit` (default 50, maximum 200), `pageToken`, and an optional
inclusive `createdBefore` RFC3339 cutoff. Collections also accept
`arrayMode=individual|slurm-array`. Results sort by creation time and UUID,
descending. Keep the cutoff and filters unchanged while following opaque page
tokens. The `total` decimal string counts all matching wrappers, ignoring the
page cursor. Catalog responses echo a supplied `createdBefore`.

Each catalog item and summary contains `metadata`, `spec`, and `status` with
complete source counts. It has no child array. An empty catalog has `items: []`
and `total: "0"`; missing access is an authorization error, never a fabricated
empty namespace. Individual summary responses wrap the document in `summary`.
Counts preserve the established Control semantics: graph waiting nodes are
accepted, while group active counts exclude both accepted and terminal jobs.
Namespace overview active counts instead cover every nonterminal child job.

These are live page snapshots. Immutable creation and child-index cursors prevent
newer inserts from moving backward into the catalog, but statuses can change
between pages. Counts are exact at each returned `asOf`, not a frozen transaction
across HTTP requests. Completed-outcome time windows belong to the namespace
summary/job APIs; wrapper catalogs use their explicitly echoed creation cutoff.

## Children and Slurm arrays

- `GET /v1/namespaces/{namespace}/collections/{id}/items`
- `GET /v1/namespaces/{namespace}/graphs/{id}/nodes`

Use `limit` (default 50, maximum 200) and `afterIndex` (default -1). The response
contains `items`, a complete `total` decimal string, and `nextAfterIndex` only
when another page exists. Indices are immutable source indices, not page offsets.
Each item contains the current factual job snapshot including original owner,
run, execution, lifecycle provenance, and scheduler observation when available.

Slurm array collection items additionally contain `arrayTaskIndex`, the exact
immutable task index Control uses in the Slurm array assignment binding. It may
be zero and must not be discarded as false. It is absent on ordinary collections.
This planned binding is not a claim that Slurm has accepted the task; the job's
native identity and observed scheduler fields provide that separate evidence.

Graph nodes contain `dependencyCounts` with complete `total`, `satisfied`,
`waiting`, and `unsatisfied` counts for all incoming edges. Nodes never carry an
unbounded dependency array on this route. One bounded job query and one grouped
predicate query populate a node page; there is no round trip per node.

## Dependencies and neighborhoods

`GET /v1/namespaces/{namespace}/graphs/{id}/dependencies` accepts `limit`
(default 100, maximum 500), opaque `pageToken`, and optional `nodeId` (job UUID).
Without a node filter it pages the whole graph's edges. With a node filter it
selects incident edges; `direction=incoming|outgoing` narrows that selection and
requires `nodeId`. Edges sort by upstream/downstream job UUID. The complete
`total` decimal string ignores the page cursor. Keep the filters unchanged.

Each edge includes node names (`from`, `to`), job UUIDs, predicate, explicit
outcomes, upstream phase/outcome, and source-computed `state`. Nonterminal
upstreams are `waiting`; terminal upstreams are `satisfied` or `unsatisfied`
according to Control's existing success, failure, any-terminal, or outcomes
predicate. A dashboard must not infer a competing execution state machine.

`GET /v1/namespaces/{namespace}/graphs/{id}/neighborhood` requires `nodeId`
and accepts `maxNodes` (default 50, maximum 200) and `maxEdges` (default 100,
maximum 500). It returns the one-hop induced neighborhood: the center and all
immediately adjacent nodes, with edges whose endpoints belong to that set.
The center is always selected; remaining nodes use immutable index order. The
response includes only edges whose endpoints are actually returned, so truncation
never creates dangling references. `totalNodes`, `totalEdges`, `omittedNodes`,
and `omittedEdges` cover the complete neighborhood, including edges between
neighbors. Node dependency counts still cover their full incoming dependencies,
including dependencies outside the neighborhood.

## Upgrade and verification

Migration 15 adds creation-catalog indexes and a reverse dependency index. It
changes no existing group data or legacy response shape. Apply migrations before
running the new binary. As with other forward-only migrations, rollback requires
restoring the previous schema/data backup; an older binary rejects unknown
migrations. Migration 13's authorization rollout precautions still apply.

The PostgreSQL integration suite checks bounded catalogs, stable cursors,
creation cutoffs, array filtering and source indices, complete node/predicate
counts, terminal edge satisfaction, isolated nodes, exact truncation counts,
no dangling edges, and authorization on every new repository read. HTTP tests
cover all new response shapes and reject duplicate/unknown queries, invalid
IDs, invalid cursors, and limits above the documented bounds.
