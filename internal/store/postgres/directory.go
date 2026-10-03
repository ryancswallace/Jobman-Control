package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

// PlanDirectory validates identity preservation and reports the managed transition.
// It performs no writes, LDAP requests, or proof fabrication.
func (store *Store) PlanDirectory(ctx context.Context, mapping domain.DirectoryMapping) (domain.DirectoryPlan, error) {
	if err := directory.ValidateMapping(mapping); err != nil {
		return domain.DirectoryPlan{}, err
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.DirectoryPlan, error) { return planDirectory(ctx, tx, mapping) })
}

func planDirectory(ctx context.Context, tx pgx.Tx, mapping domain.DirectoryMapping) (domain.DirectoryPlan, error) {
	plan := domain.DirectoryPlan{SourceID: mapping.SourceID, Revision: mapping.Revision, NamespaceCount: len(mapping.Namespaces), IdentityCount: len(mapping.Identities), BindingCount: len(mapping.Bindings)}
	for _, id := range mapping.Namespaces {
		var source string
		err := tx.QueryRow(ctx, `SELECT COALESCE(s.source_id,'') FROM namespaces n LEFT JOIN namespace_directory_state s ON s.namespace_id=n.id WHERE n.id=$1`, id).Scan(&source)
		if errors.Is(err, pgx.ErrNoRows) {
			return plan, domain.ErrNotFound
		}
		if err != nil {
			return plan, err
		}
		if source != "" && source != mapping.SourceID {
			return plan, domain.ErrConflict
		}
		if source == "" {
			plan.NewManagedNamespaces++
		}
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM membership_grants WHERE namespace_id=ANY($1::uuid[]) AND provenance!='directory' AND revoked_at IS NULL`, mapping.Namespaces).Scan(&plan.RetainedNonDirectoryGrants); err != nil {
		return plan, err
	}
	for _, identity := range mapping.Identities {
		var conflicts bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principals WHERE (id=$1 AND (issuer!=$2 OR subject!=$3)) OR (issuer=$2 AND subject=$3 AND id!=$1))
 OR EXISTS(SELECT 1 FROM directory_accounts WHERE (directory_id=$4 OR principal_id=$1) AND (directory_id!=$4 OR principal_id!=$1 OR (source_id IS NOT NULL AND source_id!=$5)))`, identity.PrincipalID, identity.Issuer, identity.Subject, identity.DirectoryID, mapping.SourceID).Scan(&conflicts)
		if err != nil {
			return plan, err
		}
		if conflicts {
			return plan, domain.ErrConflict
		}
		for _, alias := range directory.IdentityAliases(identity) {
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principal_aliases WHERE issuer=$1 AND subject=$2 AND (directory_id!=$3 OR principal_id!=$4 OR (source_id IS NOT NULL AND source_id!=$5)))
 OR EXISTS(SELECT 1 FROM principals WHERE issuer=$1 AND subject=$2 AND id!=$4)`, alias.Issuer, alias.Subject, identity.DirectoryID, identity.PrincipalID, mapping.SourceID).Scan(&conflicts)
			if err != nil {
				return plan, err
			}
			if conflicts {
				return plan, domain.ErrConflict
			}
		}
	}
	for _, binding := range mapping.Bindings {
		var otherSource bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM directory_role_bindings b JOIN namespace_directory_state s ON s.namespace_id=b.namespace_id WHERE b.group_id=$1 AND s.source_id!=$2)`, binding.GroupID, mapping.SourceID).Scan(&otherSource); err != nil {
			return plan, err
		}
		if otherSource {
			return plan, domain.ErrConflict
		}
	}
	return plan, nil
}

