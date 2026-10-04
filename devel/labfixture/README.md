# Synthetic Dashboard Lab source

This developer helper creates an isolated Control acceptance source. It is
not part of the production runtime or release archive. It uses synthetic users,
LDAP entries, job observations and log bytes, and does not establish corporate
AD FS compatibility or actual host/Slurm execution acceptance.

The normal Control binary serves all API requests, verifies delegation, and runs
the real directory reader/reconciler. The helper's LDAP listener binds only to
`127.0.0.1:18636`; it serves explicit GUIDs, direct groups and eligibility over
verified TLS. A fresh state file is read for each connection. Changing the file
with a higher revision permits real removal/disable/freshness acceptance through
the normal reconciler. No directory or bearer shortcut is added to Control.

## Prepare once

Build the normal Control binary and the helper from the same exact commit:

```sh
env -u GOROOT CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/jobman-control-fixture .
env -u GOROOT CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/jobman-control-lab-helper ./devel/labfixture
```

Set the correct guest architecture and standard build information flags in the
deployment workflow. Record the source revision and binary hashes separately.
Install under a new private Lab service path; preserve the existing Control
service, binaries, realm, database and configuration.

Provide public configuration with exactly Alice then Bob. The OIDC subjects
must be the actual immutable subjects of the separately provisioned Lab users;
never infer them from names or email. Example values below are placeholders:

```json
{
  "issuer": "https://oidc.lab.test:8443/realms/jobman-lab",
  "audience": "jobman-dashboard-api",
  "host": "10.77.0.21",
  "users": [
    {"directoryId": "71000000-0000-4000-8000-000000000001", "subject": "approved-alice-subject", "name": "Synthetic Alice"},
    {"directoryId": "71000000-0000-4000-8000-000000000002", "subject": "approved-bob-subject", "name": "Synthetic Bob"}
  ]
}
```

The private DSN must use a PostgreSQL URL and `sslmode=verify-full`. Before migration
or seeding, the helper requires `current_database()` to equal
`jobman_dashboard_control`. It will refuse the original Control database or the
Dashboard state database. The operator must separately create the dedicated
role/database and TLS rule; the helper never grants database privileges.

```sh
jobman-control-lab-helper prepare \
  --root /etc/jobman-dashboard-lab/control-fixture \
  --config /etc/jobman-dashboard-lab/fixture-input.json \
  --database-url-file /etc/jobman-dashboard-lab/control-database-url \
  --log-root /var/lib/jobman-dashboard-lab/seed-logs
```

The root is a real directory with mode `0700`. Files are exclusively created
with mode `0600`; existing trust material is never overwritten. A completed
preparation with identical public input is a no-op. A partial failure requires
inspection of this isolated fixture before retry; the helper never resets or
deletes a database. Errors do not print configuration values or wrapped driver
errors.

Preparation creates two namespaces and preserves real generated principal IDs.
Alice has viewer plus submitter contributions in `dashboard-research` and
administrator in `dashboard-operations`; Bob has viewer only in research.
Explicit mappings replace both users' temporary seed bootstrap grants when the
normal service starts. The eight groups each map to one namespace/role. There
are no nested group grants. Both accounts and namespace authority must be
refreshed by the actual LDAP reconciler before delegation succeeds.

Every namespace has terminal nonempty and empty logs, an open zero-prefix stream
with an out-of-order marker, another owner's awaiting job, synthetic imported
terminal history, a collection, a Slurm array with real task indices, and a
small dependency graph. Seeding uses normal Store admission and execution
observation methods. Nothing launches a workload or changes existing Lab targets.
The fake agent eventually becomes stale, which is expected source evidence.

## Service and material files

`fixture-info.json` contains public source instance, namespace, preserved
principal, target-generation and job/group identities. It is the authority for
constructing source-qualified Dashboard configuration. `fixture-input.json`
records the approved public inputs. Do not print the private files:

- `control.env`: private normal Control environment, including DSN/token key.
- `directory.json` and `directory-state.json`: explicit synthetic mappings/state.
- `delegation.json`: public registration policy and Ed25519 keys.
- `fixture-ca.crt`/`.key`: isolated Lab CA, valid thirty days.
- `control-server.crt`/`.key`: server certificate for the configured host and
  loopback, valid seven days, used by Control and synthetic LDAP.
