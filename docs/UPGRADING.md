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

## Agent recovery migration 23

Migration `000023_agent_recovery.sql` adds an initially empty recovery ledger
and an agent lookup index. Existing agents, assignments, executions, sessions
and certificates are unchanged. The ledger stores public certificate responses,
request digests and credential identifiers; no private keys or clear tokens.
Normal enrollment and bearer renewal clients remain compatible. Upgrade Control
before clients use the new recovery route. Mixed binaries remain unsupported
because schema validation requires the exact migration set.

Back up PostgreSQL before applying migration 23. Rollback requires the matching
pre-upgrade database and old binary; it must also account for credentials issued
or revoked after that backup. Stop agents before recovery and retain their
private state and execution journal. Use the [recovery contract](API.md#agent-credential-recovery)
to restore the same identity; never replace its state directory as an expiry
workaround. Target-generation changes and explicit revocation require operator
review and cannot be bypassed by recovery.
