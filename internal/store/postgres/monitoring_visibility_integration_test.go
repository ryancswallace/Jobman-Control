package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDirectoryBindingAuthorizationFenceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	const group = "41414141-4141-4141-8141-414141414141"
	var principalID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM principals WHERE issuer=$1 AND subject=$2`, f.owner.Issuer, f.owner.Subject).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	// Retained contributions on both sides model reconfiguration without first
	// rewriting membership rows; unrelated legacy grants must not mask its fence.
	if _, err := f.pool.Exec(ctx, `INSERT INTO membership_grants(id,namespace_id,principal_id,role,provenance,source_key)
 VALUES(gen_random_uuid(),$1,$3,'viewer','directory',$4),(gen_random_uuid(),$2,$3,'operator','directory',$4)`, f.namespaceID, f.otherNamespaceID, principalID, group); err != nil {
		t.Fatal(err)
	}
	read := func() [2]int64 {
		t.Helper()
		var versions [2]int64
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT revision FROM authorization_versions WHERE namespace_id=$1 AND principal_id=$3),(SELECT revision FROM authorization_versions WHERE namespace_id=$2 AND principal_id=$3)`, f.namespaceID, f.otherNamespaceID, principalID).Scan(&versions[0], &versions[1]); err != nil {
			t.Fatal(err)
		}
		return versions
	}
	mutate := func(query string, want [2]bool, args ...any) {
		t.Helper()
		before := read()
		if _, err := f.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
		after := read()
		for i := range before {
			if (after[i] > before[i]) != want[i] {
				t.Fatalf("binding mutation authorization fence %d: before=%d after=%d want advance=%t", i, before[i], after[i], want[i])
			}
		}
	}
	mutate(`INSERT INTO directory_role_bindings(group_id,namespace_id,role) VALUES($1,$2,'viewer')`, [2]bool{true, false}, group, f.namespaceID)
	mutate(`UPDATE directory_role_bindings SET enabled=false WHERE group_id=$1`, [2]bool{true, false}, group)
	mutate(`UPDATE directory_role_bindings SET enabled=true WHERE group_id=$1`, [2]bool{true, false}, group)
	mutate(`UPDATE directory_role_bindings SET revision=revision+1 WHERE group_id=$1`, [2]bool{}, group)
	mutate(`UPDATE directory_role_bindings SET namespace_id=$2,role='operator' WHERE group_id=$1`, [2]bool{true, true}, group, f.otherNamespaceID)
	mutate(`DELETE FROM directory_role_bindings WHERE group_id=$1`, [2]bool{false, true}, group)
	mutate(`INSERT INTO directory_role_bindings(group_id,namespace_id,role) VALUES($1,$2,'operator')`, [2]bool{false, true}, group, f.otherNamespaceID)
}

func TestTargetCatalogLateCommitWatermarkIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, tx)
	// Start the writer before another target commits; transaction time must not
	// assign this delayed writer a timestamp behind an already-visible target.
	var began time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&began); err != nil {
		t.Fatal(err)
	}
	spec := domain.TargetSpec{Name: "committed-later", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}}
	if _, err = f.store.CreateTarget(ctx, f.owner, "research", "watermark-target", fmt.Sprintf("sha256:%064x", 99), spec); err != nil {
		t.Fatal(err)
	}
	pendingID := insertVisibilityTarget(ctx, t, tx, f.namespaceID, "pending-target")
	future := began.Add(time.Hour)
	first, err := f.store.ListTargetCatalog(ctx, f.owner, "research", domain.TargetCatalogOptions{Limit: 1, CreatedBefore: &future})
	if err != nil || first.Total != 2 || first.NextCursor == nil || !first.CreatedBefore.Before(future) {
		t.Fatalf("first catalog: total=%d err=%v", first.Total, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.ListTargetCatalog(ctx, f.owner, "research", domain.TargetCatalogOptions{Limit: 200, CreatedBefore: &first.CreatedBefore, Before: first.NextCursor})
	if err != nil || next.Total != first.Total || len(next.Items) != 1 {
		t.Fatalf("late commit changed traversal: total=%d items=%d err=%v", next.Total, len(next.Items), err)
	}
	fresh, err := f.store.ListTargetCatalog(ctx, f.owner, "research", domain.TargetCatalogOptions{Limit: 200})
	if err != nil || fresh.Total != 3 || fresh.Items[0].ID != pendingID || !fresh.Items[0].CreatedAt.After(first.CreatedBefore) {
		t.Fatalf("fresh catalog omitted new committed target: err=%v", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE targets SET created_at=created_at-interval '1 day' WHERE id=$1`, pendingID); err == nil {
		t.Fatal("creation ordering can be rewritten")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE targets SET namespace_id=$2 WHERE id=$1`, pendingID, f.otherNamespaceID); err == nil {
		t.Fatal("target can move behind another namespace's creation watermark")
	}
}

func insertVisibilityTarget(ctx context.Context, t *testing.T, tx pgx.Tx, namespaceID, name string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO targets(id,namespace_id,name,kind,created_at) VALUES(gen_random_uuid(),$1,$2,'host','2000-01-01') RETURNING id::text`, namespaceID, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := tx.QueryRow(ctx, `INSERT INTO target_generations(id,namespace_id,target_id,generation,execution_backend,control_transport,runtimes,operating_systems,architectures,capabilities,provider)
 VALUES(gen_random_uuid(),$1,$2,1,'subprocess','agent-api',ARRAY['native'],ARRAY[]::text[],ARRAY[]::text[],ARRAY[]::text[],'{"kind":"on-prem"}') RETURNING id::text`, namespaceID, id).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE targets SET current_generation_id=$2 WHERE id=$1`, id, generation); err != nil {
		t.Fatal(err)
	}
	return id
}

func rollbackVisibilityTest(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Error(err)
	}
}