- `dashboard-client.crt`/`.key` and `dashboard-signing-key.pem`: Dashboard client
  material; service `dashboard-lab`, key `synthetic-lab-v1`.
- `broker-client.crt`/`.key` and `broker-signing-key.pem`: independent log-broker
  client material; service `dashboard-log-broker-lab`, key `synthetic-broker-v1`,
  limited to `namespace.read` and `logs.read`.
- `dashboard-signing-public.pem` and `broker-signing-public.pem`: public PKIX keys.

Both services use Control delegation audience
`urn:jobman:dashboard-lab:control`; each registration pins its own actual mTLS
leaf thumbprint. Signing keys are PKCS8 Ed25519 PEM. Copy only the required
private service files to each corresponding runtime's private directory. Never
copy the CA private key to Dashboard or brokers. Existing Keycloak issuer trust
comes from the Lab CA already installed in the guest trust store.

Start the helper in a separate non-root service:

```sh
jobman-control-lab-helper directory --root /etc/jobman-dashboard-lab/control-fixture
```

Then start the exact normal Control binary as a separate non-root service with
`EnvironmentFile=/etc/jobman-dashboard-lab/control-fixture/control.env`. Its TLS
API listens on port `18443`. Both processes need read access to their respective
private configuration/material; use scoped ownership or ACLs without making the
private root world-readable. The ordinary Control coordinator and directory
worker run unchanged. Stop both new services to retire the live fixture; do not
stop or reconfigure the original Control service.

The log store is `lab-nfs`, version `1`. Preparation writes only synthetic byte
objects to the local spool. On root-squashed NFS, grant Alice read/traverse on
that spool and create the new namespace trees as Alice (UID/GID 21001) beneath
`/data/jobman/alice`; do not write NFS as root or preserve a source ACL over the
NFS default ACL. Create directories `0750` and files `0640`, preserving the
explicit reader ACL mask for UID21901. The broker mapping pins the returned
actual target generation to this physical root. Never expose the private
fixture root as a log mount.

## Verification

`make integration-test` includes the fixture seed test when the explicit test
PostgreSQL setting is supplied; it creates/drops its own schema and does not
prepare the long-lived database. Unit tests verify real authenticated LDAPS,
account eligibility, independent keys, protected file modes and LDAP bounds.
Use Dashboard's real source and broker adapters for live acceptance, including
both empty terminal variants, group unions, Bob's narrower scope, removal,
directory outage beyond 120 seconds, reproof, and pinned immutable byte reads.
Do not call synthetic LDAP/Keycloak behavior evidence of AD FS or managed-phone
acceptance.

## Add one diagnostic observation fixture

The optional `diagnostic` mode adds one dedicated `synthetic-diagnostics` host
target, one synthetic agent, and one Alice-owned failed job in the existing
`dashboard-operations` namespace. This is synthetic Store observation data:
the helper never launches a subprocess or Slurm workload. It preserves original
targets, agents, jobs, log objects, trust material, group memberships and
`fixture-info.json`. The additional catalog entry must be included explicitly in
Dashboard acceptance expectations and its exact generation in the broker mapping.

This mode requires POSIX private permissions and directory fsync and rejects
Windows before reading or writing fixture files. Use a separately reviewed exact
helper build, with the same migration set as the
running synthetic Control. First apply the reviewed source upgrade through the
Lab's dedicated-database/instance-guarded upgrade procedure. This mode checks
`CheckMigrations` and never runs migrations. It requires the existing private
environment to pin the diagnostic deployment below, directory enforcement and
disabled migrate-on-start. The database must still be `jobman_dashboard_control`
over `verify-full`, with the same instance as the original fixture.

Create a separate empty local spool owned by the synthetic source user with mode
`0750`, for example `/var/lib/jobman-dashboard-lab/diagnostic-logs`. Run the helper
as that user, with read access to the existing private DSN file:

```sh
jobman-control-lab-helper diagnostic \
  --root /etc/jobman-dashboard-lab/control-fixture \
  --database-url-file /etc/jobman-dashboard-lab/control-database-url \
  --log-root /var/lib/jobman-dashboard-lab/diagnostic-logs \
  --deployment-id 72000000-0000-4000-8000-000000000001
```

