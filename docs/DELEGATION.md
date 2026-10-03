# Read-only service delegation

Control supports certificate-bound Dashboard actor assertions as an additional
read authentication mechanism. Production use also requires independently
verified directory aliases, managed namespace grants, and fresh authoritative
directory verification. This implementation does not turn a Dashboard assertion
into an identity-provisioning or membership-administration request.

The `read-delegation` capability identifies the implemented wire and repository
boundary. Live directory reconciliation is a separate feature; clients must
require `directory-authorization` before enabling production Dashboard access.
The latter is not advertised by this slice. Ordinary OIDC clients and unmanaged
namespaces preserve their existing behavior.

## Wire contract

Use the actual mTLS client connection and this header:

```text
Authorization: Jobman-Delegation <compact signed JWT>
```

Control never substitutes proxy certificate headers for the TLS peer. The
protected JWT header must contain `alg=EdDSA`, `typ=JWT`, and a registered `kid`.
Only pinned Ed25519 public keys are accepted. Discovery of signing keys from
assertion-supplied URLs is not supported.

Required claims are:

| Claim | Meaning |
| --- | --- |
| `iss` | Registered Dashboard service ID, maximum 128 characters. |
| `sub` | Immutable represented directory UUID, equal to `actor.directoryId`. |
| `aud` | Exactly one configured per-Control delegation audience. |
| `iat`, `nbf`, `exp` | Required integer NumericDate values; positive lifetime of at most 60 seconds. |
| `jti` | Unpadded base64url random value containing 16–64 random bytes. |
| `cnf.x5t#S256` | Unpadded base64url SHA-256 of the presented leaf certificate DER. |
| `actor` | `directoryId`, verified OIDC `issuer`, and verified OIDC `subject`. |
| `operation` | One exact read capability from the route matrix below. |
| `namespaceIds` | Explicit registered namespace UUID subset, 1–320 distinct entries. |
| `mode` | `interactive` or `worker`. Both require current represented-user authorization. |

Signature, issuer, audience, required times, certificate validity/binding, current
service registration, and route operation are checked before dispatch. Clock
leeway is explicitly five seconds; the JWT library's larger default is not used.
Assertions cannot outlive their certificate. Keep Control, PostgreSQL, Dashboard,
and the directory synchronized to trusted time. A stale/future directory proof
or inconsistent database clock fails closed. Incrementing the recovery epoch
after restore invalidates every directory proof until independent verification
completes again.

An assertion is consumed once through a unique `(service_id, assertion_id)`
record. Retrying a request requires minting a new assertion. The audit record
contains service/key identity, represented principal/directory/alias, operation,
namespace scope, mode, issue/expiry times, and an assertion digest. It never
contains the compact JWT, signing key, commands, log bytes, or job names. It
records acceptance of authentication, not a claim that the later resource read
succeeded. Accepted assertions are retained for content-free audit for 90 days by
default; bounded cleanup never removes them during their replay-valid period.

## Exact route matrix

All routes below use GET. The names in braces are routing selectors, not claims
of authority; Control resolves and checks their immutable namespace identity.

| Route | Required assertion operation |
| --- | --- |
| `/v1/me` | `namespace.read` |
| Namespace `/summary`, `/jobs`, `/jobs/{jobID}` | `jobs.read` |
| Collection catalog, complete document, summary, and items | `groups.read` |
| Graph catalog, complete document, summary, nodes, dependencies, neighborhood | `groups.read` |
| Namespace `/targets` and `/targets/{target}` | `targets.read` |
| Job `/logs` metadata | `logs.read` |
| Job `/artifacts` metadata | `artifacts.read` |

`evidence.read` is reserved in the service capability catalog; it does not create
an evidence endpoint. New routes remain denied until explicitly added to the
route matrix. Every execution/admin mutation, policy read, audit export, agent
route, arbitrary proxy route, and service-only event feed rejects actor
assertions. A represented namespace administrator cannot cancel, submit, enroll,
change membership, or operate targets through this boundary. Repository checks
also reject these operations even when called without the HTTP wrapper.

## Current authority and discovery

