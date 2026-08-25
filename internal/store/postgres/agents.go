package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const createEnrollmentOperation = "agent-enrollment-tokens.create"

// CreateEnrollmentToken creates a short-lived, one-time credential bound to
// the current target generation and an existing namespace member.
func (store *Store) CreateEnrollmentToken(
	ctx context.Context,
	actor domain.Principal,
	namespace string,
	targetName string,
	idempotencyKey string,
	requestDigest string,
	request domain.EnrollmentRequest,
) (domain.EnrollmentToken, error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.EnrollmentToken{}, errors.New("invalid idempotency key")
	}
	if err := domain.ValidateEnrollmentRequest(request); err != nil {
		return domain.EnrollmentToken{}, err
	}
	enrollmentID, err := store.newID()
	if err != nil {
		return domain.EnrollmentToken{}, err
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.EnrollmentToken, error) {
		authorization, authorizeErr := authorizeNamespace(
			ctx, tx, actor, namespace,
			domain.RoleSubmitter, domain.RoleOperator, domain.RoleNamespaceAdmin,
		)
		if authorizeErr != nil {
			return domain.EnrollmentToken{}, authorizeErr
		}
		if authorization.role != domain.RoleNamespaceAdmin && request.Principal != actor {
			return domain.EnrollmentToken{}, domain.ErrForbidden
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, createEnrollmentOperation, "agent_enrollment_token",
			idempotencyKey, requestDigest, 201,
		)
		if reserveErr != nil {
			return domain.EnrollmentToken{}, reserveErr
		}
		if replayed {
			token, lookupErr := store.getEnrollmentToken(ctx, tx, authorization.namespaceID, resourceID)
			token.Replayed = true
			return token, lookupErr
		}

		var targetID string
		var generationID string
		var state string
		if queryErr := tx.QueryRow(ctx, `
			SELECT t.id::text, tg.id::text, t.state
			FROM targets AS t
			JOIN target_generations AS tg ON tg.id = t.current_generation_id
			WHERE t.namespace_id = $1 AND t.name = $2
		`, authorization.namespaceID, targetName).Scan(&targetID, &generationID, &state); queryErr != nil {
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return domain.EnrollmentToken{}, domain.ErrNotFound
			}
			return domain.EnrollmentToken{}, fmt.Errorf("resolve enrollment target: %w", queryErr)
		}
		if state != "active" {
			return domain.EnrollmentToken{}, domain.ErrConflict
		}
		var enrolledPrincipalID string
		if queryErr := tx.QueryRow(ctx, `
			SELECT p.id::text
			FROM principals AS p
			JOIN memberships AS m ON m.principal_id = p.id
			WHERE m.namespace_id = $1 AND p.issuer = $2 AND p.subject = $3
		`, authorization.namespaceID, request.Principal.Issuer, request.Principal.Subject).
			Scan(&enrolledPrincipalID); queryErr != nil {
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return domain.EnrollmentToken{}, domain.ErrNotFound
			}
			return domain.EnrollmentToken{}, fmt.Errorf("resolve enrollment principal: %w", queryErr)
		}
		clearToken := store.deriveToken("jme", enrollmentID)
		var expiresAt time.Time
		if queryErr := tx.QueryRow(ctx, `
			INSERT INTO agent_enrollment_tokens (
				id, namespace_id, target_id, target_generation_id,
				principal_id, created_by_principal_id, expected_user,
				token_hash, request_digest, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
				transaction_timestamp() + $10::interval)
			RETURNING expires_at
		`, enrollmentID, authorization.namespaceID, targetID, generationID,
			enrolledPrincipalID, authorization.principalID, request.ExpectedUser,
			hashToken(clearToken), requestDigest, request.Lifetime.String(),
		).Scan(&expiresAt); queryErr != nil {
			return domain.EnrollmentToken{}, fmt.Errorf("insert agent enrollment token: %w", queryErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, createEnrollmentOperation,
			idempotencyKey, enrollmentID, 201,
		); completeErr != nil {
			return domain.EnrollmentToken{}, completeErr
		}
		if auditErr := auditEnrollment(
			ctx, tx, authorization, enrollmentID, generationID,
			idempotencyKey, requestDigest, request,
		); auditErr != nil {
			return domain.EnrollmentToken{}, auditErr
		}

		return domain.EnrollmentToken{
			ID: enrollmentID, Namespace: namespace, Target: targetName,
			TargetGenerationID: generationID, Principal: request.Principal,
			ExpectedUser: request.ExpectedUser, Token: clearToken,
			ExpiresAt: expiresAt.UTC(),
		}, nil
	})
	if err != nil {
		return domain.EnrollmentToken{}, fmt.Errorf("create enrollment token: %w", err)
	}

	return result, nil
}