The helper waits at most45 seconds for the ordinary coordinator to offer the
new job to the dedicated agent; it never runs broad reconciliation itself. It
accepts that exact job assignment, records synthetic start/completion with exit
code1, and publishes one complete stderr chunk plus an empty complete stdout
chunk through normal Store methods. The nonsecret stderr contains a deterministic
permission diagnostic and the Lab redaction canary. No other execution is accepted.

`diagnostic-fixture.json` is a separately created immutable supplemental manifest.
It identifies synthetic observation mode, helper commit, deployment/Control
instance/epoch, namespace, new target/generation, job/revision/run/execution and
exact immutable chunk metadata. It contains no credential or log bytes. Grant
Alice read/traverse only on the separate spool and copy just those new chunks as
Alice into the existing NFS root with its designated-reader ACL; never copy the
private fixture directory or change root squashing. The Lab wrapper verifies
chunk hashes before and after copying and adds only the exact new broker mapping.

A completed repeat verifies the original job, source epoch, manifests and bytes,
then performs no writes. A private exclusive `.diagnostic-prepare.json` receipt
is synced before mutations. Any partial failure leaves it for operator inspection
and prevents an automatic retry; never delete it merely to rerun the helper.
Existing directory-revocation recovery receipts also block preparation. The
helper does not reset or roll back source data or overwrite an immutable object.

Fixture tests use disposable schemas and a separate test coordinator, prove old
job rows unchanged, round-trip the supplemental metadata, reject altered source
epochs/bytes and duplicate preparation, and verify private receipt/file bounds.
Live report acceptance must separately exercise Control, actual NFS broker reads,
the public collector/deterministic engine, stored pairs, API and sealed citations.

## Notification cancellation scenario

The `notifications` mode supports deployed Dashboard event acceptance without
adding an agent or target. A reviewed Lab wrapper invokes it as the existing
synthetic source user with the dedicated private database file, exact deployment,
private fixture root, `--receipt` (32 lowercase hexadecimal characters), and
`--action prepare|complete`. Completion also requires `--case first|stopped`.
Only the fixed `dashboard-research` namespace is used.

Preparation submits two new Alice-owned jobs through ordinary `SubmitJob`; it
never inserts jobs through SQL, changes directory groups, or reconciles other
work. A durable private pending receipt precedes writes; incomplete preparation
requires inspection. Up to 100 completed scenario manifests are retained.
Completed preparation replays verify the exact source epoch and job names/IDs.

Completion calls ordinary `CancelJob` for the exact recorded job and stable
idempotency key. The normal terminal trigger and publisher supply the durable
event. Repeats return the same original terminal-event UUID. A job that unexpectedly
launched and cannot immediately terminalize fails the scenario; the helper does
not synthesize a process result. Read-only outbox inspection verifies the exact
namespace, job, outcome and revision. Public output contains only synthetic
provenance, source-qualified IDs, timestamps and optional actual run/execution
IDs. Credentials and source documents remain private.

The wrapper verifies actual publisher/ingestion/evaluation completion through
read-only exact-event queries, then the Dashboard API test checks rules, inbox,
read state and immediate stops. No helper performs Dashboard SQL mutations.
These scenarios establish cancellation-event integration only; they do not
establish real workload execution, AD FS, APNs or managed-phone acceptance.


## Add a separate secondary source

The explicit immutable `--profile secondary-v1` selector prepares a second
isolated source. Omitting it, or selecting `primary`, retains the original
primary database, listeners, material names, memberships and public JSON shape.
Profiles do not accept arbitrary database names, ports, source identities or
operation sets. The secondary profile refuses diagnostic mutation mode.

Before using this mode, provision a fresh dedicated database and role named
`jobman_dashboard_control_secondary`, with a private `verify-full` DSN and the
same reviewed migration set as the exact normal Control binary. Preparation
checks both the URL and `current_database()`, requires `current_schema()` to be
`public`, and refuses any pre-existing table. It never alters the original
Control or first Dashboard fixture database. Namespace names intentionally match
the first source, while all namespace, principal, job and Control-instance UUIDs
are newly generated. No primary database, trust key or recovery state is copied.

