package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// RenewAgentSessionWithCertificate can restore an expired inert session only
// under a currently valid certificate for that same agent. A lost response may
// replay its direct replacement; it never extends that replacement's lifetime.
func (store *Store) RenewAgentSessionWithCertificate(ctx context.Context, identity domain.AgentIdentity, token string, lifetime time.Duration) (domain.AgentSession, error) {
	if !validSessionLifetime(lifetime) {
		return domain.AgentSession{}, errors.New("agent session lifetime is invalid")
	}
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.AgentSession, error) {
		var namespaceID string
		err := tx.QueryRow(ctx, `SELECT a.namespace_id::text FROM agents a
		 JOIN targets t ON t.id=a.target_id JOIN agent_certificates c ON c.agent_id=a.id AND c.namespace_id=a.namespace_id
		 WHERE a.id=$1 AND a.namespace_id=$2 AND a.target_id=$3 AND a.target_generation_id=$4
		 AND c.serial=$5 AND c.revoked_at IS NULL AND c.not_after>clock_timestamp()
		 AND a.status IN ('active','draining') AND t.state IN ('active','draining')
		 FOR UPDATE OF a,t,c`, identity.AgentID, identity.NamespaceID, identity.TargetID, identity.TargetGenerationID, identity.CertificateSerial).Scan(&namespaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AgentSession{}, domain.ErrUnauthenticated
		}
		if err != nil {
			return domain.AgentSession{}, fmt.Errorf("authorize session certificate: %w", err)
		}
		var sessionID string
		var revoked bool
		var replacement *string
		err = tx.QueryRow(ctx, `SELECT id::text, revoked_at IS NOT NULL,replaced_by_session_id::text FROM agent_sessions
		 WHERE token_hash=$1 AND agent_id=$2 AND namespace_id=$3 FOR UPDATE`, hashToken(token), identity.AgentID, namespaceID).Scan(&sessionID, &revoked, &replacement)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AgentSession{}, domain.ErrUnauthenticated
		}
		if err != nil {
			return domain.AgentSession{}, fmt.Errorf("read session for certificate renewal: %w", err)
		}
		var certificateCurrent bool
		if err = tx.QueryRow(ctx, `SELECT not_after>clock_timestamp() FROM agent_certificates WHERE serial=$1`, identity.CertificateSerial).Scan(&certificateCurrent); err != nil {
			return domain.AgentSession{}, fmt.Errorf("recheck session certificate expiry: %w", err)
		}
		if !certificateCurrent {
			return domain.AgentSession{}, domain.ErrUnauthenticated
		}
		if revoked {
			if replacement == nil {
				return domain.AgentSession{}, domain.ErrUnauthenticated
			}
			var expiry time.Time
			err = tx.QueryRow(ctx, `SELECT expires_at FROM agent_sessions WHERE id=$1 AND agent_id=$2 AND namespace_id=$3 AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, *replacement, identity.AgentID, namespaceID).Scan(&expiry)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.AgentSession{}, domain.ErrUnauthenticated
			}
			if err != nil {
				return domain.AgentSession{}, fmt.Errorf("replay renewed session: %w", err)
			}
			if currentErr := checkRecoveryCredentialExpiry(ctx, tx, identity.CertificateSerial, *replacement); currentErr != nil {
				return domain.AgentSession{}, currentErr
			}
			return domain.AgentSession{AgentID: identity.AgentID, SessionID: *replacement, Token: store.deriveToken("jms", *replacement), ExpiresAt: expiry.UTC(), Replayed: true}, nil
		}
		result, err := store.insertRecoverySession(ctx, tx, namespaceID, identity.AgentID, lifetime)
		if err != nil {
			return domain.AgentSession{}, err
		}
		_, err = tx.Exec(ctx, `UPDATE agent_sessions SET revoked_at=clock_timestamp(),replaced_by_session_id=$2 WHERE id=$1`, sessionID, result.SessionID)
		if err != nil {
			return domain.AgentSession{}, fmt.Errorf("replace expired session: %w", err)
		}
		return result, nil
	})
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("renew certificate-bound session: %w", err)
	}
	return result, nil
}

func (store *Store) insertRecoverySession(ctx context.Context, tx pgx.Tx, namespaceID, agentID string, lifetime time.Duration) (domain.AgentSession, error) {
	id, err := store.newID()
	if err != nil {
		return domain.AgentSession{}, err
	}
	token := store.deriveToken("jms", id)
	var expiry time.Time
	err = tx.QueryRow(ctx, `INSERT INTO agent_sessions(id,namespace_id,agent_id,token_hash,expires_at) VALUES($1,$2,$3,$4,clock_timestamp()+$5::interval) RETURNING expires_at`, id, namespaceID, agentID, hashToken(token), lifetime.String()).Scan(&expiry)
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("insert recovery session: %w", err)
	}
	return domain.AgentSession{AgentID: agentID, SessionID: id, Token: token, ExpiresAt: expiry.UTC()}, nil
}

// RecoverAgent changes credentials, never the enrolled identity or registration.
// Fresh enrollment authority is the only authority for an expired certificate.
func (store *Store) RecoverAgent(ctx context.Context, token string, request domain.AgentRecovery, lifetime time.Duration) (domain.AgentRecoveryResult, error) {
	if !validSessionLifetime(lifetime) || !domain.IsID(request.AgentID) || !domain.IsID(request.TargetGenerationID) || len(request.ExecutionUser) < 1 || len(request.ExecutionUser) > 512 || len(request.PreviousCertificateSerial) < 1 || len(request.PreviousCertificateSerial) > 64 || len(request.CertificatePEM) < 1 || len(request.CertificatePEM) > 65536 || len(request.CACertificatePEM) < 1 || len(request.CACertificatePEM) > 65536 {
		return domain.AgentRecoveryResult{}, domain.ErrConflict
	}
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.AgentRecoveryResult, error) {
		binding, err := lookupEnrollmentForUpdate(ctx, tx, token)
		if err != nil {
			return domain.AgentRecoveryResult{}, err
		}
		if binding.targetState != "active" || binding.generationID != request.TargetGenerationID || binding.expectedUser != request.ExecutionUser {
			return domain.AgentRecoveryResult{}, domain.ErrForbidden
		}
		var agentID string
		err = tx.QueryRow(ctx, `SELECT a.id::text FROM agents a JOIN targets t ON t.id=a.target_id
		 WHERE a.id=$1 AND a.namespace_id=$2 AND a.principal_id=$3 AND a.target_id=$4 AND a.target_generation_id=$5
		 AND a.execution_user=$6 AND a.status='active' AND t.state='active' AND t.current_generation_id=a.target_generation_id FOR UPDATE OF a,t`, request.AgentID, binding.namespaceID, binding.principalID, binding.targetID, binding.generationID, request.ExecutionUser).Scan(&agentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AgentRecoveryResult{}, domain.ErrForbidden
		}
		if err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("authorize recovery agent: %w", err)
		}
		if binding.usedAgentID != "" {
			return store.replayAgentRecovery(ctx, tx, binding, request)
		}
		if binding.expired {
			return domain.AgentRecoveryResult{}, domain.ErrConflict
		}
		var serial string
		err = tx.QueryRow(ctx, `SELECT serial FROM agent_certificates WHERE serial=$1 AND agent_id=$2 AND namespace_id=$3 AND revoked_at IS NULL AND not_after<=clock_timestamp() FOR UPDATE`, request.PreviousCertificateSerial, agentID, binding.namespaceID).Scan(&serial)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AgentRecoveryResult{}, domain.ErrUnauthenticated
		}
		if err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("check expired recovery certificate: %w", err)
		}
		var installed string
		certificate := request.Certificate
		err = tx.QueryRow(ctx, `INSERT INTO agent_certificates(serial,namespace_id,agent_id,public_key_digest,not_after)
		 SELECT $1,$2,$3,$4,$5 WHERE $5>clock_timestamp() RETURNING serial,not_after`, request.Certificate.Serial, binding.namespaceID, agentID, request.Certificate.PublicKeyDigest, request.Certificate.ExpiresAt).Scan(&installed, &certificate.ExpiresAt)
		if err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("install recovery certificate: %w", err)
		}
		certificate.ExpiresAt = certificate.ExpiresAt.UTC()
		session, err := store.insertRecoverySession(ctx, tx, binding.namespaceID, agentID, lifetime)
		if err != nil {
			return domain.AgentRecoveryResult{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE agent_sessions SET revoked_at=clock_timestamp(),replaced_by_session_id=$2 WHERE agent_id=$1 AND id<>$2 AND revoked_at IS NULL`, agentID, session.SessionID); err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("revoke prior recovery sessions: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE agent_certificates SET revoked_at=clock_timestamp(),replaced_by_serial=$2 WHERE agent_id=$1 AND serial<>$2 AND revoked_at IS NULL`, agentID, installed); err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("revoke prior recovery certificates: %w", err)
		}
		// Recheck after every credential row lock; a blocked mutation must not
		// retain permissions or directory freshness observed before that wait.
		if authorizeErr := authorizeRecoveryEnrollment(ctx, tx, binding); authorizeErr != nil {
			return domain.AgentRecoveryResult{}, authorizeErr
		}
		consumed, err := tx.Exec(ctx, `UPDATE agent_enrollment_tokens SET used_at=clock_timestamp(),used_by_agent_id=$2 WHERE id=$1 AND used_at IS NULL AND expires_at>clock_timestamp()`, binding.enrollmentID, agentID)
		if err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("consume recovery enrollment: %w", err)
		}
		if consumed.RowsAffected() != 1 {
			return domain.AgentRecoveryResult{}, domain.ErrConflict
		}
		if _, err = tx.Exec(ctx, `INSERT INTO agent_recoveries(enrollment_id,namespace_id,agent_id,request_digest,session_id,certificate_serial,certificate_pem,ca_certificate_pem) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, binding.enrollmentID, binding.namespaceID, agentID, request.RequestDigest, session.SessionID, installed, request.CertificatePEM, request.CACertificatePEM); err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("record recovery replay: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(namespace_id,actor_principal_id,action,resource_type,resource_id,request_digest,details)
		 VALUES($1,$2,'agent.recovered','agent',$3,$4,jsonb_build_object('targetGenerationId',$5::text))`, binding.namespaceID, binding.principalID, agentID, request.RequestDigest, binding.generationID); err != nil {
			return domain.AgentRecoveryResult{}, fmt.Errorf("audit agent recovery: %w", err)
		}
		return domain.AgentRecoveryResult{Session: session, Certificate: certificate, CertificatePEM: request.CertificatePEM, CACertificatePEM: request.CACertificatePEM}, nil
	})
	if err != nil {
		return domain.AgentRecoveryResult{}, fmt.Errorf("recover enrolled agent: %w", err)
	}
	return result, nil
}

