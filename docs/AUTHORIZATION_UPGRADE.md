# Authorization grants and upgrade

Control authorizes a namespace through every active contributing grant for the
verified principal. Capabilities are the exact set union, independently in each
namespace. The current catalog preserves existing Control behavior:

| Roles receiving a capability | Capabilities |
| --- | --- |
| All four built-in roles | `namespace.read`, `jobs.read`, `groups.read`, `targets.read`, `logs.read`, `artifacts.read`, `evidence.read`, `reports.read`, `diagnosis.request`, `policy.read`, `jobs.cancel.own` |
| Submitter, operator, namespace administrator | `jobs.submit`, `enrollment.create.own` |
| Operator, namespace administrator | `jobs.cancel.any`, `targets.operate`, `audit.read` |
| Namespace administrator | `enrollment.create.any`, `targets.manage`, `memberships.manage`, `policy.manage` |

Names use the API role values `viewer`, `submitter`, `operator`, and
`namespace_admin`. The catalog describes authority; evidence/report capabilities
do not imply those upcoming service endpoints already exist. Dashboard must
still restrict itself to monitoring, even for users whose Control capabilities
include job mutation. All current members can read everyone's jobs in their
namespace. A viewer can cancel a job they previously submitted, preserving the
existing owner rule; another user's job requires `jobs.cancel.any`.

## Schema and compatibility

Migration `000013_membership_grants.sql` copies existing membership rows to
`legacy` contributions and records `membership.legacy.migrated` audit entries.
No existing grant is discarded. Each new manual contribution has a stable UUID,
role, principal, namespace, timestamps, and provenance. Revocation tombstones
retain identity so replay cannot restore access. There is no grant expiry field.
The authorization revision increases when a contribution changes, including
changes that leave overlapping capabilities available.

The old `memberships` table remains the legacy write surface. Its database
trigger maintains only the associated legacy contribution. Existing membership
PUT, development bootstrap, and legacy deletion therefore preserve new manual
contributions. All service authorization reads use `authorized_memberships`,
which groups active grants into one row per namespace/principal. This avoids
multiplying job, target, log, artifact, group, and policy results. No route returns
an invented single highest role for a multi-role user.

Before upgrade, stop writers, back up the database, and run the normal migrate
command with the new binary. Restart only binaries that know the complete
migration ledger. The readiness check rejects an older binary against this
schema. Do not run a rolling mixture with already-running old binaries: those
would continue reading only the legacy table. Test discovery, owner cancellation,
operator audit, overlapping grants, and last-grant removal before restoring use.

Rollback to an old binary requires a service hold and restoration of a pre-upgrade
backup, followed by reconciliation of access changes. There is no destructive
reverse migration and no safe conversion of arbitrary grant sets to one role.
Never delete the migration ledger entry to make an old binary start.

## AD-managed rollout dependency

This slice adds the grant and repository-authorization foundation. Direct AD
reconciliation, immutable directory aliases, complete-group snapshot checks,
account eligibility, managed-namespace policy, and directory freshness failure
handling remain separate implementation work. The schema reserves `directory`
provenance but public manual endpoints cannot assert it. No production namespace
is declared AD-managed by this migration.

Before enabling directory authority, inventory legacy and manual grants for each
managed namespace, compare proposed direct AD bindings, and explicitly reconcile
unapproved manual contributions. An unnoticed legacy grant must not retain access
after an AD removal. Future directory writes must preserve independent source
provenance, update the authorization revision, audit additions/removals, and never
interpret an incomplete query as an empty group.

`GET /v1/me` reports `authorizationCheckedAt` from its database statement snapshot.
It is not directory freshness evidence. Its decimal-string revision is useful for
cache invalidation, but neither revision nor timestamp authorizes a later request.
Every resource read repeats repository authorization using current contributions.
