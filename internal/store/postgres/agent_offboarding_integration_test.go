package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestAgentOffboardingIntegration(t *testing.T) {
	fixture := newRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	job, err := fixture.store.SubmitJob(ctx, fixture.principal, "offboard-active-job", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	if assigned, reconcileErr := fixture.store.ReconcileAssignments(ctx, 1); reconcileErr != nil || assigned != 1 {
		t.Fatalf("create synthetic assignment: %d, %v", assigned, reconcileErr)
	}
	assignments, err := fixture.store.ListAssignments(ctx, fixture.identity, 1)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("load synthetic assignment: %d, %v", len(assignments), err)
	}
	assignment := assignments[0]
	if _, err = fixture.store.AcceptAssignment(ctx, fixture.identity, integrationAcceptance(t, assignment, fixture.identity)); err != nil {
		t.Fatal(err)
	}
	start := integrationEvent(t, fixture.identity, assignment.ExecutionID, 1, "process.started", "4242", nil)
	if _, err = fixture.store.RecordExecutionEvent(ctx, fixture.identity, start); err != nil {
		t.Fatal(err)
	}
	next := fixture.request.Certificate
	wrong := fixture.identity
	wrong.TargetGenerationID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if err = fixture.store.RotateAgentCertificate(ctx, wrong, next); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("wrong-generation renewal was accepted: %v", err)
	}
	if err = fixture.store.RotateAgentCertificate(ctx, fixture.identity, next); err != nil {
		t.Fatal(err)
	}
	rotated, err := fixture.store.AuthenticateAgentCertificate(ctx, fixture.session.AgentID, next.Serial, next.PublicKeyDigest)
	if err != nil || rotated.AgentID != fixture.identity.AgentID ||
		rotated.TargetGenerationID != fixture.identity.TargetGenerationID {
		t.Fatalf("rotation changed the agent binding: %#v, %v", rotated, err)
	}

	const retire = "SELECT jobman_control_retire_agent($1, $2, 'offboarding-drill')"
	_, err = fixture.pool.Exec(ctx, retire, rotated.AgentID, wrong.TargetGenerationID)
	assertOperatorConflict(t, err)
	blocker, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, blocker)
	var changed bool
	if err = blocker.QueryRow(ctx, retire, rotated.AgentID, rotated.TargetGenerationID).Scan(&changed); err != nil || !changed {
		t.Fatalf("stage retirement: %v, %v", changed, err)
	}
	replicaConfig := fixture.pool.Config()
	replicaConfig.MaxConns = 1
	replica, err := pgxpool.NewWithConfig(ctx, replicaConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	connection, err := replica.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := connection.Conn().PgConn().PID()
	connection.Release()
	replicaStore := New(replica, []byte("0123456789abcdef0123456789abcdef"))
	checked := make(chan error, 1)
	go func() {
		_, authErr := replicaStore.AuthenticateAgentCertificate(ctx, rotated.AgentID, next.Serial, next.PublicKeyDigest)
		checked <- authErr
	}()
	waitVisibilityLock(ctx, t, fixture.pool, pid)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-checked; !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("waiting replica admitted retired certificate: %v", err)
	}
	if err = fixture.pool.QueryRow(ctx, retire, rotated.AgentID, rotated.TargetGenerationID).Scan(&changed); err != nil || changed {
		t.Fatalf("retirement replay: %v, %v", changed, err)
	}
	if _, err = replicaStore.AuthenticateAgent(ctx, fixture.session.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("retired bearer token was accepted: %v", err)
	}
	if _, err = replicaStore.EnrollAgent(ctx, fixture.originalToken, fixture.registration, 15*time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("retired enrollment replay was accepted: %v", err)
	}
	if _, err = replicaStore.RecoverAgent(ctx, fixture.token, fixture.request, 15*time.Minute); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("retired recovery was accepted: %v", err)
	}
	retained, err := fixture.store.GetJob(ctx, fixture.principal, "research", job.Job.ID)
	if err != nil || retained.Phase != "running" || retained.Outcome != "" {
		t.Fatalf("retirement changed running job result: %#v, %v", retained, err)
	}
	var executionID, agentID string
	var auditCount int
	err = fixture.pool.QueryRow(ctx, `
		SELECT e.id, e.agent_id,
		    (SELECT count(*) FROM service_operator_audit WHERE action='agent.retired' AND resource_id=$2)
		FROM executions e JOIN runs r ON r.id=e.run_id WHERE r.job_id=$1
	`, job.Job.ID, rotated.AgentID).Scan(&executionID, &agentID, &auditCount)
	if err != nil || executionID != assignment.ExecutionID || agentID != rotated.AgentID || auditCount != 1 {
		t.Fatalf("retained execution identity or audit changed: %v", err)
	}
}