// ConfigureDirectory atomically installs approved public mappings. It never
// marks a new alias verified; that requires a complete independent LDAP snapshot.
func (store *Store) ConfigureDirectory(ctx context.Context, mapping domain.DirectoryMapping) error {
	if err := directory.ValidateMapping(mapping); err != nil {
		return err
	}
	digest, err := directory.Digest(mapping)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	_, err = inTransaction(ctx, store.pool, func(tx pgx.Tx) (struct{}, error) {
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('jobman-directory-configuration',0))`); e != nil {
			return struct{}{}, e
		}
		if _, e := planDirectory(ctx, tx, mapping); e != nil {
			return struct{}{}, e
		}
		var revision int64
		var currentDigest string
		e := tx.QueryRow(ctx, `SELECT revision,configuration_digest FROM directory_sources WHERE source_id=$1 FOR UPDATE`, mapping.SourceID).Scan(&revision, &currentDigest)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return struct{}{}, e
		}
		if revision > mapping.Revision || (revision == mapping.Revision && currentDigest != digest) {
			return struct{}{}, domain.ErrConflict
		}
		if revision == mapping.Revision {
			return struct{}{}, nil
		}
		for _, id := range mapping.Namespaces {
			var managed bool
			if managedErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM namespace_directory_state WHERE namespace_id=$1)`, id).Scan(&managed); managedErr != nil {
				return struct{}{}, managedErr
			}
			if !managed && !slices.Contains(mapping.ApprovedTransitions, id) {
				return struct{}{}, errors.New("new managed namespace requires explicit transition approval")
			}
		}
		if _, e = tx.Exec(ctx, `INSERT INTO directory_sources(source_id,revision,configuration_digest,mapping) VALUES($1,$2,$3,$4)
 ON CONFLICT(source_id) DO UPDATE SET revision=EXCLUDED.revision,configuration_digest=EXCLUDED.configuration_digest,mapping=EXCLUDED.mapping,configured_at=transaction_timestamp(),last_verified_at=NULL`, mapping.SourceID, mapping.Revision, digest, encoded); e != nil {
			return struct{}{}, e
		}
		for _, id := range mapping.Namespaces {
			if _, e = tx.Exec(ctx, `INSERT INTO namespace_directory_state(namespace_id,source_id,configuration_revision) VALUES($1,$2,$3)
 ON CONFLICT(namespace_id) DO UPDATE SET configuration_revision=EXCLUDED.configuration_revision,last_verified_at=NULL`, id, mapping.SourceID, mapping.Revision); e != nil {
				return struct{}{}, e
			}
		}
		if _, e = tx.Exec(ctx, `UPDATE namespace_directory_state SET last_verified_at=NULL,configuration_revision=$2 WHERE source_id=$1`, mapping.SourceID, mapping.Revision); e != nil {
			return struct{}{}, e
		}
		if _, e = tx.Exec(ctx, `UPDATE directory_role_bindings SET enabled=false,revision=revision+1 WHERE namespace_id IN(SELECT namespace_id FROM namespace_directory_state WHERE source_id=$1)`, mapping.SourceID); e != nil {
			return struct{}{}, e
		}
		if _, e = tx.Exec(ctx, `UPDATE directory_accounts SET enabled=false,last_verified_at=NULL WHERE source_id=$1`, mapping.SourceID); e != nil {
			return struct{}{}, e
		}
		for _, binding := range mapping.Bindings {
			if _, e = tx.Exec(ctx, `INSERT INTO directory_role_bindings(group_id,namespace_id,role) VALUES($1,$2,$3)
 ON CONFLICT(group_id) DO UPDATE SET namespace_id=EXCLUDED.namespace_id,role=EXCLUDED.role,enabled=true,revision=directory_role_bindings.revision+1,updated_at=transaction_timestamp()`, binding.GroupID, binding.NamespaceID, binding.Role); e != nil {
				return struct{}{}, e
			}
		}
		for _, identity := range mapping.Identities {
			if _, e = tx.Exec(ctx, `INSERT INTO principals(id,issuer,subject,display_name) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, identity.PrincipalID, identity.Issuer, identity.Subject, identity.DisplayName); e != nil {
				return struct{}{}, e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO directory_accounts(directory_id,principal_id,source_id) VALUES($1,$2,$3) ON CONFLICT(directory_id) DO UPDATE SET source_id=EXCLUDED.source_id,enabled=false,last_verified_at=NULL`, identity.DirectoryID, identity.PrincipalID, mapping.SourceID); e != nil {
				return struct{}{}, e
			}
		}
		// The approved map is authoritative for all adopted accounts, including
		// legacy operator aliases with NULL source provenance. Recreate only the
		// declared aliases after independent directory verification.
		if _, e = tx.Exec(ctx, `WITH removed AS(DELETE FROM principal_aliases a WHERE a.source_id=$1 OR a.directory_id IN(SELECT directory_id FROM directory_accounts WHERE source_id=$1) RETURNING directory_id,principal_id,issuer,subject)
 INSERT INTO directory_audit_events(source_id,action,details) SELECT $1,'alias.removed',jsonb_build_object('directoryId',directory_id,'principalId',principal_id,'issuer',issuer,'subject',subject) FROM removed`, mapping.SourceID); e != nil {
			return struct{}{}, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO directory_audit_events(source_id,action,details) VALUES($1,'configuration.applied',jsonb_build_object('revision',$2::bigint,'digest',$3::text,'namespaceCount',$4::int,'identityCount',$5::int))`, mapping.SourceID, mapping.Revision, digest, len(mapping.Namespaces), len(mapping.Identities)); e != nil {
			return struct{}{}, e
		}
		return struct{}{}, nil
	})
	return err
}

// DirectoryRecoveryEpoch fences external reads against a concurrent DB restore.
func (store *Store) DirectoryRecoveryEpoch(ctx context.Context) (int64, error) {
	var epoch int64
	err := store.pool.QueryRow(ctx, `SELECT restore_epoch FROM service_recovery_state WHERE singleton`).Scan(&epoch)
	return epoch, err
}

// ApplyDirectorySnapshot verifies completeness and atomically changes grants.
// All directory I/O has already completed before the transaction begins.
func (store *Store) ApplyDirectorySnapshot(ctx context.Context, snapshot domain.DirectorySnapshot) error {
	_, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (struct{}, error) {
		var mapping domain.DirectoryMapping
		var encoded []byte
		var digest string
		var revision, epoch int64
		var now time.Time
		var previous *time.Time
		err := tx.QueryRow(ctx, `SELECT mapping,configuration_digest,revision,statement_timestamp(),last_verified_at FROM directory_sources WHERE source_id=$1 FOR UPDATE`, snapshot.SourceID).Scan(&encoded, &digest, &revision, &now, &previous)
		if err != nil {
			return struct{}{}, err
		}
		if decodeErr := json.Unmarshal(encoded, &mapping); decodeErr != nil {
			return struct{}{}, decodeErr
		}
		if epochErr := tx.QueryRow(ctx, `SELECT restore_epoch FROM service_recovery_state WHERE singleton FOR SHARE`).Scan(&epoch); epochErr != nil {
			return struct{}{}, epochErr
		}
		if revision != snapshot.Revision || digest != snapshot.Digest || epoch != snapshot.RecoveryEpoch || snapshot.VerifiedAt.After(now.Add(5*time.Second)) || !snapshot.VerifiedAt.After(now.Add(-30*time.Second)) {
			return struct{}{}, domain.ErrConflict
		}
		if previous != nil && !snapshot.VerifiedAt.After(*previous) {
			return struct{}{}, domain.ErrConflict
		}
		if snapshotErr := validateDirectorySnapshot(mapping, snapshot); snapshotErr != nil {
			return struct{}{}, snapshotErr
		}
		accounts := map[string]domain.DirectoryAccountObservation{}
		for _, account := range snapshot.Accounts {
			accounts[account.DirectoryID] = account
		}
		for _, identity := range mapping.Identities {
			account := accounts[identity.DirectoryID]
			var wasEnabled bool
			if accountErr := tx.QueryRow(ctx, `SELECT enabled FROM directory_accounts WHERE directory_id=$1`, identity.DirectoryID).Scan(&wasEnabled); accountErr != nil {
				return struct{}{}, accountErr
			}
			if _, err = tx.Exec(ctx, `UPDATE directory_accounts SET enabled=$2,last_verified_at=$3 WHERE directory_id=$1 AND source_id=$4`, identity.DirectoryID, account.Exists && account.Enabled, snapshot.VerifiedAt, mapping.SourceID); err != nil {
				return struct{}{}, err
			}
			if wasEnabled != (account.Exists && account.Enabled) {
				if _, err = tx.Exec(ctx, `INSERT INTO directory_audit_events(source_id,action,details) VALUES($1,'account.eligibility',jsonb_build_object('directoryId',$2::text,'enabled',$3::boolean))`, mapping.SourceID, identity.DirectoryID, account.Exists && account.Enabled); err != nil {
					return struct{}{}, err
				}
			}
			if account.Exists {
				for _, alias := range directory.IdentityAliases(identity) {
					command, aliasErr := tx.Exec(ctx, `INSERT INTO principal_aliases(issuer,subject,directory_id,principal_id,provenance,source_id) VALUES($1,$2,$3,$4,'operator',$5) ON CONFLICT(issuer,subject) DO NOTHING`, alias.Issuer, alias.Subject, identity.DirectoryID, identity.PrincipalID, mapping.SourceID)
					if aliasErr != nil {
						return struct{}{}, aliasErr
					}
					if command.RowsAffected() > 0 {
						if _, err = tx.Exec(ctx, `INSERT INTO directory_audit_events(source_id,action,details) VALUES($1,'alias.verified',jsonb_build_object('principalId',$2::text,'directoryId',$3::text,'issuer',$4::text,'subject',$5::text))`, mapping.SourceID, identity.PrincipalID, identity.DirectoryID, alias.Issuer, alias.Subject); err != nil {
							return struct{}{}, err
						}
					}
				}
			}
		}
		// Desired contributions use immutable group source keys. Revocation and
		// reactivation update the same grant; overlapping groups remain separate.
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE desired_directory_grants(namespace_id uuid,principal_id uuid,role text,source_key text,PRIMARY KEY(namespace_id,principal_id,source_key)) ON COMMIT DROP`); err != nil {
			return struct{}{}, err
		}
		bindings := map[string]domain.DirectoryBinding{}
		identities := map[string]domain.DirectoryIdentity{}
		for _, b := range mapping.Bindings {
			bindings[b.GroupID] = b
		}
		for _, identity := range mapping.Identities {
			identities[identity.DirectoryID] = identity
		}
		desired := make([][]any, 0)
		for _, group := range snapshot.Groups {
			b := bindings[group.GroupID]
			for _, id := range group.DirectoryIDs {
				account := accounts[id]
				if !account.Exists || !account.Enabled {
					continue
				}
				desired = append(desired, []any{b.NamespaceID, identities[id].PrincipalID, b.Role, b.GroupID})
			}
		}
		if _, err = tx.CopyFrom(ctx, pgx.Identifier{"desired_directory_grants"}, []string{"namespace_id", "principal_id", "role", "source_key"}, pgx.CopyFromRows(desired)); err != nil {
			return struct{}{}, err
		}
		if _, err = tx.Exec(ctx, `WITH removed AS (
 UPDATE membership_grants g SET revoked_at=transaction_timestamp(),updated_at=transaction_timestamp()
 WHERE g.provenance='directory' AND g.revoked_at IS NULL AND g.namespace_id IN(SELECT namespace_id FROM namespace_directory_state WHERE source_id=$1)
 AND NOT EXISTS(SELECT 1 FROM desired_directory_grants d WHERE d.namespace_id=g.namespace_id AND d.principal_id=g.principal_id AND d.source_key=g.source_key AND d.role=g.role)
 RETURNING g.namespace_id,g.principal_id,g.source_key,g.role)
 INSERT INTO audit_events(namespace_id,actor_kind,action,resource_type,resource_id,details) SELECT namespace_id,'system','directory.grant.revoked','principal',principal_id,jsonb_build_object('groupId',source_key,'role',role) FROM removed`, mapping.SourceID); err != nil {
			return struct{}{}, err
		}
		if _, err = tx.Exec(ctx, `WITH granted AS (
 INSERT INTO membership_grants(id,namespace_id,principal_id,role,provenance,source_key)
 SELECT gen_random_uuid(),namespace_id,principal_id,role,'directory',source_key FROM desired_directory_grants
 ON CONFLICT(namespace_id,principal_id,provenance,source_key) DO UPDATE SET role=EXCLUDED.role,revoked_at=NULL,updated_at=transaction_timestamp()
 WHERE membership_grants.revoked_at IS NOT NULL OR membership_grants.role!=EXCLUDED.role
 RETURNING namespace_id,principal_id,source_key,role)
 INSERT INTO audit_events(namespace_id,actor_kind,action,resource_type,resource_id,details) SELECT namespace_id,'system','directory.grant.applied','principal',principal_id,jsonb_build_object('groupId',source_key,'role',role) FROM granted`); err != nil {
			return struct{}{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE namespace_directory_state SET last_verified_at=$2,last_attempt_at=statement_timestamp(),last_error_code=NULL WHERE source_id=$1`, mapping.SourceID, snapshot.VerifiedAt); err != nil {
			return struct{}{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE directory_sources SET last_verified_at=$2,last_attempt_at=statement_timestamp(),last_error_code=NULL,ignored_direct_members=$3 WHERE source_id=$1`, mapping.SourceID, snapshot.VerifiedAt, snapshot.IgnoredDirectMembers); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("apply directory snapshot: %w", err)
	}
	return nil
}

