package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type recoveryFixture struct {
	store         *Store
	pool          *pgxpool.Pool
	principal     domain.Principal
	target        domain.Target
	originalToken string
	registration  domain.AgentRegistration
	session       domain.AgentSession
	identity      domain.AgentIdentity
	token         string
	request       domain.AgentRecovery
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	url := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	pool := newIntegrationPool(ctx, t, url)
	// Exercise the deployed migration22 -> migration23 path, not just fresh DBs.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, pool, migrations[:22]); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "recovery-test", Subject: "owner"}
	if err = store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "Recovery owner", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTarget(ctx, principal, "research", "recovery-target", recoveryDigest("1"), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}, LogStoreName: "test-nfs", LogStoreVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	makeToken := func(key string) string {
		t.Helper()
		value, tokenErr := store.CreateEnrollmentToken(ctx, principal, "research", "workstation-a", key, recoveryDigest("2"), domain.EnrollmentRequest{Principal: principal, ExpectedUser: "worker", Lifetime: 10 * time.Minute})
		if tokenErr != nil {
			t.Fatal(tokenErr)
		}
		return value.Token
	}
	originalToken := makeToken("original-enrollment")
	registration := domain.AgentRegistration{TargetGenerationID: target.Value.GenerationID, AgentVersion: "test", ProtocolVersions: []string{"jobman/v1alpha1"}, OperatingSystem: "linux", Architecture: "amd64", Hostname: "worker-host", ExecutionUser: "worker", ExecutionBackends: []string{"subprocess"}, Runtimes: []string{"native"}, RequestDigest: recoveryDigest("3")}
	session, err := store.EnrollAgent(ctx, originalToken, registration, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.AuthenticateAgent(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	identity.CertificateSerial = "1001"
	if err = store.RecordAgentCertificate(ctx, session.AgentID, domain.AgentCertificate{Serial: "1001", PublicKeyDigest: recoveryDigest("a"), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	request := domain.AgentRecovery{AgentID: session.AgentID, TargetGenerationID: target.Value.GenerationID, PreviousCertificateSerial: "1001", ExecutionUser: "worker", RequestDigest: recoveryDigest("4"), Certificate: domain.AgentCertificate{Serial: "1002", PublicKeyDigest: recoveryDigest("b"), ExpiresAt: time.Now().Add(time.Hour).UTC()}, CertificatePEM: "synthetic-public-certificate", CACertificatePEM: "synthetic-public-ca"}
	return recoveryFixture{store: store, pool: pool, principal: principal, target: target.Value, originalToken: originalToken, registration: registration, session: session, identity: identity, token: makeToken("recovery-enrollment"), request: request}
}
func recoveryDigest(value string) string { return "sha256:" + strings.Repeat(value, 64) }
func (f recoveryFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryFixture) expire(t *testing.T) {
	t.Helper()
	f.exec(t, `UPDATE agent_certificates SET not_after=clock_timestamp()-interval '1 second' WHERE serial='1001'`)
	f.exec(t, `UPDATE agent_sessions SET expires_at=clock_timestamp()-interval '1 second'`)
}

func TestAgentSessionRecoveryIntegration(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := t.Context()
	f.exec(t, `UPDATE agent_sessions SET expires_at=clock_timestamp()-interval '1 second'`)
	if _, err := f.store.RenewAgentSession(ctx, f.session.Token, 15*time.Minute); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("expired bearer renewal: %v", err)
	}
	wrong := f.identity
	wrong.AgentID = "11111111-1111-4111-8111-111111111111"
	if _, err := f.store.RenewAgentSessionWithCertificate(ctx, wrong, f.session.Token, 15*time.Minute); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("wrong agent renewal: %v", err)
	}
	renewed, err := f.store.RenewAgentSessionWithCertificate(ctx, f.identity, f.session.Token, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.store.RenewAgentSessionWithCertificate(ctx, f.identity, f.session.Token, 15*time.Minute)
	if err != nil || !replay.Replayed || replay.Token != renewed.Token || !replay.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("direct replay mismatch: %v", err)
	}
	next, err := f.store.RenewAgentSessionWithCertificate(ctx, f.identity, renewed.Token, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.RenewAgentSessionWithCertificate(ctx, f.identity, f.session.Token, 15*time.Minute); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("transitive replay permitted: %v", err)
	}
	f.exec(t, `UPDATE agent_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, next.SessionID)
	if _, err = f.store.RenewAgentSessionWithCertificate(ctx, f.identity, next.Token, 15*time.Minute); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("explicit revocation bypass: %v", err)
	}
}

func TestAgentSessionRecoveryAuthorizationIntegration(t *testing.T) {
	for _, query := range []string{`UPDATE agent_certificates SET revoked_at=clock_timestamp()`, `UPDATE agent_certificates SET not_after=clock_timestamp()-interval '1 second'`, `UPDATE agents SET status='disabled'`, `UPDATE targets SET state='disabled'`} {
		t.Run(query, func(t *testing.T) {
			f := newRecoveryFixture(t)
			f.exec(t, query)
			if _, err := f.store.RenewAgentSessionWithCertificate(t.Context(), f.identity, f.session.Token, 15*time.Minute); !errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("authorization bypass: %v", err)
			}
		})
	}
}

func TestAgentRecoveryReplayAndIdentityIntegration(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := t.Context()
	job, err := f.store.SubmitJob(ctx, f.principal, "pending-recovery-job", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ReconcileAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	assignments, err := f.store.ListAssignments(ctx, f.identity, 10)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("pending assignment setup: count%d error%v", len(assignments), err)
	}
	f.expire(t)
	// Durable work references and registration remain unchanged, including any
	// journal entries represented by this stable identity.
	var before string
	if err = f.pool.QueryRow(ctx, `SELECT row_to_json(a)::text FROM agents a WHERE id=$1`, f.session.AgentID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	results := make([]domain.AgentRecoveryResult, 2)
	errs := make([]error, 2)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			results[i], errs[i] = f.store.RecoverAgent(ctx, f.token, f.request, 15*time.Minute)
		}()
	}
	group.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if results[0].Session.Token != results[1].Session.Token || results[0].CertificatePEM != results[1].CertificatePEM || !results[0].Certificate.ExpiresAt.Equal(results[1].Certificate.ExpiresAt) || results[0].Session.Replayed == results[1].Session.Replayed {
		t.Fatal("concurrent recovery was not an exact replay")
	}
	result := results[0]
	if result.Session.AgentID != f.session.AgentID {
		t.Fatal("agent identity changed")
	}
	var after string
	if err = f.pool.QueryRow(ctx, `SELECT row_to_json(a)::text FROM agents a WHERE id=$1`, f.session.AgentID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("stored registration changed")
	}
	if _, err = f.store.EnrollAgent(ctx, f.originalToken, f.registration, 15*time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("old enrollment authority revived: %v", err)
	}
	if err = f.store.RecordAgentCertificate(ctx, f.session.AgentID, domain.AgentCertificate{Serial: "1003", PublicKeyDigest: recoveryDigest("a"), ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("in-flight old enrollment certificate accepted: %v", err)
	}
	if _, err = f.store.AuthenticateAgentCertificate(ctx, f.session.AgentID, "1001", recoveryDigest("a")); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("old certificate accepted: %v", err)
	}
	if _, err = f.store.AuthenticateAgentCertificate(ctx, f.session.AgentID, "1002", recoveryDigest("b")); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.AuthenticateAgent(ctx, result.Session.Token); err != nil {
		t.Fatal(err)
	}
	redelivered, err := f.store.ListAssignments(ctx, f.identity, 10)
	if err != nil || len(redelivered) != 1 || redelivered[0].DeliveryID != assignments[0].DeliveryID || redelivered[0].ExecutionID != assignments[0].ExecutionID {
		t.Fatalf("pending assignment lost during recovery: %v", err)
	}
	retained, err := f.store.GetJob(ctx, f.principal, "research", job.Job.ID)
	if err != nil || retained.ID != job.Job.ID {
		t.Fatalf("job identity lost during recovery: %v", err)
	}
	changed := f.request
	changed.RequestDigest = recoveryDigest("5")
	if _, err = f.store.RecoverAgent(ctx, f.token, changed, 15*time.Minute); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("changed recovery replay: %v", err)
	}
	f.exec(t, `UPDATE agent_certificates SET revoked_at=clock_timestamp() WHERE serial='1002'`)
	if _, err = f.store.RecoverAgent(ctx, f.token, f.request, 15*time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("revoked replay accepted: %v", err)
	}
}

func TestAgentRecoveryDenialsIntegration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, recoveryFixture) domain.AgentRecovery
		want   error
	}{
		{"wrong user", func(_ *testing.T, f recoveryFixture) domain.AgentRecovery {
			f.request.ExecutionUser = "other"
			return f.request
		}, domain.ErrForbidden},
		{"wrong agent", func(_ *testing.T, f recoveryFixture) domain.AgentRecovery {
			f.request.AgentID = "11111111-1111-4111-8111-111111111111"
			return f.request
		}, domain.ErrForbidden},
		{"wrong principal", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			other := domain.Principal{Issuer: f.principal.Issuer, Subject: "other-owner"}
			if err := f.store.EnsureDevelopmentIdentity(t.Context(), domain.DevelopmentIdentity{Principal: other, DisplayName: "Other owner", Namespace: "research"}); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE agents SET principal_id=(SELECT id FROM principals WHERE issuer=$1 AND subject=$2) WHERE id=$3`, other.Issuer, other.Subject, f.session.AgentID)
			return f.request
		}, domain.ErrForbidden},
		{"generation rolled over", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			_, err := f.store.CreateTargetGeneration(t.Context(), f.principal, "research", f.target.Name, "rollover-recovery", recoveryDigest("6"), domain.TargetGenerationChange{ExpectedRevision: f.target.Revision, Spec: domain.TargetSpec{Name: f.target.Name, Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}, LogStoreName: "test-nfs", LogStoreVersion: 2}})
			if err != nil {
				t.Fatal(err)
			}
			return f.request
		}, domain.ErrUnauthenticated},
		{"wrong generation", func(_ *testing.T, f recoveryFixture) domain.AgentRecovery {
			f.request.TargetGenerationID = "11111111-1111-4111-8111-111111111111"
			return f.request
		}, domain.ErrForbidden},
		{"revoked prior certificate", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			f.exec(t, `UPDATE agent_certificates SET revoked_at=clock_timestamp()`)
			return f.request
		}, domain.ErrUnauthenticated},
		{"unexpired prior certificate", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			f.exec(t, `UPDATE agent_certificates SET not_after=clock_timestamp()+interval '1 hour'`)
			return f.request
		}, domain.ErrUnauthenticated},
		{"disabled agent", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			f.exec(t, `UPDATE agents SET status='disabled'`)
			return f.request
		}, domain.ErrForbidden},
		{"disabled target", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			f.exec(t, `UPDATE targets SET state='disabled'`)
			return f.request
		}, domain.ErrForbidden},
		{"expired enrollment", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			f.exec(t, `UPDATE agent_enrollment_tokens SET expires_at=clock_timestamp()-interval '1 second'`)
			return f.request
		}, domain.ErrConflict},
		{"revoked membership", func(t *testing.T, f recoveryFixture) domain.AgentRecovery {
			t.Helper()
			f.exec(t, `UPDATE membership_grants SET revoked_at=clock_timestamp()`)
			return f.request
		}, domain.ErrForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			f.expire(t)
			request := test.mutate(t, f)
			if _, err := f.store.RecoverAgent(t.Context(), f.token, request, 15*time.Minute); !errors.Is(err, test.want) {
				t.Fatalf("recovery error %v, want %v", err, test.want)
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_recoveries`).Scan(&count); err != nil || count != 0 {
				t.Fatal("denied recovery mutated ledger")
			}
		})
	}
}

func TestAgentRecoveryRechecksAuthorizationAfterLockIntegration(t *testing.T) {
	f := newRecoveryFixture(t)
	f.expire(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, blocker)
	if _, err = blocker.Exec(ctx, `SELECT id FROM agents WHERE id=$1 FOR UPDATE`, f.session.AgentID); err != nil {
		t.Fatal(err)
	}
	config := f.pool.Config()
	config.MaxConns = 1
	recoveryPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryPool.Close()
	connection, err := recoveryPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := connection.Conn().PgConn().PID()
	connection.Release()
	recoveryStore := New(recoveryPool, []byte("0123456789abcdef0123456789abcdef"))
	completed := make(chan error, 1)
	go func() {
		_, recoverErr := recoveryStore.RecoverAgent(ctx, f.token, f.request, 15*time.Minute)
		completed <- recoverErr
	}()
	waitVisibilityLock(ctx, t, f.pool, pid)
	f.exec(t, `UPDATE membership_grants SET revoked_at=clock_timestamp()`)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-completed; !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("membership revocation during wait bypassed: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_certificates WHERE serial='1002'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("denied recovery did not roll back credential installation")
	}
}
