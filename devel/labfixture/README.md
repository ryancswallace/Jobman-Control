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