// Interleave a committed publication immediately after the checkpoint's first
// SELECT fixes its repeatable-read snapshot, without timing-based sleeps.
type checkpointInterleaveTx struct {
	pgx.Tx
	afterSnapshot func()
}

func (tx *checkpointInterleaveTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if tx.afterSnapshot != nil {
		action := tx.afterSnapshot
		tx.afterSnapshot = nil
		action()
	}
	return row
}

func TestMonitoringCheckpointSnapshotClockIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	initial := f.checkpoint(ctx, t)
	job := f.submit(ctx, t, "research", 51)
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, tx)
	// A separate connection's timestamp is after BEGIN but before the first
	// snapshot-taking statement. transaction_timestamp() fails this boundary.
	var beforeSnapshot time.Time
	if err = f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&beforeSnapshot); err != nil {
		t.Fatal(err)
	}
	wrapped := &checkpointInterleaveTx{Tx: tx, afterSnapshot: func() {
		f.terminal(ctx, t, job)
		if count, publishErr := f.store.PublishMonitoringEvents(ctx, 10); publishErr != nil || count != 1 {
			t.Fatalf("interleaved publication=%d,%v", count, publishErr)
		}
	}}
	result, _, _, err := f.store.readMonitoringCheckpoint(ctx, wrapped, f.service)
	if err != nil {
		t.Fatal(err)
	}
	var recorded time.Time
	if err = f.pool.QueryRow(ctx, `SELECT created_at FROM outbox WHERE topic='monitoring.job_terminal.v1' AND aggregate_id=$1`, job.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if result.AsOf.Before(beforeSnapshot) || result.AsOf.After(recorded) || result.HeadCursor != initial.HeadCursor || result.BacklogCount != 0 {
		t.Fatalf("checkpoint clock does not match its snapshot: before=%s asOf=%s recorded=%s", beforeSnapshot, result.AsOf, recorded)
	}
	if current := f.checkpoint(ctx, t); current.HeadCursor == initial.HeadCursor {
		t.Fatal("fresh snapshot omitted interleaved commit")
	}
}

