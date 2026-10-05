package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Independent pre-optimization predicate/order oracle; do not share the new
// filter fragment, so adding or dropping a fast-path filter breaks comparison.
const originalJobListQuery = jobSelect + `
 JOIN authorized_memberships AS m ON m.namespace_id = n.id
 JOIN principals AS p ON p.id = m.principal_id
 WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
 AND ($4::text = '' OR j.phase = $4 OR ($4 = 'active' AND j.phase <> 'terminal') OR ($4 = 'awaiting' AND j.phase IN ('accepted','assigning','accepted_execution')))
 AND ($5::timestamptz IS NULL OR (j.created_at, j.id) < ($5, $6::uuid))
 AND ($8::text = '' OR j.outcome = $8)
 AND (NULLIF($9, '')::uuid IS NULL OR (NOT j.imported AND j.owner_principal_id = NULLIF($9, '')::uuid))
 AND ($10::timestamptz IS NULL OR j.completed_at >= $10)
 AND ($11::timestamptz IS NULL OR j.completed_at < $11)
 AND ($12::timestamptz IS NULL OR j.created_at <= $12)
 AND (NULLIF($13, '')::uuid IS NULL OR j.id = NULLIF($13, '')::uuid)
 AND ($14::text = '' OR current_execution.observation_confidence = $14
 OR ($14 = 'attention' AND j.phase <> 'terminal' AND current_execution.observation_confidence IN ('stale','uncertain','lost')))
 ORDER BY j.created_at DESC, j.id DESC LIMIT $7
`

func TestJobListCandidateDifferentialIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "list-test", Subject: "reader"}
	other := domain.Principal{Issuer: principal.Issuer, Subject: "other"}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var seed domain.Job
	var otherID string
	for i := range 10 {
		namespace := fmt.Sprintf("list-%02d", i)
		for _, caller := range []domain.Principal{principal, other} {
			if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: caller, DisplayName: "Synthetic list fixture", Namespace: namespace}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.CreateTarget(ctx, principal, namespace, "list-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}}); err != nil {
			t.Fatal(err)
		}
		submission := integrationSubmission(t)
		submission.Namespace = namespace
		result, err := store.SubmitJob(ctx, principal, "list-template", submission)
		if err != nil {
			t.Fatal(err)
		}
		// Keep the real fixture job newer than copied history without relying on
		// the wall clock used by this test runner.
		if _, err = pool.Exec(ctx, `UPDATE jobs SET created_at=$2,updated_at=$2 WHERE id=$1`, result.Job.ID, base.Add(200*time.Second)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			seed = result.Job
			if err = pool.QueryRow(ctx, `SELECT id::text FROM principals WHERE issuer=$1 AND subject=$2`, other.Issuer, other.Subject).Scan(&otherID); err != nil {
				t.Fatal(err)
			}
		}
		// Exactly ten authorized namespaces with many tied timestamps reproduce
		// the membership multiplier. These are synthetic retained database rows,
		// not execution evidence; imported rows have no run or execution.
		if _, err = pool.Exec(ctx, `
 INSERT INTO jobs(id,namespace_id,owner_principal_id,name,labels,phase,desired_state,
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,created_at,updated_at,outcome,completed_at,imported,import_source)
 SELECT gen_random_uuid(),namespace_id,CASE WHEN g%2=0 THEN $3::uuid ELSE owner_principal_id END,
 'synthetic-list-'||g::text,jsonb_build_object('fixture',g::text),
 CASE WHEN g>20 THEN 'terminal' ELSE (ARRAY['accepted','assigning','accepted_execution','running'])[1+g%4] END,'run',
 placement_target,placement_partition,workload_digest,request_digest,request_document,target_id,target_generation_id,
 $2::timestamptz+(g/10)*interval '1 second',
 $2::timestamptz+(CASE WHEN g>20 THEN g ELSE g/10 END)*interval '1 second',
 CASE WHEN g>20 THEN CASE WHEN g%2=0 THEN 'success' ELSE 'failure' END END,
 CASE WHEN g>20 THEN $2::timestamptz+g*interval '1 second' END,
 g>100,CASE WHEN g>100 THEN jsonb_build_object('store','synthetic','jobId',g::text) END
 FROM jobs CROSS JOIN generate_series(1,1000) g WHERE id=$1`, result.Job.ID, base, otherID); err != nil {
			t.Fatal(err)
		}
	}
	// Add current execution metadata to the most recent real fixture job; the
	// unchanged confidence-filter path must select on that metadata before LIMIT.
	var agentID, runID string
	if err := pool.QueryRow(ctx, `INSERT INTO agents(id,namespace_id,target_id,target_generation_id,principal_id,
 agent_version,protocol_versions,operating_system,architecture,hostname,execution_user,execution_backends,runtimes,capabilities,registration_digest)
 SELECT gen_random_uuid(),namespace_id,target_id,target_generation_id,owner_principal_id,
 'synthetic','{jobman/v1alpha1}','linux','amd64','synthetic','synthetic','{subprocess}','{native}','{}','sha256:'||repeat('1',64)
 FROM jobs WHERE id=$1 RETURNING id::text`, seed.ID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO runs(id,namespace_id,job_id,run_number,phase,desired_state)
 SELECT gen_random_uuid(),namespace_id,id,1,'ready','run' FROM jobs WHERE id=$1 RETURNING id::text`, seed.ID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO executions(id,namespace_id,run_id,target_id,target_generation_id,agent_id,
 phase,effective_spec_digest,effective_spec,observation_confidence,native_id)
 SELECT gen_random_uuid(),namespace_id,$2,target_id,target_generation_id,$3,'planned',request_digest,'{}','stale','synthetic-native'
 FROM jobs WHERE id=$1`, seed.ID, runID, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE jobs; ANALYZE runs; ANALYZE executions; ANALYZE namespaces; ANALYZE principals; ANALYZE membership_grants`); err != nil {
		t.Fatal(err)
	}
	lower, upper, creation := base.Add(30*time.Second), base.Add(60*time.Second), base.Add(50*time.Second)
	filters := map[string]domain.JobListOptions{
		"first": {Limit: 50}, "active": {Limit: 50, Phase: "active"}, "awaiting": {Limit: 50, Phase: "awaiting"},
		"running": {Limit: 50, Phase: "running"}, "terminal": {Limit: 50, Phase: "terminal"},
		"success": {Limit: 50, Outcome: "success"}, "unknown-outcome": {Limit: 50, Outcome: "future"},
		"owner": {Limit: 50, OwnerPrincipalID: seed.Owner.ID}, "other-owner": {Limit: 50, OwnerPrincipalID: otherID},
		"completion-window": {Limit: 50, CompletedFrom: &lower, CompletedBefore: &upper},
		"created-before":    {Limit: 50, CreatedBefore: &creation}, "exact": {Limit: 50, JobID: seed.ID},
		"combined": {Limit: 11, Phase: "terminal", Outcome: "success", OwnerPrincipalID: otherID, CompletedFrom: &lower, CompletedBefore: &upper, CreatedBefore: &creation},
		"stale":    {Limit: 1, Confidence: "stale"}, "attention": {Limit: 1, Confidence: "attention"}, "current": {Limit: 1, Confidence: "current"},
	}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackJobListTest(ctx, t, tx)
			if _, err = tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode); err != nil {
				t.Fatal(err)
			}
			for name, options := range filters {
				t.Run(name, func(t *testing.T) {
					args := jobListTestArgs(principal, options)
					old := readJobListQuery(ctx, t, tx, originalJobListQuery, args)
					got := readJobListQuery(ctx, t, tx, jobListQuery(options.Confidence), args)
					if !reflect.DeepEqual(got, old) {
						t.Fatalf("candidate page differs from full projection for %s: got=%d old=%d", name, len(got), len(old))
					}
					if name == "first" {
						if len(got) != 51 || got[0].CurrentRun == nil || got[0].NativeID != "synthetic-native" {
							t.Fatal("first page lost lookahead or current execution metadata")
						}
						options.Before = &domain.JobCursor{CreatedAt: old[9].CreatedAt, ID: old[9].ID}
						args = jobListTestArgs(principal, options)
						if !reflect.DeepEqual(readJobListQuery(ctx, t, tx, jobListCandidateQuery, args), readJobListQuery(ctx, t, tx, originalJobListQuery, args)) {
							t.Fatal("equal-time exclusive cursor changed the selected IDs")
						}
					}
				})
			}
			assertCandidatePlanBounded(ctx, t, tx, jobListTestArgs(principal, domain.JobListOptions{Limit: 50}))
		})
	}
	// Traverse all tied pages through the actual repository method, preserving
	// page trimming, source scope and final NextCursor semantics.
	seen := map[string]bool{}
	options := domain.JobListOptions{Limit: 50}
	for range 21 {
		page, err := store.ListJobs(ctx, principal, "list-00", options)
		if err != nil || len(page.Jobs) == 0 || len(page.Jobs) > 50 {
			t.Fatalf("bounded repository page failed: %v", err)
		}
		for _, job := range page.Jobs {
			if seen[job.ID] || job.NamespaceID != seed.NamespaceID {
				t.Fatal("cursor duplicated or crossed a namespace")
			}
			seen[job.ID] = true
		}
		options.Before = page.NextCursor
		if page.NextCursor == nil {
			break
		}
	}
	if len(seen) != 1001 || options.Before != nil {
		t.Fatalf("retained pagination incomplete: %d", len(seen))
	}
	// Concurrent revocation preserves existing repeatable-read semantics. A
	// read already authorized in its snapshot remains identical to the prior
	// query; every newly started repository read observes and denies removal.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackJobListTest(ctx, t, tx)
	if _, err = authorizeNamespace(ctx, tx, principal, "list-00", domain.CapabilityJobsRead); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE membership_grants SET revoked_at=statement_timestamp() WHERE principal_id=$1 AND namespace_id=$2`, seed.Owner.ID, seed.NamespaceID); err != nil {
		t.Fatal(err)
	}
	args := jobListTestArgs(principal, domain.JobListOptions{Limit: 50})
	old := readJobListQuery(ctx, t, tx, originalJobListQuery, args)
	got := readJobListQuery(ctx, t, tx, jobListCandidateQuery, args)
	if len(got) != 51 || !reflect.DeepEqual(got, old) {
		t.Fatal("concurrent revocation changed established snapshot semantics")
	}
	if _, err = store.ListJobs(ctx, principal, "list-00", domain.JobListOptions{Limit: 50}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("new request failed to observe revocation: %v", err)
	}
}

