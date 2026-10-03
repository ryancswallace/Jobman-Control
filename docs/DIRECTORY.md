# Active Directory authorization

Control can reconcile dedicated Active Directory role groups over LDAPS. The
`directory-authorization` capability means this implementation is available; a
client must still require fresh per-namespace proof from `/v1/me`. Installing
the binary does not configure or approve an organization's directory access.

## Identity and direct membership

An operator-owned JSON configuration maps stable group object GUIDs one-to-one
to a namespace UUID and a Control role. The same user can be a direct member of
several mapped groups; their effective capabilities are the exact role union.
Removing one membership removes only that contribution. Nested groups are not
expanded, and primary-group membership does not confer access. Renaming a group
or moving a user within the configured directory base preserves its object ID.

Each configured user has an immutable directory UUID, a preserved Control
principal UUID, a canonical issuer/subject, and any independently approved OIDC
aliases. Explicit new principal UUIDs are supported, but an existing principal
or alias cannot be assigned to a different identity. No email or display-name
match is used. Configuration conflicts fail before changing authority. Database
constraints also protect aliases from concurrent ordinary principal creation.
The declared alias set replaces every earlier alias for an adopted account,
including legacy aliases without source provenance. Omitted aliases are removed
and audited; later configuration revisions cannot silently retain a removed alias.

