package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type monitoringFixture struct {
	store                         *Store
	pool                          *pgxpool.Pool
	owner, service                domain.Principal
	namespaceID, otherNamespaceID string
	key                           domain.DelegationKey
}

func newMonitoringFixture(ctx context.Context, t *testing.T) monitoringFixture {
	t.Helper()
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	owner := domain.Principal{Issuer: "monitoring-test", Subject: "owner"}
	for _, namespace := range []string{"research", "other"} {
		if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: owner, DisplayName: "private owner name", Namespace: namespace}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateTarget(ctx, owner, namespace, "monitoring-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}}); err != nil {
			t.Fatal(err)
		}
	}
	var namespaceID, otherID string
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT (SELECT id::text FROM namespaces WHERE name='research'),(SELECT id::text FROM namespaces WHERE name='other'),statement_timestamp()`).Scan(&namespaceID, &otherID, &now); err != nil {
		t.Fatal(err)
	}
	fingerprint := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	key := domain.DelegationKey{ServiceID: "dashboard-feed", KeyID: "feed-1", Audience: "monitoring-test", PublicKey: make([]byte, 32), CertificateThumbprints: []string{fingerprint}, NamespaceIDs: []string{namespaceID, otherID}, Operations: []string{domain.CapabilityEventsRead}, Enabled: true}
	if err := store.RegisterDelegationKeys(ctx, []domain.DelegationKey{key}); err != nil {
		t.Fatal(err)
	}
	service := domain.Principal{Delegation: &domain.DelegatedActor{ServiceOnly: true, ServiceID: key.ServiceID, KeyID: key.KeyID, Audience: key.Audience, CertificateThumbprint: fingerprint, Operation: domain.CapabilityEventsRead, NamespaceIDs: []string{namespaceID}, Mode: "worker", AssertionID: base64.RawURLEncoding.EncodeToString(make([]byte, 24)), AssertionDigest: fingerprint, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}}
	if err := store.AcceptDelegationAssertion(ctx, service); err != nil {
		t.Fatal(err)
	}
	return monitoringFixture{store: store, pool: pool, owner: owner, service: service, namespaceID: namespaceID, otherNamespaceID: otherID, key: key}
}

func (f monitoringFixture) submit(ctx context.Context, t *testing.T, namespace string, index int) domain.Job {
	t.Helper()
	submission := integrationSubmission(t)
	submission.Namespace = namespace
	submission.Name = fmt.Sprintf("private-job-%d", index)
	submission.RequestDigest = fmt.Sprintf("sha256:%064x", index+100)
	result, err := f.store.SubmitJob(ctx, f.owner, fmt.Sprintf("feed-%d", index), submission)
	if err != nil {
		t.Fatal(err)
	}
	return result.Job
}

func (f monitoringFixture) terminal(ctx context.Context, t *testing.T, job domain.Job) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE jobs SET phase='terminal',outcome='failure',revision=revision+1 WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
}

func (f monitoringFixture) checkpoint(ctx context.Context, t *testing.T) domain.MonitoringCheckpoint {
	t.Helper()
	result, err := f.store.MonitoringCheckpoint(ctx, f.service)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMonitoringFeedPublicationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	initial := f.checkpoint(ctx, t)
	if initial.BacklogCount != 0 || initial.AsOf.IsZero() || initial.HeadCursor != initial.OldestCursor || initial.RetentionSeconds != 2592000 {
		t.Fatalf("checkpoint=%#v", initial)
	}
	a, b, c := f.submit(ctx, t, "research", 1), f.submit(ctx, t, "research", 2), f.submit(ctx, t, "research", 3)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			t.Error(rollbackErr)
		}
	}()
	if _, err = tx.Exec(ctx, `UPDATE jobs SET phase='terminal',outcome='success',revision=revision+1 WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if n, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || n != 0 {
		t.Fatalf("uncommitted transition=%d,%v", n, publishErr)
	}
	if _, err = f.store.CancelJob(ctx, f.owner, "research", b.ID, "feed-cancel", "sha256:"+strings.Repeat("2", 64)); err != nil {
		t.Fatal(err)
	}
	before := f.checkpoint(ctx, t)
	if before.BacklogCount != 1 || before.OldestUnpublishedRecordedAt == nil || before.HeadCursor != initial.HeadCursor {
		t.Fatalf("unpublished boundary=%#v", before)
	}
	if n, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || n != 1 {
		t.Fatalf("publication=%d,%v", n, publishErr)
	}
	first, err := f.store.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].JobID != b.ID || first.Items[0].Outcome != cancelledOutcome || first.Items[0].RunID != "" || first.HasMore {
		t.Fatalf("first=%#v,%v", first, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || n != 1 {
		t.Fatalf("late commit=%d,%v", n, publishErr)
	}
	second, err := f.store.ReadMonitoringEvents(ctx, f.service, first.NextCursor, 2)
	if err != nil || len(second.Items) != 1 || second.Items[0].JobID != a.ID || second.Items[0].Position <= first.Items[0].Position {
		t.Fatalf("delayed transaction lost=%#v,%v", second, err)
	}
	replay, err := New(f.pool, f.store.tokenKey).ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 10)
	if err != nil || len(replay.Items) != 2 || replay.Items[0].EventID != first.Items[0].EventID {
		t.Fatal("restart changed replay identity")
	}
	encoded, encodeErr := json.Marshal(replay)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if strings.Contains(string(encoded), "private-job") || strings.Contains(string(encoded), "private owner") || strings.Contains(string(encoded), "requestDocument") {
		t.Fatal("private display or workload data in feed")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE jobs SET phase='terminal',revision=revision+1,labels='{"private":"value"}' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if n, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || n != 0 {
		t.Fatal("terminal metadata update emitted a transition")
	}
	f.terminal(ctx, t, c)
	if _, err = f.pool.Exec(ctx, `CREATE FUNCTION reject_monitoring_append() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic publication failure'; END; $$; CREATE TRIGGER reject_append BEFORE INSERT ON monitoring_feed FOR EACH ROW EXECUTE FUNCTION reject_monitoring_append()`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PublishMonitoringEvents(ctx, 10); err == nil {
		t.Fatal("publication fault was not exercised")
	}
	failed := f.checkpoint(ctx, t)
	if failed.HeadCursor != second.HeadCursor || failed.BacklogCount != 1 {
		t.Fatal("failed append advanced acknowledgement or counter")
	}
	if _, err = f.pool.Exec(ctx, `DROP TRIGGER reject_append ON monitoring_feed; DROP FUNCTION reject_monitoring_append()`); err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 30; i++ {
		f.terminal(ctx, t, f.submit(ctx, t, "research", i))
	}
	var wait sync.WaitGroup
	failures := make(chan error, 4)
	for range 4 {
		wait.Go(func() {
			for {
				n, publishErr := f.store.PublishMonitoringEvents(ctx, 3)
				if publishErr != nil {
					failures <- publishErr
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wait.Wait()
	close(failures)
	for publishErr := range failures {
		t.Fatal(publishErr)
	}
	all, err := f.store.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 200)
	if err != nil || len(all.Items) != 23 || all.HasMore {
		t.Fatalf("concurrent feed=%d,%v", len(all.Items), err)
	}
	for index, event := range all.Items {
		if event.Position != int64(index+1) || event.OwnerPrincipalID != a.Owner.ID || event.RecordedAt.IsZero() || event.NewPhase != "terminal" {
			t.Fatalf("ordered factual event=%#v", event)
		}
	}
	// Simulate a lost outbox acknowledgement while the durable event is retained.
	if _, err = f.pool.Exec(ctx, `UPDATE outbox SET published_at=NULL WHERE id=$1`, first.Items[0].EventID); err != nil {
		t.Fatal(err)
	}
	if n, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || n != 1 {
		t.Fatal(publishErr)
	}
	if f.checkpoint(ctx, t).HeadCursor != all.HeadCursor {
		t.Fatal("replayed outbox allocated a duplicate position")
	}
	// A large unrelated namespace does not prevent checkpoint advancement.
	hidden := f.submit(ctx, t, "other", 100)
	f.terminal(ctx, t, hidden)
	if _, err = f.store.PublishMonitoringEvents(ctx, 10); err != nil {
		t.Fatal(err)
	}
	empty, err := f.store.ReadMonitoringEvents(ctx, f.service, all.NextCursor, 1)
	if err != nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor == all.NextCursor || empty.NextCursor != empty.HeadCursor {
		t.Fatalf("filtered advancement=%#v,%v", empty, err)
	}
	// Completed-history inserts intentionally produce no historical alert flood.
	if _, err = f.pool.Exec(ctx, `INSERT INTO jobs(id,namespace_id,owner_principal_id,name,phase,desired_state,placement_target,workload_digest,request_digest,request_document,outcome,imported,import_source,completed_at) SELECT gen_random_uuid(),namespace_id,owner_principal_id,'imported','terminal','run',placement_target,workload_digest,request_digest,request_document,'success',true,'{"store":"test","jobId":"old"}',statement_timestamp() FROM jobs WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if n, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || n != 0 {
		t.Fatal("history insert emitted monitoring event")
	}
}

func TestMonitoringFeedRetentionRecoveryAndAuthorityIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	initial := f.checkpoint(ctx, t)
	job := f.submit(ctx, t, "research", 1)
	f.terminal(ctx, t, job)
	if _, err := f.store.PublishMonitoringEvents(ctx, 10); err != nil {
		t.Fatal(err)
	}
	original, err := f.store.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.MonitoringCheckpoint(ctx, f.owner); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("ordinary user accessed service feed")
	}
	if err = f.store.AcceptDelegationAssertion(ctx, f.service); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("service assertion replay accepted")
	}
	if _, err = f.store.GetJob(ctx, f.service, "research", job.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("service-only principal read user detail")
	}
	if _, err = f.store.CurrentPrincipal(ctx, f.service, "", 10); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("service-only principal read user discovery")
	}
	if _, err = f.store.CancelJob(ctx, f.service, "research", job.ID, "feed-denied", "sha256:"+strings.Repeat("3", 64)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("service-only principal mutated job")
	}
	other := f.service
	actor := *f.service.Delegation
	other.Delegation = &actor
	actor.NamespaceIDs = []string{f.otherNamespaceID}
	if _, err = f.store.ReadMonitoringEvents(ctx, other, initial.HeadCursor, 10); !errors.Is(err, domain.ErrEventScopeChanged) {
		t.Fatalf("changed scope=%v", err)
	}
	otherKey := f.key
	otherKey.ServiceID = "dashboard-feed-other"
	if err = f.store.RegisterDelegationKeys(ctx, []domain.DelegationKey{f.key, otherKey}); err != nil {
		t.Fatal(err)
	}
	actor.ServiceID = otherKey.ServiceID
	actor.NamespaceIDs = []string{f.namespaceID}
	if _, err = f.store.ReadMonitoringEvents(ctx, other, initial.HeadCursor, 10); !errors.Is(err, domain.ErrEventScopeChanged) {
		t.Fatalf("changed service=%v", err)
	}
	rotated := New(f.pool, []byte("fedcba9876543210fedcba9876543210"))
	if _, err = rotated.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 10); !errors.Is(err, domain.ErrEventCursorInvalid) {
		t.Fatalf("cursor key rotation=%v", err)
	}
	newCheckpoint, err := rotated.MonitoringCheckpoint(ctx, f.service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rotated.ReadMonitoringEvents(ctx, f.service, newCheckpoint.OldestCursor, 10); err != nil {
		t.Fatalf("new checkpoint after key rotation=%v", err)
	}
	if _, err = New(f.pool, nil).MonitoringCheckpoint(ctx, f.service); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatal("missing persistent key issued checkpoint")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE control_instance SET id=gen_random_uuid() WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 10); !errors.Is(err, domain.ErrEventRecoveryChanged) {
		t.Fatalf("changed instance=%v", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE control_instance SET id=$1::uuid WHERE singleton`, initial.ControlInstanceID); err != nil {
		t.Fatal(err)
	}
	f.key.Enabled = false
	if err = f.store.RegisterDelegationKeys(ctx, []domain.DelegationKey{f.key}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.MonitoringCheckpoint(ctx, f.service); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("disabled service retained access")
	}
	f.key.Enabled = true
	if err = f.store.RegisterDelegationKeys(ctx, []domain.DelegationKey{f.key}); err != nil {
		t.Fatal(err)
	}
	// Feed retention is independent of much shorter published outbox retention.
	if _, err = f.pool.Exec(ctx, `DELETE FROM outbox WHERE topic='monitoring.job_terminal.v1' AND published_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	retained, err := f.store.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 10)
	if err != nil || len(retained.Items) != 1 {
		t.Fatal("outbox cleanup removed feed")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE service_recovery_state SET restore_epoch=restore_epoch+1`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.MonitoringCheckpoint(ctx, f.service); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatal("pre-recovery service assertion accepted")
	}
	// The DB recovery floor is 5s in the future; use a just-post-floor assertion
	// inside the verifier's 5s skew by moving only the synthetic floor back 1s.
	if _, err = f.pool.Exec(ctx, `UPDATE service_recovery_state SET delegation_issued_after=statement_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&f.service.Delegation.IssuedAt); err != nil {
		t.Fatal(err)
	}
	f.service.Delegation.ExpiresAt = f.service.Delegation.IssuedAt.Add(time.Minute)
	if _, err = f.store.ReadMonitoringEvents(ctx, f.service, initial.HeadCursor, 10); !errors.Is(err, domain.ErrEventRecoveryChanged) {
		t.Fatalf("recovery cursor=%v", err)
	}
	recovered := f.checkpoint(ctx, t)
	afterRestore, err := f.store.ReadMonitoringEvents(ctx, f.service, recovered.OldestCursor, 10)
	if err != nil || len(afterRestore.Items) != 1 || afterRestore.Items[0].EventID != original.Items[0].EventID {
		t.Fatal("restore changed original event identity")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE monitoring_feed SET published_at=statement_timestamp()-interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	if n, pruneErr := f.store.PruneMonitoringEvents(ctx, 1); pruneErr != nil || n != 1 {
		t.Fatalf("prune=%d,%v", n, pruneErr)
	}
	if _, err = f.store.ReadMonitoringEvents(ctx, f.service, recovered.OldestCursor, 10); !errors.Is(err, domain.ErrEventCursorExpired) {
		t.Fatalf("retention gap=%v", err)
	}
	current := f.checkpoint(ctx, t)
	if current.HeadCursor != current.OldestCursor {
		t.Fatal("empty retention floor not at committed head")
	}
	for _, limit := range []int{0, 10001} {
		if _, err = f.store.PruneMonitoringEvents(ctx, limit); err == nil {
			t.Fatal("invalid prune bound")
		}
	}
	for _, retention := range []time.Duration{time.Hour, 366 * 24 * time.Hour, 24*time.Hour + 1} {
		if err = f.store.ConfigureMonitoringRetention(ctx, retention); err == nil {
			t.Fatal("invalid retention")
		}
	}
	if err = f.store.ConfigureMonitoringRetention(ctx, 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
}
