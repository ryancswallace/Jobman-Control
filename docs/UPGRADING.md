# Upgrading

Before every upgrade, read `CHANGELOG.md`, back up PostgreSQL and required
external artifact stores, retain the current binary and secret material, and
test the candidate against a restored copy.

Apply the candidate's embedded migrations in a controlled step. Migrations are
forward-only and atomically checksummed. Start the candidate with automatic
migration disabled to verify exact schema agreement, then replace all replicas.
Mixed Control versions have no pre-v1 compatibility guarantee.

If verification fails before traffic shifts, keep the old binary and unchanged
database. If a migration has committed, rollback requires restoring the
pre-upgrade database and matching old binary together. Never run an older
binary against a newer migration ledger.

After upgrade, verify readiness, OIDC authentication, namespace membership,
agent mTLS, one assignment and cancellation path, both terminal log streams,
and graceful shutdown. See [operations](OPERATIONS.md) for recovery details.