Provision three distinct empty real directories, each mode `0700`, with no
symlink aliases or nested paths. Run preparation as the provisioning operator;
the helper never changes ownership or converts existing directories:

```sh
jobman-control-lab-helper prepare --profile secondary-v1 \
  --root /etc/jobman-dashboard-secondary/control \
  --directory-root /etc/jobman-dashboard-secondary/directory \
  --config /etc/jobman-dashboard-secondary/fixture-input.json \
  --database-url-file /etc/jobman-dashboard-secondary/control-database-url \
  --log-root /var/lib/jobman-dashboard-secondary/seed-logs
```

Use the same two approved Keycloak subjects and immutable directory GUIDs in
Alice/Bob order. The secondary direct groups use the fixed `73000000` UUID
prefix. Alice receives research viewer only; Bob receives research viewer plus
submitter and operations namespace administrator. Eight one-group-to-one-role
mappings use the separate source `synthetic-dashboard-lab-secondary` and base
`DC=dashboard-secondary,DC=lab,DC=test`. Existing primary grants are unaffected.
The helper still produces synthetic observation data, not actual execution.

The separate Control listener is `28443`; synthetic LDAP binds only loopback
`28636`. The delegation audience is `urn:jobman:dashboard-lab-secondary:control`.
Independent API, worker and broker registrations each pin a newly generated
Ed25519 key and mTLS leaf. API (`dashboard-api-lab-secondary`, `secondary-api-v1`)
has namespace/jobs/groups/targets/logs/artifacts/evidence reads plus events.
Worker (`dashboard-worker-lab-secondary`, `secondary-worker-v1`) has
namespace/jobs/logs/evidence/events reads. Broker
(`dashboard-log-broker-lab-secondary`, `secondary-broker-v1`) has only namespace
and log reads. Public resource names do not select a trust identity.

The secondary Control root adds `worker-client.crt`/`.key`,
`worker-signing-key.pem`, `worker-signing-public.pem`, and a separate loopback-only
`directory-server.crt`/`.key`. The LDAP export contains **only** that LDAP pair,
`directory-password`, `directory-state.json`, and `directory-profile.json`.
It receives no source database URL, agent token key, CA private key or delegation
signing material. The provisioning operator assigns the Control root to UID21907
and the independent LDAP root to UID21908, retaining `0700`/`0600`. The separate
LDAP process requires no access to the Control root:

```sh
jobman-control-lab-helper directory --profile secondary-v1 \
  --root /etc/jobman-dashboard-secondary/directory
```

Start the exact normal Control binary with the separate `control.env`. Its
configuration already pins diagnostic deployment
`72000000-0000-4000-8000-000000000002`, directory enforcement and disabled
migrate-on-start. The operator grants only the required public CA and private
leaf/signing files to each Dashboard process. Copy the new namespace logs as
Alice to the separate NFS subtree `/data/jobman/alice/dashboard-secondary`, using
the designated broker-reader ACL and retaining root squashing. Never expose a
private material root through a log mapping.

A synced exclusive `.secondary-prepare.json` receipt precedes migrations or
seeding. Any partial preparation requires inspection; retry refuses populated
roots or a pending receipt. A completed identical rerun verifies the profile
and exact separate directory export before doing no work. A different profile,
DSN role/database, export root or original input cannot repurpose the fixture.
Normal secondary cancellation scenarios select Bob, require this profile and
exact secondary deployment, and retain the existing per-job receipt and target
checks. Before invoking a secondary mutation as the source UID, the privileged
Lab wrapper must also check the independent LDAP root for directory-recovery
receipts; the source UID is intentionally unable to read that root.

Fixture tests include actual loopback LDAPS into the normal directory reconciler
against a disposable PostgreSQL schema. They verify both asymmetric role unions,
Alice's denied operations scope, same-namespace other-owner reads, Bob's ordinary
cancellation event, unchanged prior jobs and rejection of a same-name job on a
different target. These tests do not apply or deploy the second source. Live
Dashboard registry extension, source-qualified aggregation, revocation and
notification acceptance remain separate reviewed Lab actions.
