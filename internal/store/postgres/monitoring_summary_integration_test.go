package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Keep the prior query as an independent differential oracle. This test uses
// only its disposable schema; the observations below are synthetic DB facts,
// not a claim that any workload or agent executed them.
const originalNamespaceSummaryQuery = `
 WITH scope AS (
   SELECT n.id,n.name FROM namespaces AS n
   JOIN authorized_memberships AS m ON m.namespace_id=n.id
   JOIN principals AS p ON p.id=m.principal_id
   WHERE p.issuer=$1 AND p.subject=$2 AND n.name=$3
 ), bounds AS (
   SELECT COALESCE($4::timestamptz, statement_timestamp()-interval '24 hours') AS since,
     COALESCE($5::timestamptz, statement_timestamp()) AS until
 ), scoped_jobs AS (
   SELECT j.*, e.observation_confidence FROM scope JOIN jobs AS j ON j.namespace_id=scope.id
   LEFT JOIN LATERAL (
     SELECT execution.observation_confidence FROM runs AS r
     JOIN executions AS execution ON execution.run_id=r.id WHERE r.job_id=j.id
     ORDER BY r.run_number DESC LIMIT 1
   ) AS e ON true
 )
 SELECT scope.id::text,scope.name,statement_timestamp(),bounds.since,bounds.until,
   (SELECT count(*)::text FROM scoped_jobs),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase <> 'terminal'),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase IN ('accepted','assigning','accepted_execution')),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase <> 'terminal' AND observation_confidence IN ('stale','uncertain','lost')),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase = 'terminal' AND completed_at IS NULL),
   COALESCE((SELECT jsonb_object_agg(phase,total) FROM (SELECT phase,count(*)::text AS total FROM scoped_jobs GROUP BY phase) AS counts),'{}'::jsonb),
   COALESCE((SELECT jsonb_object_agg(outcome,total) FROM (SELECT outcome,count(*)::text AS total FROM scoped_jobs WHERE phase='terminal' AND completed_at >= bounds.since AND completed_at < bounds.until GROUP BY outcome) AS counts),'{}'::jsonb)
 FROM scope CROSS JOIN bounds
`