// EnrollAgent consumes a one-time token or safely replays the original
// enrollment response when the same registration digest is retried.
func (store *Store) EnrollAgent(
	ctx context.Context,
	enrollmentToken string,
	registration domain.AgentRegistration,
	sessionLifetime time.Duration,
) (domain.AgentSession, error) {
	if err := domain.ValidateAgentRegistration(registration); err != nil {
		return domain.AgentSession{}, err
	}
	if !validSessionLifetime(sessionLifetime) {
		return domain.AgentSession{}, errors.New("agent session lifetime is invalid")
	}
	agentID, err := store.newID()
	if err != nil {
		return domain.AgentSession{}, err
	}
	sessionID, err := store.newID()
	if err != nil {
		return domain.AgentSession{}, err
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.AgentSession, error) {
		binding, lookupErr := lookupEnrollmentForUpdate(ctx, tx, enrollmentToken)
		if lookupErr != nil {
			return domain.AgentSession{}, lookupErr
		}
		if binding.usedAgentID != "" {
			return store.replayEnrollment(ctx, tx, binding, registration.RequestDigest)
		}
		if binding.expired || binding.targetState != "active" ||
			binding.generationID != registration.TargetGenerationID {
			return domain.AgentSession{}, domain.ErrConflict
		}
		if binding.expectedUser != registration.ExecutionUser ||
			!slices.Contains(registration.ExecutionBackends, binding.executionBackend) ||
			!allowsValue(binding.operatingSystems, registration.OperatingSystem) ||
			!allowsValue(binding.architectures, registration.Architecture) {
			return domain.AgentSession{}, domain.ErrForbidden
		}
		if _, insertErr := tx.Exec(ctx, `
			INSERT INTO agents (
				id, namespace_id, target_id, target_generation_id, principal_id,
				agent_version, protocol_versions, operating_system, architecture,
				hostname, execution_user, execution_backends, runtimes,
				capabilities, registration_digest, last_seen_at, last_capability_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
				transaction_timestamp(), transaction_timestamp()
			)
		`, agentID, binding.namespaceID, binding.targetID, binding.generationID,
			binding.principalID, registration.AgentVersion, registration.ProtocolVersions,
			registration.OperatingSystem, registration.Architecture, registration.Hostname,
			registration.ExecutionUser, registration.ExecutionBackends,
			nonNilStrings(registration.Runtimes), nonNilStrings(registration.Capabilities),
			registration.RequestDigest,
		); insertErr != nil {
			return domain.AgentSession{}, fmt.Errorf("insert agent: %w", insertErr)
		}
		clearSessionToken := store.deriveToken("jms", sessionID)
		var expiresAt time.Time
		if insertErr := tx.QueryRow(ctx, `
			INSERT INTO agent_sessions (
				id, namespace_id, agent_id, token_hash, expires_at
			) VALUES ($1, $2, $3, $4, transaction_timestamp() + $5::interval)
			RETURNING expires_at
		`, sessionID, binding.namespaceID, agentID, hashToken(clearSessionToken), sessionLifetime.String()).
			Scan(&expiresAt); insertErr != nil {
			return domain.AgentSession{}, fmt.Errorf("insert agent session: %w", insertErr)
		}
		if commandTag, updateErr := tx.Exec(ctx, `
			UPDATE agent_enrollment_tokens
			SET used_at = transaction_timestamp(), used_by_agent_id = $2
			WHERE id = $1 AND used_at IS NULL
		`, binding.enrollmentID, agentID); updateErr != nil || commandTag.RowsAffected() != 1 {
			if updateErr != nil {
				return domain.AgentSession{}, fmt.Errorf("consume enrollment token: %w", updateErr)
			}
			return domain.AgentSession{}, errors.New("enrollment token consumption was lost")
		}
		if _, auditErr := tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type,
				resource_id, request_digest, details
			) VALUES ($1, $2, 'agent.enrolled', 'agent', $3, $4,
				jsonb_build_object('targetGenerationId', $5::text))
		`, binding.namespaceID, binding.principalID, agentID,
			registration.RequestDigest, binding.generationID); auditErr != nil {
			return domain.AgentSession{}, fmt.Errorf("audit agent enrollment: %w", auditErr)
		}

		return domain.AgentSession{
			AgentID: agentID, SessionID: sessionID,
			Token: clearSessionToken, ExpiresAt: expiresAt.UTC(),
		}, nil
	})
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("enroll agent: %w", err)
	}

	return result, nil
}

// RenewAgentSession atomically rotates one still-valid inert agent token.
func (store *Store) RenewAgentSession(
	ctx context.Context,
	currentToken string,
	sessionLifetime time.Duration,
) (domain.AgentSession, error) {
	if !validSessionLifetime(sessionLifetime) {
		return domain.AgentSession{}, errors.New("agent session lifetime is invalid")
	}
	newSessionID, err := store.newID()
	if err != nil {
		return domain.AgentSession{}, err
	}
	newToken := store.deriveToken("jms", newSessionID)

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.AgentSession, error) {
		var sessionID string
		var agentID string
		var namespaceID string
		queryErr := tx.QueryRow(ctx, `
			SELECT s.id::text, a.id::text, a.namespace_id::text
			FROM agent_sessions AS s
			JOIN agents AS a ON a.id = s.agent_id
			JOIN targets AS t ON t.id = a.target_id
			WHERE s.token_hash = $1 AND s.revoked_at IS NULL
				AND s.expires_at > transaction_timestamp()
			AND a.status IN ('active', 'draining')
			AND t.state IN ('active', 'draining')
		FOR UPDATE OF s
		`, hashToken(currentToken)).Scan(&sessionID, &agentID, &namespaceID)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			return domain.AgentSession{}, domain.ErrUnauthenticated
		}
		if queryErr != nil {
			return domain.AgentSession{}, fmt.Errorf("authorize session renewal: %w", queryErr)
		}
		var expiresAt time.Time
		if queryErr = tx.QueryRow(ctx, `
			INSERT INTO agent_sessions (
				id, namespace_id, agent_id, token_hash, expires_at
			) VALUES ($1, $2, $3, $4, transaction_timestamp() + $5::interval)
			RETURNING expires_at
		`, newSessionID, namespaceID, agentID, hashToken(newToken), sessionLifetime.String()).
			Scan(&expiresAt); queryErr != nil {
			return domain.AgentSession{}, fmt.Errorf("insert renewed agent session: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE agent_sessions
			SET revoked_at = transaction_timestamp(), replaced_by_session_id = $2
			WHERE id = $1
		`, sessionID, newSessionID); queryErr != nil {
			return domain.AgentSession{}, fmt.Errorf("revoke replaced agent session: %w", queryErr)
		}

		return domain.AgentSession{
			AgentID: agentID, SessionID: newSessionID,
			Token: newToken, ExpiresAt: expiresAt.UTC(),
		}, nil
	})
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("renew agent session: %w", err)
	}

	return result, nil
}

