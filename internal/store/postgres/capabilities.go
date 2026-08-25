package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// RecordAgentCapabilities stores one immutable target-side observation and
// refreshes the assignment projection. Identical request documents replay the
// same revision.
func (store *Store) RecordAgentCapabilities(
	ctx context.Context,
	identity domain.AgentIdentity,
	capabilities domain.AgentCapabilities,
) (domain.AgentCapabilitySnapshot, error) {
	if err := domain.ValidateAgentCapabilities(capabilities); err != nil {
		return domain.AgentCapabilitySnapshot{}, err
	}
	if capabilities.AgentID != identity.AgentID {
		return domain.AgentCapabilitySnapshot{}, domain.ErrForbidden
	}
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.AgentCapabilitySnapshot, error) {
		var currentRevision int64
		var operatingSystem, architecture, hostname, executionUser, targetBackend string
		var targetRuntimes []string
		lookupErr := tx.QueryRow(ctx, `
			SELECT a.capability_revision, a.operating_system,
				a.architecture, a.hostname, a.execution_user,
				tg.execution_backend, tg.runtimes
			FROM agents AS a
			JOIN targets AS t ON t.id = a.target_id
			JOIN target_generations AS tg ON tg.id = a.target_generation_id
			WHERE a.id = $1 AND a.namespace_id = $2
				AND a.target_generation_id = $3
				AND a.status IN ('active', 'draining')
				AND t.state IN ('active', 'draining')
			FOR UPDATE OF a
		`, identity.AgentID, identity.NamespaceID, identity.TargetGenerationID).Scan(
			&currentRevision, &operatingSystem, &architecture,
			&hostname, &executionUser, &targetBackend, &targetRuntimes,
		)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.AgentCapabilitySnapshot{}, domain.ErrConflict
		}
		if lookupErr != nil {
			return domain.AgentCapabilitySnapshot{}, fmt.Errorf("lock agent capability projection: %w", lookupErr)
		}
		var replayRevision int64
		replayErr := tx.QueryRow(ctx, `
			SELECT revision FROM agent_capability_snapshots
			WHERE agent_id = $1 AND document_digest = $2
		`, identity.AgentID, capabilities.DocumentDigest).Scan(&replayRevision)
		if replayErr == nil {
			return domain.AgentCapabilitySnapshot{
				AgentCapabilities: capabilities, Revision: replayRevision, Replayed: true,
			}, nil
		}
		if !errors.Is(replayErr, pgx.ErrNoRows) {
			return domain.AgentCapabilitySnapshot{}, fmt.Errorf("inspect capability replay: %w", replayErr)
		}
		if operatingSystem != capabilities.OperatingSystem || architecture != capabilities.Architecture ||
			hostname != capabilities.Hostname ||
			executionUser != capabilities.ExecutionUser {
			return domain.AgentCapabilitySnapshot{}, domain.ErrConflict
		}
		if !slices.Contains(capabilities.ExecutionBackends, targetBackend) ||
			!isSubset(capabilities.Runtimes, targetRuntimes) {
			return domain.AgentCapabilitySnapshot{}, domain.ErrConflict
		}
		revision := currentRevision + 1
		if _, insertErr := tx.Exec(ctx, `
			INSERT INTO agent_capability_snapshots (
				namespace_id, agent_id, revision, observed_at, accepting_assignments,
				agent_version, operating_system, architecture, hostname, execution_user,
				execution_backends, runtimes, capabilities, document_digest, document
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15::jsonb)
		`, identity.NamespaceID, identity.AgentID, revision, capabilities.ObservedAt,
			capabilities.AcceptingAssignments, capabilities.AgentVersion,
			capabilities.OperatingSystem, capabilities.Architecture, capabilities.Hostname,
			capabilities.ExecutionUser, nonNilStrings(capabilities.ExecutionBackends),
			nonNilStrings(capabilities.Runtimes), nonNilStrings(capabilities.Capabilities),
			capabilities.DocumentDigest, string(capabilities.Document)); insertErr != nil {
			return domain.AgentCapabilitySnapshot{}, fmt.Errorf("insert capability snapshot: %w", insertErr)
		}
		if _, updateErr := tx.Exec(ctx, `
			UPDATE agents
			SET agent_version = $2, execution_backends = $3, runtimes = $4, capabilities = $5,
				accepting_assignments = $6, capability_revision = $7,
				last_capability_at = $8, updated_at = transaction_timestamp()
			WHERE id = $1
		`, identity.AgentID, capabilities.AgentVersion, nonNilStrings(capabilities.ExecutionBackends),
			nonNilStrings(capabilities.Runtimes), nonNilStrings(capabilities.Capabilities),
			capabilities.AcceptingAssignments, revision, capabilities.ObservedAt); updateErr != nil {
			return domain.AgentCapabilitySnapshot{}, fmt.Errorf("update agent capability projection: %w", updateErr)
		}
		if _, auditErr := tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_kind, actor_agent_id, action,
				resource_type, resource_id, request_digest, details
			) VALUES ($1, 'agent', $2, 'agent.capabilities_reported', 'agent', $2, $3,
				jsonb_build_object('revision', $4::bigint, 'acceptingAssignments', $5::boolean))
		`, identity.NamespaceID, identity.AgentID, capabilities.DocumentDigest,
			revision, capabilities.AcceptingAssignments); auditErr != nil {
			return domain.AgentCapabilitySnapshot{}, fmt.Errorf("audit capability snapshot: %w", auditErr)
		}

		return domain.AgentCapabilitySnapshot{
			AgentCapabilities: capabilities, Revision: revision,
		}, nil
	})
	if err != nil {
		return domain.AgentCapabilitySnapshot{}, fmt.Errorf("record agent capabilities: %w", err)
	}

	return result, nil
}

func isSubset(values, allowed []string) bool {
	for _, value := range values {
		if !slices.Contains(allowed, value) {
			return false
		}
	}

	return true
}
