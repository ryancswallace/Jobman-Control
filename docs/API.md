# API

The complete current interface is
[`api/openapi-v1alpha1.yaml`](../api/openapi-v1alpha1.yaml). The API is
pre-release and uses `jobman.control/v1alpha1` documents plus the portable
`jobman/v1alpha1` workload contracts.

## Authentication classes

- Client endpoints use an OIDC bearer token, or the fixed loopback identity in
  explicitly enabled development mode.
- Agent enrollment uses a short-lived single-use enrollment token and local
  key proof.
- Execution endpoints require an enrolled agent's currently valid mTLS
  certificate.
- `/healthz`, `/readyz`, and `/metrics` are unauthenticated. Metrics use only a
  fixed set of lifecycle/status labels, but counts can still reveal service
  activity and should be network-restricted to the monitoring plane.

TLS is required for non-loopback OIDC deployments. Agent mTLS requires the
service TLS certificate and agent CA configuration.

## Mutation semantics

Client creation and cancellation requests require an `Idempotency-Key`.
Replaying the same canonical intent returns the original resource; reusing the
key for different intent returns conflict. Agent acceptances, observations,
action acknowledgements, and log metadata carry stable identities and digests
for equivalent replay detection.

Namespace policy replacement uses an exact `If-Match: "revision-N"`
precondition instead of an idempotency key. A stale or concurrently reused
revision fails with conflict, so a client may safely read the current policy
before deciding whether to retry a lost response.

Target administrators may move a target from `active` to `draining` or
`disabled`, and later reactivate or retire it, using a revision precondition
and idempotency key. Draining prevents new assignments while preserving agent
control for accepted work. Agents renew compatibility sessions, rotate mTLS
certificates, and submit immutable capability/liveness observations. Target
policy remains the upper authorization bound even when an agent advertises
more.

Namespace administrators create a new immutable target generation with
`POST /v1/namespaces/{namespace}/targets/{target}/generations`, an
`Idempotency-Key`, and an `If-Match` revision. The target name and kind cannot
change. Existing jobs, executions, and agents retain their prior generation;
new submissions resolve the newly current one. Provider facts distinguish
`on-prem` from `aws-parallelcluster` without storing AWS credentials.

Job submission resolves an immutable target generation and fails placement
before acceptance when the current agent slice cannot execute the workload.
Both subprocess and Slurm targets accept direct native or container commands, ordinary
environment values, and run timeouts. Portable CPU, memory, GPU, node, task,
and wall-time requests require a Slurm target. Declared regular-file inputs
and outputs may use one target-approved local/NFS or S3 logical store; Control pins the
mapping version and validates published output metadata against the declaration.
Temporary storage, multiple stores, directory/archive artifacts, environment
profiles, secret bindings, extensions, and retries are reserved for later
slices. Slurm-provided environment variable names cannot be overridden by
workload values.

Collection submission accepts one portable `CollectionRequest` at
`POST /v1/namespaces/{namespace}/collections`. Every explicit child resolves
and is inserted in the same transaction. `maxActive`, `continue`/`fail-fast`,
and `never`/`prefer`/`require` array policy are durable. Compatible Slurm
children carry explicit array task indices and one agent affinity; each remains
an independently readable and cancellable job. The collection GET returns
aggregate counts and ordered child snapshots.

Graph submission accepts one portable `GraphRequest` at
`POST /v1/namespaces/{namespace}/graphs`. Every sealed node and target
placement resolves in the same transaction or nothing is accepted. Graphs are
immutable and bounded to 10,000 nodes and 100,000 edges before a namespace may
apply a lower node quota. Cycles, duplicate edges, unknown references, and
invalid predicates are rejected. `maxActive` gates dispatch and each edge uses
`success`, `failure`, `any-terminal`, or a selected `outcomes` set. Control,
not Slurm, is authoritative for cross-target readiness. GET returns aggregate
counts, ordered node jobs, and dependency satisfaction; graph cancellation
applies ordinary durable job cancellation to every nonterminal node.

Every namespace has a policy readable by a current member at
`GET /v1/namespaces/{namespace}/policy`. A namespace administrator may replace
it with `PUT` and an exact revision ETag. The policy bounds active and queued
jobs, collection items, graph nodes, completed-idempotency retention, and
published-outbox retention. Quota rejection returns HTTP 422 without partial
group creation.

Operators and namespace administrators may export append-only audit evidence
from `GET /v1/namespaces/{namespace}/audit`. Pages are ascending by numeric
event ID; pass `nextAfterId` back as `afterId` to resume. Audit rows are not
removed by ordinary operational retention.

`POST /v1/namespaces/{namespace}/history/imports` validates or imports one
`CompletedHistoryImport`. `dryRun=true` performs authentication, authorization,
portable workload validation, placement resolution, and provenance validation
without writing. A real import requires an idempotency key and creates one
terminal job with a new shared ID, source store/schema/job ID provenance, and
no run, execution, assignment, log, artifact bytes, or live-process identity.
Only SQLite source metadata and terminal outcomes are accepted.

Assignments are offers until the agent durably accepts and receives a launch
authorization. Delivery may repeat. Process and scheduler execution events are
monotonically source-ordered. Slurm events distinguish uncertain submission,
durable scheduler identity, nonterminal observation, and terminal result. The
API never claims exactly-once process or scheduler launch across an external
failure boundary.

Job reads expose `observationConfidence` as `current`, `stale`, `uncertain`, or
`lost`. Stale agent evidence never proves that accepted external work stopped.
The log and artifact manifest endpoints authorize metadata by current namespace
membership; neither endpoint proxies bytes or returns physical paths.

## Errors and disclosure

Errors use bounded JSON classifications. Authentication failures do not reveal
credential details. Cross-namespace reads generally return not-found. Server
logs record safe operation classifications, not request bodies, database URLs,
tokens, workload commands, environment values, log bytes, or certificate key
material.

Consumers should pin an API version, preserve unknown response fields, use
ETags and idempotency keys as documented, bound retries, and treat all user and
agent output as untrusted data.