func jobListTestArgs(p domain.Principal, o domain.JobListOptions) []any {
	var at, id any
	if o.Before != nil {
		at, id = o.Before.CreatedAt, o.Before.ID
	}
	return []any{p.Issuer, p.Subject, "list-00", o.Phase, at, id, o.Limit + 1, o.Outcome, o.OwnerPrincipalID, o.CompletedFrom, o.CompletedBefore, o.CreatedBefore, o.JobID, o.Confidence}
}

func rollbackJobListTest(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Error(err)
	}
}

func readJobListQuery(ctx context.Context, t *testing.T, tx pgx.Tx, query string, args []any) []domain.Job {
	t.Helper()
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []domain.Job{}
	for rows.Next() {
		job, scanErr := scanJob(rows)
		if scanErr != nil {
			t.Fatal(scanErr)
		}
		result = append(result, job)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertCandidatePlanBounded(ctx context.Context, t *testing.T, tx pgx.Tx, args []any) {
	t.Helper()
	var raw []byte
	if err := tx.QueryRow(ctx, `EXPLAIN(ANALYZE,FORMAT JSON) `+jobListCandidateQuery, args...).Scan(&raw); err != nil || len(raw) > 1<<20 {
		t.Fatal("bounded candidate plan unavailable:", err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal("invalid candidate plan")
	}
	var inspect func(map[string]any)
	var jobRows, runLoops float64
	inspect = func(node map[string]any) {
		if node["Relation Name"] == "runs" || node["Relation Name"] == "jobs" {
			loops, ok := node["Actual Loops"].(float64)
			rows, valid := node["Actual Rows"].(float64)
			if !ok || !valid {
				t.Fatal("relation missing analyzed bounds")
			}
			if node["Relation Name"] == "runs" {
				runLoops += loops
			} else {
				if removed, present := node["Rows Removed by Filter"].(float64); present {
					rows += removed
				}
				jobRows += rows * loops
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if next, valid := child.(map[string]any); valid {
					inspect(next)
				}
			}
		}
	}
	inspect(plans[0].Plan)
	if runLoops == 0 || runLoops > 51 || jobRows > 2002 {
		t.Fatalf("candidate boundary exceeded: runLoops=%v examinedJobs=%v", runLoops, jobRows)
	}
}