Each repository operation independently rechecks the enabled service key,
certificate registration, operation, namespace allowlist, approved alias,
account eligibility, current grant union, and directory freshness. Resource
reads and their authorization use one read-only repeatable-read transaction.
Disabling a service/key or removing a certificate/scope takes effect for the next
read; an assertion is not a cached authorization grant. A request already reading
an earlier transaction snapshot may complete that bounded read.

Aliases are operator verified and bind `(issuer, subject)` plus directory UUID
to the existing Control principal UUID. A claimed alias cannot create or merge
an identity. Email and display names are never identity join keys. Managed
namespaces accept only current configured directory contributions; retained
legacy/manual grants cannot survive as an unintended AD-removal bypass.

Delegated `/v1/me` returns `principal.directoryId` from the independent binding
and preserves `principal.id` for ownership filtering. It may report the presented
verified alias. Clients should bind the directory UUID and retain the principal
UUID rather than assuming all applications share one OIDC subject.

For each verified managed namespace the response includes:

- `authorizationCheckedAt`: database decision time.
- `lastDirectoryVerifiedAt`: the older of the account and namespace proofs.
- `authorizationExpiresAt`: that proof time plus 120 seconds.
- `authorizationStatus`: `verified`.

The existing top-level `authorizationCheckedAt` remains a database snapshot,
never directory proof. Legacy unmanaged namespaces omit the proof fields and
are unavailable through delegation. Expired or unavailable proof returns HTTP
503 `authorization_unavailable`, preserving the last grant records. Actual
removal/disabled accounts returns a denial. Refreshing an assertion does not
extend directory freshness. Job and job-list envelopes additionally expose
`asOf` from the actual database read transaction, including empty job lists.

## Operator configuration and rotation

Configure production OIDC and server TLS first, then both:

```text
JOBMAN_CONTROL_DELEGATION_REGISTRY_FILE=/etc/jobman-control/delegation.json
JOBMAN_CONTROL_DELEGATION_CLIENT_CA_FILE=/etc/jobman-control/tls/dashboard-client-ca.pem
JOBMAN_CONTROL_DELEGATION_AUDIT_RETENTION=2160h
```

The audit retention may be increased up to ten years; it cannot be below 90 days.
The public registry format is a JSON object with a `services` array. Each entry
has `serviceId`, `keyId`, `audience`, `publicKey` (standard base64 of 32 public-key
bytes), `certificateThumbprints`, `namespaceIds`, `operations`, and explicit
`enabled`. The registry is limited to 64 keys and one MiB. Private-key properties
and unknown fields are rejected. Generate real signing material outside Control;
never reuse the synthetic test fixtures.

At startup, Control validates the CA and atomically applies the entire public
registry. Keys missing from this file are disabled. All replicas must use the
same approved registry. Existing key material cannot be changed under the same
service/key ID; rotate with a new key ID and an explicit overlap. Certificate
rotation can likewise temporarily register both leaf thumbprints. Operator
updates to the registry table's enabled state take effect without restarting;
keep the file consistent so a later restart does not undo the intended state.

The service CA is added to the server's client trust pool alongside the existing
agent CA. Possession of an agent certificate alone cannot delegate: a registered
service leaf thumbprint and a valid pinned service signature are still required.
Requests without certificates continue to use the existing OIDC path; delegation
always requires a verified TLS chain and completed handshake.

Migration 16 is additive and leaves unmanaged namespaces unchanged. It adds
operator-owned service registrations, approved alias/account and managed-scope
records, a constrained directory contribution view, and replay/audit records.
Managed namespace transition requires the separate directory reconciliation and
migration approval; do not fabricate proof timestamps or seed production aliases
from incoming assertions. Rollback requires the established database restore
procedure because old binaries reject newer migration ledgers.

## Verification

Unit tests cover signing algorithm/type, audience, actor binding, namespace and
operation bounds, expiry/skew, certificate binding, replay rejection, service
registration errors, and strict public configuration. Isolated PostgreSQL tests
cover preserved owners, alias discovery, fresh/stale authority, disabled users,
removed directory grants with surviving legacy rows, service disablement,
immutable key IDs, and repository mutation denial. A real synthetic loopback
mTLS HTTP exercise covers successful reads, replay, missing certificates,
unverified aliases, cross-namespace requests, wrong operation, and cancellation
attempts. Actual corporate AD FS/LDAP and deployed certificate configuration
remain part of the release integration proof.