The reader looks up each approved user object and its current eligibility, then
all configured groups' explicit `member` values. It uses a single authenticated
LDAPS connection, validates the configured CA and hostname, rejects referrals,
and queries immutable object GUIDs. Unique-object searches have a two-entry
limit; an error or duplicate is not treated as a missing object. Group values
are fetched with bounded range requests until a terminal range is returned.
Missing ranges, gaps, malformed ranges, and changes to `uSNChanged` during or
after retrieval invalidate the entire snapshot. Read behavior follows
[Microsoft's range retrieval contract](https://learn.microsoft.com/en-us/windows/win32/adsi/attribute-range-retrieval).
User object versions and distinguished names are rechecked after group reads so
a concurrent rename or reused name cannot assign another user's memberships.
The client enforces entry limits even if the server ignores them. Before BER
decoding, each response is limited to two MiB, each connection to 32 MiB, and
constructed nesting/element counts are bounded. Oversized or malformed responses
fail the cycle without refreshing proof. Definite-length framing follows
[LDAP protocol encoding](https://www.rfc-editor.org/rfc/rfc4511#section-5.1).

Only configured, resolved user objects can confer access. Nested groups,
unmapped users, and foreign-domain members produce a bounded operator count and
no grants. Cross-domain resolution is not enabled by default. Configure an
approved resolver and complete its acceptance exercise before supporting a
foreign domain. The configured read identity needs permission to read user
eligibility, object GUIDs, group membership, and group change versions throughout
the configured base. Validate that permission with the directory owner.

Disabled users, deleted/missing users, and expired AD accounts are ineligible.
The explicit disable bit and AD `accountExpires` are checked; account expiry is
directory eligibility and does not add a scheduled lifetime to namespace grants.
Missing eligibility attributes fail the snapshot. Lockout policy remains owned
by AD FS; Control does not infer it from an incomplete attribute.

## Timing and failure behavior

The service starts reconciliation immediately, then every 30 seconds. A cycle
has a 25-second total budget, with at most 20 seconds for LDAPS. The proof time
is the start of external directory reads, never the later database commit.
Reads occur outside transactions; a complete snapshot atomically changes grants,
audits, account eligibility, and proof timestamps. Each configuration supports
up to 320 namespaces, 1,280 groups, 10,000 approved identities, and 100,000 direct
memberships per snapshot. These are safety bounds, not validated capacity claims.

The target is removal within 60 seconds after a change becomes visible to the
configured directory endpoint. Measure directory replication separately. After
120 seconds without fresh account and namespace proof, sensitive reads fail with
`authorization_unavailable`. A failed/truncated query preserves prior grants and
their original proof time. A complete group deletion or member removal revokes
only its contribution. Current eligibility applies equally to interactive and
background delegated reads and ordinary Control clients in managed namespaces.

Snapshots are fenced by the configuration revision/digest and database recovery
epoch. Older overlapping snapshots cannot overwrite newer proof. A restore
requires a new complete directory read, and an old read in flight cannot apply
after recovery. The assertion recovery boundary is documented in
[Read-only service delegation](DELEGATION.md). Keep all clocks synchronized; the
database rejects future proof beyond five seconds and snapshots older than the
bounded cycle window.

## Configuration preview and approved transition

Use the synthetic [configuration example](../etc/jobman-control/directory.example.json)
as a template. Replace every placeholder through the normal operator approval
process. The configuration contains public mappings and file references only;
the LDAPS bind password is read from a separately protected file on each cycle.
An empty password is rejected, preventing accidental anonymous binding. The
approved CA is likewise loaded each cycle, supporting deliberate rotation.

```text
JOBMAN_CONTROL_DIRECTORY_CONFIG_FILE=/etc/jobman-control/directory.json
JOBMAN_CONTROL_DIRECTORY_MODE=preview
```

Preview is the default when a directory file is supplied. The process validates
the mapping against the database, reports counts of new managed namespaces and
retained non-directory grants, then exits without starting the API or modifying
directory authority. Use `JOBMAN_CONTROL_MIGRATE_ON_START=false` when requesting
a strictly read-only preview; migrations are a separate prerequisite. Preview
does not test LDAPS connectivity or approve the identity bindings.

Before approval, verify each canonical principal UUID and issuer/subject,
separately verify every application alias, inventory existing grants, and review
the namespace transition counts. Existing owner UUIDs remain unchanged.
Production transition requires the operator's explicit approval. Record each
approved namespace UUID in `mapping.approvedTransitions`, then select
`JOBMAN_CONTROL_DIRECTORY_MODE=enforce`. New managed namespaces require that
explicit list. Configuration revision must increase whenever the mapping
changes; same revision with different contents or an older revision is rejected.
All replicas must use the same current configuration.

Enforcement initially invalidates proof. It creates approved account records but
does not create usable aliases until an independent complete LDAP read resolves
the corresponding user. Sensitive access remains unavailable until then. Existing
managed configurations can start during a directory outage: the reconciler
retries while proof expires. Operators may inspect `directory_sources` and
`namespace_directory_state` using the restricted database administration path;
these records expose attempt/proof times and bounded failure codes. Logs do not
include raw directory errors, member DNs, passwords, or tokens.

Once a namespace is managed, retained legacy/manual grants do not contribute.
Membership-writing APIs reject changes in managed namespaces. Removing a
namespace or binding from a subsequent configuration never silently restores
legacy authority: that namespace remains managed and the next complete snapshot
removes the obsolete directory contributions. Removed configured accounts are
disabled immediately. A reverse transition requires a separate reviewed
migration; deleting state rows is not a supported rollback.

Operator configuration and eligibility/alias changes have content-free records
in `directory_audit_events`; actual role changes have namespace audit events.
The directory audit uses the same configurable 90-day minimum retention as
delegation audits. Configuration files must be kept consistent with approved
state so a restart cannot restore obsolete policy.

## Upgrade and acceptance

Migration 17 adds source configuration, ownership, bounded audit retention, and
principal/alias collision guards. It leaves existing unmanaged namespaces
unchanged. Back up first, run forward migrations, preview, then approve the
transition. Older binaries reject the newer migration ledger; rollback uses the
documented database restore procedure and fresh directory verification.

Automated checks cover direct versus nested membership, AD GUID byte order,
range completeness/change detection, missing/deleted groups, disabled/expired
users, referral/transport failure, role union/removal, alias preservation and
conflicts, manual-grant denial, stale proof, configuration/recovery races, and
atomic PostgreSQL application. Production acceptance still requires the actual
AD endpoint/permissions, representative large groups, observed replication and
revocation timing, certificate rotation, outage/recovery, and AD FS alias proof.
Synthetic tests do not establish those environmental facts.