func validateDirectorySnapshot(mapping domain.DirectoryMapping, snapshot domain.DirectorySnapshot) error {
	if len(snapshot.Accounts) != len(mapping.Identities) || len(snapshot.Groups) != len(mapping.Bindings) || snapshot.IgnoredDirectMembers < 0 || snapshot.IgnoredDirectMembers > 100000 {
		return errors.New("directory snapshot is incomplete")
	}
	accounts, groups := map[string]bool{}, map[string]bool{}
	for _, identity := range mapping.Identities {
		accounts[identity.DirectoryID] = true
	}
	for _, b := range mapping.Bindings {
		groups[b.GroupID] = true
	}
	seenAccounts, seenGroups := map[string]bool{}, map[string]bool{}
	for _, a := range snapshot.Accounts {
		if !accounts[a.DirectoryID] || seenAccounts[a.DirectoryID] || (!a.Exists && a.Enabled) {
			return errors.New("directory account snapshot is invalid")
		}
		seenAccounts[a.DirectoryID] = true
	}
	total := 0
	for _, g := range snapshot.Groups {
		if !groups[g.GroupID] || seenGroups[g.GroupID] || (!g.Exists && len(g.DirectoryIDs) > 0) {
			return errors.New("directory group snapshot is invalid")
		}
		seenGroups[g.GroupID] = true
		seen := map[string]bool{}
		for _, id := range g.DirectoryIDs {
			if !accounts[id] || seen[id] {
				return errors.New("directory group contains an unresolved or duplicate identity")
			}
			seen[id] = true
			total++
		}
	}
	if total > 100000 {
		return errors.New("directory snapshot exceeds membership bound")
	}
	return nil
}

