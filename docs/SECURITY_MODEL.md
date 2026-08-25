# Security model

## Assets

High-value assets include PostgreSQL contents and credentials, the agent token
derivation key, server TLS private key, agent CA private key, OIDC trust
configuration, enrollment tokens, agent private keys and certificates,
workload specifications, namespace membership, audit history, and external log
and artifact bytes.

Job and target identifiers are lower sensitivity than credentials but can
still reveal departmental activity. Workload commands, environments, process
results, and logs may contain secrets even when Jobman does not classify them
as credentials.

## Principals and authority

- OIDC principals may act only through namespace roles stored in PostgreSQL.
- Namespace administrators may provision membership and target enrollment
  intent for their namespace.
- Agents may accept only their target generation's assignments, report only
  assigned executions, acknowledge their desired actions, and commit only the
  target-approved artifact manifest.
- The Control service can authorize jobs, issue agent certificates, mutate all
  database control state, and advance shared lifecycle snapshots.
- PostgreSQL administrators and service secret administrators are trusted
  deployment roles outside application authorization.

No API caller receives PostgreSQL credentials. Control never starts a process,
invokes Slurm, opens SSH, or reads bulk artifact bytes.

## Boundaries and controls

User authentication binds the exact OIDC issuer, audience, and stable subject.
Agent authentication combines TLS chain validation with database checks for
serial, public-key digest, expiry, revocation, agent state, target, and the
agent's pinned target generation. Generation rollover preserves control of old
accepted work, but target-generation matching and a fresh immutable capability
report are required for new assignment eligibility. A current compatibility
session is also required. Namespace
authorization is repeated in repository queries so an HTTP routing mistake
does not become the only boundary.

Mutation documents are bounded, strictly decoded, canonicalized, digested, and
idempotent. Database constraints preserve namespace ancestry, known states,
monotonic sequences, immutable target generations, and artifact metadata
bounds. External execution remains at-least-once intent across failures, so
agents journal offers and require durable launch authorization.

Graph admission rejects cycles, unknown nodes, duplicate edges, excessive
expansion, and unsupported outcome predicates before state is created. Every
node is authorized and placement-resolved under one namespace. Control owns
readiness across targets, so an untrusted target or scheduler cannot authorize
another node by mutating graph state directly.

Namespace policy serializes queued/group quota checks and bounds active
assignment. Fair dispatch uses only namespace policy state and stable creation
order; it does not accept user-supplied scheduler priority. Metrics have fixed
labels and no names or content, but the unauthenticated counts still require a
network boundary. Audit export is role-protected and ascending, and ordinary
retention never deletes audit evidence.

Completed-history import accepts only terminal metadata, creates a new ID, and
records unique SQLite provenance. It does not create executable state or trust
the source's primary key as a shared identifier. Dry-run performs the same
authorization, workload, placement, and provenance checks without writing.

Log object keys are server-derived from authorized identities. Declared output
keys are user intent pinned to a target-approved logical store and are matched
exactly against terminal metadata. Manifests carry SHA-256 checksums and
lengths; clients verify log bytes at read time. Control does not trust the
object store to enforce namespace authorization and does not proxy its bytes.
Filesystem permissions and cross-user NFS ACLs remain an operator
responsibility. S3 bucket/prefix policy, expected-owner validation, encryption,
versioning, endpoints, and short-lived target identity are likewise deployment
controls; Control stores only the approved logical mapping name and version.

## Secret handling

Secrets enter through the environment or private mounted files. They must not
be accepted as ordinary API configuration, labels, logs, metrics, error text,
or release artifacts. Avoid putting secrets directly in process command lines.
Restrict access to service-manager environment files and `/proc` according to
the operating system.

Error handling deliberately returns classifications and wraps internal
operations without including database URLs, bearer tokens, private keys,
request bodies, certificate contents, log bytes, or workload environments.
Debugging must preserve those redaction boundaries.

## Fail-closed behavior

- Invalid or ambiguous identity is unauthenticated.
- Cross-namespace resource lookup is denied, often as not-found.
- Unknown, changed, pending, or newer migrations stop startup.
- Assignment delivery does not authorize process launch; acceptance does.
- Draining or stale targets cannot receive new assignments; silence alone
  cannot release an accepted execution for duplicate launch.
- Replayed mutation identity with different intent conflicts.
- Revision mismatch prevents target-generation rollover, and a rollover never
  rewrites existing job, execution, or agent generation identity.
- A collection is wholly accepted or wholly rejected; `require` array policy
  fails placement rather than falling back.
- A graph is wholly accepted or wholly rejected; cycles and unsatisfied
  downstream branches never become silently runnable.
- Namespace quota checks lock policy state, and a persistent recovery hold
  prevents all new assignment after a restore until an operator releases it.
- Gapped log chunks remain hidden.
- Log-enabled executions cannot become terminal until both streams complete.
- Development identity cannot bind a non-loopback listener.

## Known gaps

The current slice lacks administrative emergency revocation, CA rollover,
multi-issuer identity, service principals, proxy-terminated TLS policy,
cross-user artifact ACL provisioning, per-execution credential brokering, and
a separately packaged least-privilege migration command. Stale execution
repair and post-restore convergence remain operator decisions because silence
cannot authorize duplicate launch.

Metrics are unauthenticated and must be network-restricted. Audit rows are
application-append-only but remain mutable to a PostgreSQL superuser; durable
external export and institutional retention are deployment responsibilities.
The recovery hold is changed through a privileged direct database procedure
that needs external change-control evidence. Real backup/restore, load, chaos,
on-premises Slurm, and ParallelCluster security acceptance have not been run.
These remain production blockers where the corresponding risk cannot be
accepted externally; see the production-readiness review.
