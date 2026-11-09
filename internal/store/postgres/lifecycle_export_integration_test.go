package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLifecycleExportIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture := newMonitoringFixture(ctx, t)
	job := fixture.submit(ctx, t, "research", 0)
	fixture.terminal(ctx, t, job)
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE outbox SET created_at = clock_timestamp() - interval '2 hours'
		WHERE topic = 'job.accepted'
	`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.OperationalSnapshot(ctx)
	if err != nil || snapshot.LifecycleBacklog != 1 || snapshot.MonitoringBacklog != 1 ||
		snapshot.OldestLifecycleBacklogAge < 2*time.Hour {
		t.Fatalf("consumer outage snapshot = %#v, %v", snapshot, err)
	}
	export := func(limit int) []byte {
		t.Helper()
		var raw []byte
		if scanErr := fixture.pool.QueryRow(ctx, "SELECT jobman_control_export_lifecycle($1)", limit).Scan(&raw); scanErr != nil {
			t.Fatal(scanErr)
		}
		return raw
	}
	first := export(1)
	var batch struct {
		Epoch  int64 `json:"restoreEpoch"`
		Events []struct {
			ID    string `json:"id"`
			Topic string `json:"topic"`
		} `json:"events"`
	}
	if err = json.Unmarshal(first, &batch); err != nil || batch.Epoch != 1 ||
		len(batch.Events) != 1 || batch.Events[0].Topic != "job.accepted" {
		t.Fatalf("export = %s, %v", first, err)
	}
	// Failed delivery leaves the original ID available on the next pull.
	if _, err = fixture.store.PruneOperationalData(ctx, 100); err != nil || !bytes.Equal(first, export(100)) {
		t.Fatalf("pruning changed an unacknowledged export: %v", err)
	}
	acknowledge := func(epoch int64, id, reference string) (bool, error) {
		t.Helper()
		var changed bool
		scanErr := fixture.pool.QueryRow(ctx, "SELECT jobman_control_ack_lifecycle($1, $2, $3)", epoch, id, reference).Scan(&changed)
		return changed, scanErr
	}
	id := batch.Events[0].ID
	if _, err = SetRecovery(ctx, fixture.pool, 1, 2, true, "restore-test"); err != nil {
		t.Fatal(err)
	}
	_, err = acknowledge(2, id, "test-archive-01")
	assertOperatorConflict(t, err)
	if _, err = SetRecovery(ctx, fixture.pool, 2, 2, false, "restore-test"); err != nil {
		t.Fatal(err)
	}
	_, err = acknowledge(1, id, "test-archive-01")
	assertOperatorConflict(t, err)
	if changed, ackErr := acknowledge(2, id, "test-archive-01"); ackErr != nil || !changed {
		t.Fatalf("durable receipt = %v, %v", changed, ackErr)
	}
	var terminalID string
	if err = fixture.pool.QueryRow(ctx, "SELECT id FROM outbox WHERE topic = 'monitoring.job_terminal.v1'").Scan(&terminalID); err != nil {
		t.Fatal(err)
	}
	_, err = acknowledge(2, terminalID, "test-archive-01")
	assertOperatorConflict(t, err)
	if _, err = fixture.pool.Exec(ctx, `
		UPDATE outbox SET published_at = clock_timestamp() - interval '8 days' WHERE id = $1
	`, id); err != nil {
		t.Fatal(err)
	}
	if removed, pruneErr := fixture.store.PruneOperationalData(ctx, 100); pruneErr != nil || removed != 1 {
		t.Fatalf("prune published delivery = %d, %v", removed, pruneErr)
	}
	if changed, ackErr := acknowledge(2, id, "test-archive-01"); ackErr != nil || changed {
		t.Fatalf("ack replay after pruning = %v, %v", changed, ackErr)
	}
	_, err = acknowledge(2, id, "different-receipt")
	assertOperatorConflict(t, err)
	snapshot, err = fixture.store.OperationalSnapshot(ctx)
	if err != nil || snapshot.LifecycleBacklog != 0 || snapshot.MonitoringBacklog != 1 {
		t.Fatalf("resolved lifecycle snapshot = %#v, %v", snapshot, err)
	}
	var operatorAudit, jobAudit int
	if err = fixture.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM service_operator_audit WHERE action = 'lifecycle.ack'),
		       (SELECT count(*) FROM audit_events WHERE action = 'job.accepted')
	`).Scan(&operatorAudit, &jobAudit); err != nil || operatorAudit != 1 || jobAudit != 1 {
		t.Fatalf("retained audit = %d, %d, %v", operatorAudit, jobAudit, err)
	}
}

func assertOperatorConflict(t *testing.T, err error) {
	t.Helper()
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "40001" {
		t.Fatalf("expected fenced acknowledgement, got %v", err)
	}
}