// RecordDirectoryFailure preserves the last complete grants and proof timestamp.
func (store *Store) RecordDirectoryFailure(ctx context.Context, sourceID string, revision int64) error {
	_, err := store.pool.Exec(ctx, `WITH failed AS(UPDATE directory_sources SET last_attempt_at=statement_timestamp(),last_error_code='verification_failed' WHERE source_id=$1 AND revision=$2 RETURNING source_id)
 UPDATE namespace_directory_state SET last_attempt_at=statement_timestamp(),last_error_code='verification_failed' WHERE source_id IN(SELECT source_id FROM failed)`, sourceID, revision)
	return err
}

// PruneDirectoryAudits bounds content-free operator history using the same
// approved minimum 90-day retention as delegated read authentication records.
func (store *Store) PruneDirectoryAudits(ctx context.Context, limit int, retention time.Duration) (int, error) {
	if limit < 1 || limit > 10000 || retention < 90*24*time.Hour || retention > 3650*24*time.Hour {
		return 0, errors.New("directory audit retention bounds are invalid")
	}
	command, err := store.pool.Exec(ctx, `DELETE FROM directory_audit_events WHERE id IN(SELECT id FROM directory_audit_events WHERE recorded_at<statement_timestamp()-($1::bigint*interval '1 second') ORDER BY recorded_at LIMIT $2)`, int64(retention.Seconds()), limit)
	if err != nil {
		return 0, fmt.Errorf("prune directory audit: %w", err)
	}
	return int(command.RowsAffected()), nil
}