func TestMonitoringRecordedAtAfterRowLockIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	job := f.submit(ctx, t, "research", 52)
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, blocker)
	if _, err = blocker.Exec(ctx, `SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	pid := writer.Conn().PgConn().PID()
	completed := make(chan error, 1)
	go func() {
		_, updateErr := writer.Exec(ctx, `UPDATE jobs SET phase='terminal',outcome='failure',revision=revision+1 WHERE id=$1`, job.ID)
		completed <- updateErr
	}()
	waitVisibilityLock(ctx, t, f.pool, pid)
	boundary := f.checkpoint(ctx, t)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-completed; err != nil {
		t.Fatal(err)
	}
	var recorded time.Time
	if err = f.pool.QueryRow(ctx, `SELECT (payload->>'recordedAt')::timestamptz FROM outbox WHERE topic='monitoring.job_terminal.v1' AND aggregate_id=$1`, job.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Before(boundary.AsOf) {
		t.Fatal("row-lock-delayed transition was dated before subscription boundary")
	}
}

// cspell:ignore pids
func waitVisibilityLock(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	for range 10000 {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
	}
	t.Fatal("concurrent statement did not reach the expected database lock")
}

func TestTargetCreationClockCommitOrderIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	first, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, first)
	var firstStamp time.Time
	if err = first.QueryRow(ctx, `INSERT INTO targets(id,namespace_id,name,kind) VALUES(gen_random_uuid(),$1,'first-pending','host') RETURNING created_at`, f.namespaceID).Scan(&firstStamp); err != nil {
		t.Fatal(err)
	}
	writer, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := writer.Conn().PgConn().PID()
	type outcome struct {
		stamp time.Time
		err   error
	}
	completed := make(chan outcome, 1)
	go func() {
		defer writer.Release()
		var result outcome
		result.err = writer.QueryRow(ctx, `INSERT INTO targets(id,namespace_id,name,kind,created_at) VALUES(gen_random_uuid(),$1,'second-pending','host','2000-01-01') RETURNING created_at`, f.namespaceID).Scan(&result.stamp)
		completed <- result
	}()
	// A second allocation must wait for the first transaction to finish. A
	// sequence or bare clock timestamp would permit commit order inversion.
	waitVisibilityLock(ctx, t, f.pool, pid)
	if err = first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-completed
	if result.err != nil || !result.stamp.After(firstStamp) {
		t.Fatalf("creation timestamps do not follow serialized commits: %v", result.err)
	}
}

func TestEmptyTargetCatalogWatermarkIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newMonitoringFixture(ctx, t)
	if err := f.store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: f.owner, DisplayName: "Synthetic catalog reader", Namespace: "empty"}); err != nil {
		t.Fatal(err)
	}
	first, err := f.store.ListTargetCatalog(ctx, f.owner, "empty", domain.TargetCatalogOptions{Limit: 1})
	if err != nil || first.Total != 0 || first.CreatedBefore.IsZero() {
		t.Fatalf("empty catalog watermark: %v", err)
	}
	spec := domain.TargetSpec{Name: "first-target", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}}
	if _, err = f.store.CreateTarget(ctx, f.owner, "empty", "empty-catalog-target", fmt.Sprintf("sha256:%064x", 98), spec); err != nil {
		t.Fatal(err)
	}
	again, err := f.store.ListTargetCatalog(ctx, f.owner, "empty", domain.TargetCatalogOptions{Limit: 1, CreatedBefore: &first.CreatedBefore})
	if err != nil || again.Total != 0 || len(again.Items) != 0 {
		t.Fatalf("empty catalog traversal admitted a later creation: %v", err)
	}
}

func TestMonitoringVisibilityUpgradeFrom21Integration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, pool, migrations[:21]); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	owner := domain.Principal{Issuer: "test-upgrade", Subject: "owner"}
	if err = store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: owner, DisplayName: "Synthetic upgrade reader", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	var namespaceID, targetID string
	var original time.Time
	if err = pool.QueryRow(ctx, `INSERT INTO targets(id,namespace_id,name,kind,created_at) SELECT gen_random_uuid(),id,'existing-target','host','2020-01-01' FROM namespaces WHERE name='research' RETURNING namespace_id::text,id::text,created_at`).Scan(&namespaceID, &targetID, &original); err != nil {
		t.Fatal(err)
	}
	const payload = `{"synthetic":"retained"}`
	if _, err = pool.Exec(ctx, `INSERT INTO monitoring_feed(position,event_id,namespace_id,payload,published_at) VALUES(1,gen_random_uuid(),$1,$2::jsonb,'2020-01-01')`, namespaceID, payload); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = CheckMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var retained, clock time.Time
	var payloadRetained bool
	if err = pool.QueryRow(ctx, `SELECT t.created_at,c.last_created_at,(SELECT payload=$3::jsonb FROM monitoring_feed WHERE position=1) FROM targets t JOIN target_catalog_clocks c USING(namespace_id) WHERE t.id=$1 AND t.namespace_id=$2`, targetID, namespaceID, payload).Scan(&retained, &clock, &payloadRetained); err != nil {
		t.Fatal(err)
	}
	if !retained.Equal(original) || !clock.Equal(original) || !payloadRetained {
		t.Fatal("upgrade rewrote existing target or event history")
	}
	var next time.Time
	if err = pool.QueryRow(ctx, `INSERT INTO targets(id,namespace_id,name,kind,created_at) VALUES(gen_random_uuid(),$1,'new-target','host','2000-01-01') RETURNING created_at`, namespaceID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if !next.After(original) {
		t.Fatal("upgraded creation clock accepted a backdated insert")
	}
	if err = migrate(ctx, pool, migrations[:21]); err == nil {
		t.Fatal("old binary accepted upgraded migration ledger")
	}
}