func TestNamespaceSummaryMixedLifecycleDifferentialIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "summary-test", Subject: "reader"}
	for _, namespace := range []string{"research", "empty"} {
		if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "Synthetic reader", Namespace: namespace}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateTarget(ctx, principal, "research", "summary-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}}); err != nil {
		t.Fatal(err)
	}
	seed, err := store.SubmitJob(ctx, principal, "summary-template", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	var agentID string
	if err = pool.QueryRow(ctx, `
 INSERT INTO agents(id,namespace_id,target_id,target_generation_id,principal_id,
 agent_version,protocol_versions,operating_system,architecture,hostname,execution_user,
 execution_backends,runtimes,capabilities,registration_digest)
 SELECT gen_random_uuid(),namespace_id,target_id,target_generation_id,owner_principal_id,
 'synthetic-test','{jobman/v1alpha1}','linux','amd64','synthetic','synthetic',
 '{subprocess}','{native}','{}','sha256:'||repeat('1',64)
 FROM jobs WHERE id=$1 RETURNING id::text`, seed.Job.ID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	before := from.Add(time.Hour)
	inside, earlier := from.Add(time.Minute), from.Add(-time.Second)
	cases := []struct {
		name, phase, outcome string
		completed            *time.Time
		confidence           []string
	}{
		{"assigning", "assigning", "", nil, nil},
		{"accepted-stale", "accepted_execution", "", nil, []string{"stale"}},
		{"running-current", "running", "", nil, []string{"current"}},
		{"running-uncertain", "running", "", nil, []string{"uncertain"}},
		{"running-lost", "running", "", nil, []string{"lost"}},
		{"retry-current", "running", "", nil, []string{"lost", "current"}},
		// Preserve the existing latest-execution semantics: a newer run with
		// no execution does not hide the last retained execution's confidence.
		{"retry-unexecuted", "running", "", nil, []string{"lost", ""}},
		{"at-start", "terminal", "success", &from, nil},
		{"inside", "terminal", "failure", &inside, nil},
		{"at-end", "terminal", cancelledOutcome, &before, nil},
		{"earlier", "terminal", "timed_out", &earlier, nil},
		{"missing-time", "terminal", "aborted", nil, nil},
		{"terminal-lost", "terminal", "lost", &inside, []string{"lost"}},
	}
	for _, fixture := range cases {
		var jobID string
		if err = pool.QueryRow(ctx, `
 INSERT INTO jobs(id,namespace_id,owner_principal_id,name,labels,phase,desired_state,
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,outcome,completed_at)
 SELECT gen_random_uuid(),namespace_id,owner_principal_id,$2,labels,$3,desired_state,
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,NULLIF($4,''),$5 FROM jobs WHERE id=$1 RETURNING id::text`, seed.Job.ID, fixture.name, fixture.phase, fixture.outcome, fixture.completed).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		for i, confidence := range fixture.confidence {
			var runID string
			if err = pool.QueryRow(ctx, `INSERT INTO runs(id,namespace_id,job_id,run_number,phase,desired_state)
 SELECT gen_random_uuid(),namespace_id,id,$2,'ready','run' FROM jobs WHERE id=$1 RETURNING id::text`, jobID, i+1).Scan(&runID); err != nil {
				t.Fatal(err)
			}
			if confidence == "" {
				continue
			}
			if _, err = pool.Exec(ctx, `INSERT INTO executions(id,namespace_id,run_id,target_id,target_generation_id,agent_id,
 phase,effective_spec_digest,effective_spec,observation_confidence)
 SELECT gen_random_uuid(),namespace_id,$2,target_id,target_generation_id,$3,'planned',request_digest,'{}',$4
 FROM jobs WHERE id=$1`, jobID, runID, agentID, confidence); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Retained history has no execution rows. Its count is large enough to catch
	// a plan that accidentally returns to probing runs for every terminal job.
	if _, err = pool.Exec(ctx, `
 INSERT INTO jobs(id,namespace_id,owner_principal_id,name,labels,phase,desired_state,
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,outcome,completed_at,imported,import_source)
 SELECT gen_random_uuid(),namespace_id,owner_principal_id,'synthetic-history-'||g::text,labels,'terminal','run',
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,'success',$2,true,jsonb_build_object('store','synthetic','jobId',g::text)
 FROM jobs CROSS JOIN generate_series(1,10000) g WHERE id=$1`, seed.Job.ID, inside); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `ANALYZE jobs; ANALYZE runs; ANALYZE executions; ANALYZE namespaces; ANALYZE principals; ANALYZE membership_grants`); err != nil {
		t.Fatal(err)
	}
	for _, window := range []struct {
		name        string
		from, until time.Time
		outcomes    map[string]string
	}{
		{"mixed", from, before, map[string]string{"success": "10001", "failure": "1", "lost": "1"}},
		{"before-start", from.Add(-time.Hour), from, map[string]string{"timed_out": "1"}},
		{"after-end", before, before.Add(time.Hour), map[string]string{cancelledOutcome: "1"}},
	} {
		t.Run(window.name, func(t *testing.T) {
			got, queryErr := store.NamespaceSummary(ctx, principal, "research", &window.from, &window.until)
			if queryErr != nil {
				t.Fatal(queryErr)
			}
			old := scanSummaryOracle(t, pool.QueryRow(ctx, originalNamespaceSummaryQuery, principal.Issuer, principal.Subject, "research", window.from, window.until))
			if got.NamespaceID != old.NamespaceID || got.Namespace != old.Namespace || !got.CompletedFrom.Equal(old.CompletedFrom) || !got.CompletedBefore.Equal(old.CompletedBefore) || got.Total != old.Total || got.Active != old.Active || got.AwaitingExecution != old.AwaitingExecution || got.EvidenceAttention != old.EvidenceAttention || got.MissingCompletionTime != old.MissingCompletionTime || !maps.Equal(got.ByPhase, old.ByPhase) || !maps.Equal(got.ByOutcome, old.ByOutcome) {
				t.Fatalf("optimized summary differs: got=%+v old=%+v", got, old)
			}
			if got.Total != "10014" || got.Active != "8" || got.AwaitingExecution != "3" || got.EvidenceAttention != "4" || got.MissingCompletionTime != "1" || !maps.Equal(got.ByOutcome, window.outcomes) || !maps.Equal(got.ByPhase, map[string]string{"accepted": "1", "assigning": "1", "accepted_execution": "1", "running": "5", "terminal": "10006"}) {
				t.Fatalf("mixed lifecycle facts changed: %+v", got)
			}
		})
	}
	empty, err := store.NamespaceSummary(ctx, principal, "empty", &from, &before)
	if err != nil || empty.Total != "0" || empty.Active != "0" || empty.AwaitingExecution != "0" || empty.EvidenceAttention != "0" || empty.MissingCompletionTime != "0" || len(empty.ByPhase) != 0 || len(empty.ByOutcome) != 0 {
		t.Fatalf("empty authorized namespace=%+v err=%v", empty, err)
	}
	outsider := domain.Principal{Issuer: principal.Issuer, Subject: "outsider"}
	if _, err = store.NamespaceSummary(ctx, outsider, "research", &from, &before); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unrelated principal admitted: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE membership_grants SET revoked_at=statement_timestamp() WHERE principal_id=(SELECT id FROM principals WHERE issuer=$1 AND subject=$2)`, principal.Issuer, principal.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err = store.NamespaceSummary(ctx, principal, "research", &from, &before); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked principal admitted: %v", err)
	}
	// Inspect the actual optimized statement with authorization data restored
	// only inside this disposable fixture; never infer a latency target here.
	if _, err = pool.Exec(ctx, `UPDATE membership_grants SET revoked_at=NULL WHERE principal_id=(SELECT id FROM principals WHERE issuer=$1 AND subject=$2)`, principal.Issuer, principal.Subject); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = pool.QueryRow(ctx, `EXPLAIN(ANALYZE,FORMAT JSON) `+namespaceSummaryQuery, principal.Issuer, principal.Subject, "research", from, before).Scan(&raw); err != nil || len(raw) > 1<<20 {
		t.Fatal("bounded summary plan unavailable:", err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err = json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal("invalid analyzed summary plan")
	}
	var inspect func(map[string]any)
	var foundRuns bool
	inspect = func(node map[string]any) {
		if node["Relation Name"] == "runs" {
			foundRuns = true
			loops, ok := node["Actual Loops"].(float64)
			if !ok || loops > 8 {
				t.Fatalf("terminal history triggered run probes: loops=%v", node["Actual Loops"])
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if nested, valid := child.(map[string]any); valid {
					inspect(nested)
				}
			}
		}
	}
	inspect(plans[0].Plan)
	if !foundRuns {
		t.Fatal("analyzed plan omitted the active confidence query")
	}
}

func scanSummaryOracle(t *testing.T, row pgx.Row) domain.NamespaceSummary {
	t.Helper()
	var s domain.NamespaceSummary
	var phases, outcomes []byte
	if err := row.Scan(&s.NamespaceID, &s.Namespace, &s.AsOf, &s.CompletedFrom, &s.CompletedBefore, &s.Total, &s.Active, &s.AwaitingExecution, &s.EvidenceAttention, &s.MissingCompletionTime, &phases, &outcomes); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(phases, &s.ByPhase) != nil || json.Unmarshal(outcomes, &s.ByOutcome) != nil {
		t.Fatal("invalid baseline count maps")
	}
	return s
}