// AuthenticateAgent validates an opaque session token against database time.
func (store *Store) AuthenticateAgent(ctx context.Context, token string) (domain.AgentIdentity, error) {
	var identity domain.AgentIdentity
	err := store.pool.QueryRow(ctx, `
		UPDATE agent_sessions AS s
		SET last_seen_at = transaction_timestamp()
		FROM agents AS a, targets AS t, namespaces AS n
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL
			AND s.expires_at > transaction_timestamp()
			AND a.id = s.agent_id AND a.status IN ('active', 'draining')
			AND t.id = a.target_id AND t.state IN ('active', 'draining')
			AND n.id = a.namespace_id
		RETURNING a.id::text, a.namespace_id::text, n.name,
			a.principal_id::text, a.target_id::text, a.target_generation_id::text
	`, hashToken(token)).Scan(
		&identity.AgentID, &identity.NamespaceID, &identity.Namespace,
		&identity.PrincipalID, &identity.TargetID, &identity.TargetGenerationID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentIdentity{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.AgentIdentity{}, fmt.Errorf("authenticate agent: %w", err)
	}

	return identity, nil
}

// RecordAgentCertificate binds a newly issued certificate to an enrolled
// agent. Identical retries are safe.
func (store *Store) RecordAgentCertificate(
	ctx context.Context,
	agentID string,
	certificate domain.AgentCertificate,
) error {
	commandTag, err := store.pool.Exec(ctx, `
		INSERT INTO agent_certificates (
			serial, namespace_id, agent_id, public_key_digest, not_after
		)
		SELECT $2, namespace_id, id, $3, $4
		FROM agents
		WHERE id = $1 AND status = 'active'
		ON CONFLICT (serial) DO UPDATE
		SET serial = EXCLUDED.serial
		WHERE agent_certificates.agent_id = EXCLUDED.agent_id
			AND agent_certificates.public_key_digest = EXCLUDED.public_key_digest
			AND agent_certificates.not_after = EXCLUDED.not_after
	`, agentID, certificate.Serial, certificate.PublicKeyDigest, certificate.ExpiresAt)
	if err != nil {
		return fmt.Errorf("record agent certificate: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return domain.ErrConflict
	}

	return nil
}

// RotateAgentCertificate installs a new public key credential and revokes the
// authenticated certificate atomically.
func (store *Store) RotateAgentCertificate(
	ctx context.Context,
	identity domain.AgentIdentity,
	certificate domain.AgentCertificate,
) error {
	_, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (struct{}, error) {
		commandTag, queryErr := tx.Exec(ctx, `
			INSERT INTO agent_certificates (
				serial, namespace_id, agent_id, public_key_digest, not_after
			) VALUES ($1, $2, $3, $4, $5)
		`, certificate.Serial, identity.NamespaceID, identity.AgentID,
			certificate.PublicKeyDigest, certificate.ExpiresAt)
		if queryErr != nil {
			return struct{}{}, fmt.Errorf("insert rotated agent certificate: %w", queryErr)
		}
		if commandTag.RowsAffected() != 1 {
			return struct{}{}, domain.ErrConflict
		}
		commandTag, queryErr = tx.Exec(ctx, `
			UPDATE agent_certificates
			SET revoked_at = transaction_timestamp(), replaced_by_serial = $3
			WHERE serial = $1 AND agent_id = $2 AND revoked_at IS NULL
				AND not_after > transaction_timestamp()
		`, identity.CertificateSerial, identity.AgentID, certificate.Serial)
		if queryErr != nil {
			return struct{}{}, fmt.Errorf("revoke replaced agent certificate: %w", queryErr)
		}
		if commandTag.RowsAffected() != 1 {
			return struct{}{}, domain.ErrUnauthenticated
		}

		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("rotate agent certificate: %w", err)
	}

	return nil
}

// AuthenticateAgentCertificate validates a CA-verified certificate against
// current database revocation, target, and agent state using database time.
func (store *Store) AuthenticateAgentCertificate(
	ctx context.Context,
	agentID string,
	serial string,
	publicKeyDigest string,
) (domain.AgentIdentity, error) {
	var identity domain.AgentIdentity
	err := store.pool.QueryRow(ctx, `
		UPDATE agents AS a
		SET updated_at = transaction_timestamp(), last_seen_at = transaction_timestamp()
		FROM agent_certificates AS c, targets AS t, namespaces AS n
		WHERE a.id = $1 AND c.serial = $2 AND c.public_key_digest = $3
			AND c.agent_id = a.id AND c.namespace_id = a.namespace_id
			AND c.revoked_at IS NULL AND c.not_after > transaction_timestamp()
			AND a.status IN ('active', 'draining') AND t.id = a.target_id
			AND t.state IN ('active', 'draining')
			AND n.id = a.namespace_id
		RETURNING a.id::text, a.namespace_id::text, n.name,
			a.principal_id::text, a.target_id::text,
			a.target_generation_id::text, c.serial
	`, agentID, serial, publicKeyDigest).Scan(
		&identity.AgentID, &identity.NamespaceID, &identity.Namespace,
		&identity.PrincipalID, &identity.TargetID,
		&identity.TargetGenerationID, &identity.CertificateSerial,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentIdentity{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.AgentIdentity{}, fmt.Errorf("authenticate agent certificate: %w", err)
	}

	return identity, nil
}

// ListAssignments redelivers inert offers and records delivery evidence.
func (store *Store) ListAssignments(
	ctx context.Context,
	identity domain.AgentIdentity,
	limit int,
) ([]domain.Assignment, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("assignment limit must be between 1 and 100")
	}
	rows, err := store.pool.Query(ctx, `
		WITH selected AS (
			SELECT assignment.id
			FROM assignments AS assignment
			JOIN agents AS selected_agent ON selected_agent.id = assignment.agent_id
			JOIN targets AS selected_target ON selected_target.id = selected_agent.target_id
			WHERE assignment.agent_id = $1 AND assignment.namespace_id = $2
				AND assignment.state = 'offered'
				AND selected_agent.status = 'active'
				AND selected_agent.accepting_assignments
				AND selected_agent.last_capability_at > transaction_timestamp() - interval '10 minutes'
				AND selected_target.state = 'active'
			ORDER BY assignment.created_at, assignment.id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE assignments AS a
		SET delivery_count = a.delivery_count + 1,
			last_delivered_at = transaction_timestamp()
		FROM selected AS s, executions AS e
		WHERE a.id = s.id AND e.id = a.execution_id
		RETURNING a.id::text, a.execution_id::text, e.effective_spec_digest,
			a.document::text, a.created_at
	`, identity.AgentID, identity.NamespaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list agent assignments: %w", err)
	}
	defer rows.Close()
	assignments := make([]domain.Assignment, 0)
	for rows.Next() {
		var assignment domain.Assignment
		var document string
		if err = rows.Scan(
			&assignment.DeliveryID, &assignment.ExecutionID,
			&assignment.EffectiveExecutionDigest, &document, &assignment.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent assignment: %w", err)
		}
		assignment.Document = json.RawMessage(document)
		assignment.CreatedAt = assignment.CreatedAt.UTC()
		assignments = append(assignments, assignment)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent assignments: %w", err)
	}

	return assignments, nil
}

type enrollmentBinding struct {
	enrollmentID     string
	namespaceID      string
	targetID         string
	generationID     string
	principalID      string
	expectedUser     string
	usedAgentID      string
	targetState      string
	executionBackend string
	operatingSystems []string
	architectures    []string
	expired          bool
}

func lookupEnrollmentForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	token string,
) (enrollmentBinding, error) {
	var binding enrollmentBinding
	var usedAgentID *string
	err := tx.QueryRow(ctx, `
		SELECT e.id::text, e.namespace_id::text, e.target_id::text,
			e.target_generation_id::text, e.principal_id::text,
			e.expected_user, e.used_by_agent_id::text, t.state,
			tg.execution_backend, tg.operating_systems, tg.architectures,
			e.expires_at <= transaction_timestamp()
		FROM agent_enrollment_tokens AS e
		JOIN targets AS t ON t.id = e.target_id
		JOIN target_generations AS tg ON tg.id = e.target_generation_id
		WHERE e.token_hash = $1
			AND t.current_generation_id = e.target_generation_id
		FOR UPDATE OF e
	`, hashToken(token)).Scan(
		&binding.enrollmentID, &binding.namespaceID, &binding.targetID,
		&binding.generationID, &binding.principalID, &binding.expectedUser,
		&usedAgentID, &binding.targetState, &binding.executionBackend,
		&binding.operatingSystems, &binding.architectures, &binding.expired,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return enrollmentBinding{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return enrollmentBinding{}, fmt.Errorf("read enrollment token: %w", err)
	}
	if usedAgentID != nil {
		binding.usedAgentID = *usedAgentID
	}

	return binding, nil
}

func (store *Store) replayEnrollment(
	ctx context.Context,
	tx pgx.Tx,
	binding enrollmentBinding,
	registrationDigest string,
) (domain.AgentSession, error) {
	var persistedDigest string
	var sessionID string
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT a.registration_digest, s.id::text, s.expires_at
		FROM agents AS a
		JOIN agent_sessions AS s ON s.agent_id = a.id
		WHERE a.id = $1 AND s.revoked_at IS NULL
			AND s.expires_at > transaction_timestamp()
		ORDER BY s.created_at DESC
		LIMIT 1
	`, binding.usedAgentID).Scan(&persistedDigest, &sessionID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentSession{}, domain.ErrConflict
	}
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("replay agent enrollment: %w", err)
	}
	if persistedDigest != registrationDigest {
		return domain.AgentSession{}, domain.ErrIdempotencyConflict
	}

	return domain.AgentSession{
		AgentID: binding.usedAgentID, SessionID: sessionID,
		Token: store.deriveToken("jms", sessionID), ExpiresAt: expiresAt.UTC(),
		Replayed: true,
	}, nil
}

func (store *Store) getEnrollmentToken(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	enrollmentID string,
) (domain.EnrollmentToken, error) {
	var token domain.EnrollmentToken
	var used bool
	err := tx.QueryRow(ctx, `
		SELECT e.id::text, n.name, t.name, e.target_generation_id::text,
			p.issuer, p.subject, e.expected_user, e.expires_at,
			e.used_at IS NOT NULL
		FROM agent_enrollment_tokens AS e
		JOIN namespaces AS n ON n.id = e.namespace_id
		JOIN targets AS t ON t.id = e.target_id
		JOIN principals AS p ON p.id = e.principal_id
		WHERE e.namespace_id = $1 AND e.id = $2
	`, namespaceID, enrollmentID).Scan(
		&token.ID, &token.Namespace, &token.Target, &token.TargetGenerationID,
		&token.Principal.Issuer, &token.Principal.Subject, &token.ExpectedUser,
		&token.ExpiresAt, &used,
	)
	if err != nil {
		return domain.EnrollmentToken{}, err
	}
	if !used {
		token.Token = store.deriveToken("jme", token.ID)
	}
	token.ExpiresAt = token.ExpiresAt.UTC()

	return token, nil
}

func auditEnrollment(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	enrollmentID string,
	generationID string,
	idempotencyKey string,
	requestDigest string,
	request domain.EnrollmentRequest,
) error {
	details, err := json.Marshal(map[string]string{
		"targetGenerationId": generationID,
		"principalIssuer":    request.Principal.Issuer,
		"principalSubject":   request.Principal.Subject,
		"expectedUser":       request.ExpectedUser,
	})
	if err != nil {
		return fmt.Errorf("encode enrollment audit details: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			namespace_id, actor_principal_id, action, resource_type,
			resource_id, request_digest, idempotency_key, details
		) VALUES ($1, $2, 'agent.enrollment-token.created',
			'agent_enrollment_token', $3, $4, $5, $6::jsonb)
	`, authorization.namespaceID, authorization.principalID, enrollmentID,
		requestDigest, idempotencyKey, string(details)); err != nil {
		return fmt.Errorf("audit enrollment token: %w", err)
	}

	return nil
}

func validSessionLifetime(value time.Duration) bool {
	return value >= time.Minute && value <= 24*time.Hour
}

func allowsValue(allowed []string, value string) bool {
	return len(allowed) == 0 || slices.Contains(allowed, value)
}