func authorizeRecoveryEnrollment(ctx context.Context, tx pgx.Tx, binding enrollmentBinding) error {
	var creator, recipient domain.Principal
	var namespace string
	err := tx.QueryRow(ctx, `SELECT p.issuer,p.subject,r.issuer,r.subject,n.name FROM agent_enrollment_tokens e
	 JOIN principals p ON p.id=e.created_by_principal_id JOIN principals r ON r.id=e.principal_id JOIN namespaces n ON n.id=e.namespace_id WHERE e.id=$1`, binding.enrollmentID).Scan(&creator.Issuer, &creator.Subject, &recipient.Issuer, &recipient.Subject, &namespace)
	if err != nil {
		return fmt.Errorf("read recovery authorization: %w", err)
	}
	authorization, err := authorizeNamespace(ctx, tx, creator, namespace, domain.CapabilityEnrollmentCreateOwn)
	if err != nil {
		return err
	}
	if !authorization.permits(domain.CapabilityEnrollmentCreateAny) && authorization.principalID != binding.principalID {
		return domain.ErrForbidden
	}
	_, err = authorizeNamespace(ctx, tx, recipient, namespace, domain.CapabilityNamespaceRead)
	return err
}

func (store *Store) replayAgentRecovery(ctx context.Context, tx pgx.Tx, binding enrollmentBinding, request domain.AgentRecovery) (domain.AgentRecoveryResult, error) {
	var result domain.AgentRecoveryResult
	var digest string
	err := tx.QueryRow(ctx, `SELECT r.request_digest,s.id::text,s.expires_at,c.serial,c.public_key_digest,c.not_after,r.certificate_pem,r.ca_certificate_pem
	 FROM agent_recoveries r JOIN agent_sessions s ON s.id=r.session_id AND s.agent_id=r.agent_id
	 JOIN agent_certificates c ON c.serial=r.certificate_serial AND c.agent_id=r.agent_id
	 WHERE r.enrollment_id=$1 AND r.agent_id=$2 AND r.namespace_id=$3 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
	 AND c.revoked_at IS NULL AND c.not_after>clock_timestamp() FOR UPDATE OF s,c`, binding.enrollmentID, request.AgentID, binding.namespaceID).Scan(&digest, &result.Session.SessionID, &result.Session.ExpiresAt, &result.Certificate.Serial, &result.Certificate.PublicKeyDigest, &result.Certificate.ExpiresAt, &result.CertificatePEM, &result.CACertificatePEM)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentRecoveryResult{}, domain.ErrConflict
	}
	if err != nil {
		return domain.AgentRecoveryResult{}, fmt.Errorf("replay agent recovery: %w", err)
	}
	if digest != request.RequestDigest || binding.usedAgentID != request.AgentID {
		return domain.AgentRecoveryResult{}, domain.ErrIdempotencyConflict
	}
	if authorizeErr := authorizeRecoveryEnrollment(ctx, tx, binding); authorizeErr != nil {
		return domain.AgentRecoveryResult{}, authorizeErr
	}
	if currentErr := checkRecoveryCredentialExpiry(ctx, tx, result.Certificate.Serial, result.Session.SessionID); currentErr != nil {
		if errors.Is(currentErr, domain.ErrUnauthenticated) {
			return domain.AgentRecoveryResult{}, domain.ErrConflict
		}
		return domain.AgentRecoveryResult{}, currentErr
	}
	result.Session.AgentID = request.AgentID
	result.Session.Token = store.deriveToken("jms", result.Session.SessionID)
	result.Session.ExpiresAt = result.Session.ExpiresAt.UTC()
	result.Certificate.ExpiresAt = result.Certificate.ExpiresAt.UTC()
	result.Session.Replayed = true
	return result, nil
}

// Call after acquiring both credential row locks: predicates evaluated before
// a lock wait do not prove that a time boundary remains in the future.
func checkRecoveryCredentialExpiry(ctx context.Context, tx pgx.Tx, serial, sessionID string) error {
	var current bool
	err := tx.QueryRow(ctx, `SELECT c.not_after>clock_timestamp() AND s.expires_at>clock_timestamp()
 FROM agent_certificates c JOIN agent_sessions s ON s.agent_id=c.agent_id AND s.namespace_id=c.namespace_id
 WHERE c.serial=$1 AND s.id=$2 AND c.revoked_at IS NULL AND s.revoked_at IS NULL`, serial, sessionID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("recheck recovery credential expiry: %w", err)
	}
	if !current {
		return domain.ErrUnauthenticated
	}
	return nil
}
