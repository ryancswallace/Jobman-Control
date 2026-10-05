package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// This opt-in source-database test creates only a random disposable schema.
// Its history is explicitly synthetic imported metadata, not execution evidence.
// It measures repository reads, not Dashboard HTTP, directory, client rendering,
// two-Control aggregation, or the separate real-workload acceptance scenarios.
func TestMonitoringAcceptedScaleIntegration(t *testing.T) {
	if os.Getenv("JOBMAN_CONTROL_TEST_SCALE") != "1" || os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL") == "" {
		t.Skip("explicit disposable database and scale-test opt-ins required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	pool := newIntegrationPool(ctx, t, os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL"))
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principals := make([]domain.Principal, 25)
	namespaces := make([]string, 10)
	seedIDs := make([]string, len(namespaces))
	for i := range principals {
		principals[i] = domain.Principal{Issuer: "https://synthetic-scale.invalid", Subject: fmt.Sprintf("viewer-%02d", i)}
	}
	for n := range namespaces {
		namespace := fmt.Sprintf("scale-%02d", n)
		namespaces[n] = namespace
		for _, principal := range principals {
			if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: principal.Subject, Namespace: namespace}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.CreateTarget(ctx, principals[0], namespace, "scale-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}); err != nil {
			t.Fatal(err)
		}
		submission := integrationSubmission(t)
		submission.Namespace = namespace
		seed, err := store.SubmitJob(ctx, principals[0], "scale-template", submission)
		if err != nil {
			t.Fatal(err)
		}
		seedIDs[n] = seed.Job.ID
		// Keep the one admitted pending job plus49 pending copies. Retained
		// history has truthful imported provenance, no run and no event emission.
		_, err = pool.Exec(ctx, `
 INSERT INTO jobs(id,namespace_id,owner_principal_id,name,labels,phase,desired_state,
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,created_at,updated_at,imported,import_source,
 outcome,completed_at,completed_recorded_at,completed_provenance)
 SELECT gen_random_uuid(),j.namespace_id,j.owner_principal_id,'scale-job-'||g::text,
 jsonb_build_object('fixture','synthetic-scale'),
 CASE WHEN g<=49 THEN 'accepted' ELSE 'terminal' END,'run',
 j.placement_target,j.placement_partition,j.workload_digest,j.request_digest,j.request_document,
 j.target_id,j.target_generation_id,
 transaction_timestamp()-interval '2 hours'+(g/25)*interval '1 second',transaction_timestamp(),
 g>49,CASE WHEN g>49 THEN jsonb_build_object('store','sqlite','schema',1,'jobId','scale-'||g::text) END,
 CASE WHEN g>49 THEN 'success' END,
 CASE WHEN g>49 THEN transaction_timestamp()-interval '1 hour' END,
 CASE WHEN g>49 THEN transaction_timestamp() END,
 CASE WHEN g>49 THEN 'history_import' END
 FROM jobs j CROSS JOIN generate_series(1,10049) g WHERE j.id=$1::uuid`, seed.Job.ID)
		if err != nil {
			t.Fatal("seed bounded synthetic history:", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE memberships SET role='viewer'; ANALYZE jobs; ANALYZE namespaces; ANALYZE memberships; ANALYZE principals; ANALYZE runs; ANALYZE executions`); err != nil {
		t.Fatal(err)
	}
	var jobs, active, imported int
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE phase<>'terminal'),count(*) FILTER(WHERE imported) FROM jobs`).Scan(&jobs, &active, &imported); err != nil || jobs != 100500 || active != 500 || imported != 100000 {
		t.Fatalf("scale fixture counts jobs=%d active=%d imported=%d err=%v", jobs, active, imported, err)
	}

	// Traverse a whole namespace across equal creation timestamps, proving the
	// UUID tie-breaker and final page at full retained-history size.
	seen := map[string]bool{}
	var cursor *domain.JobCursor
	for page := 0; page < 52; page++ {
		result, err := store.ListJobs(ctx, principals[0], namespaces[0], domain.JobListOptions{Limit: 200, Before: cursor})
		if err != nil || len(result.Jobs) > 200 || result.AsOf.IsZero() {
			t.Fatal("bounded full-history page failed:", err)
		}
		for _, job := range result.Jobs {
			if seen[job.ID] || job.Namespace != namespaces[0] {
				t.Fatal("stable pagination duplicated or crossed a namespace")
			}
			seen[job.ID] = true
		}
		cursor = result.NextCursor
		if cursor == nil {
			break
		}
	}
	if cursor != nil || len(seen) != 10050 {
		t.Fatalf("full-history traversal returned %d unique jobs", len(seen))
	}

	type sample struct {
		operation string
		duration  time.Duration
		err       error
	}
	results := make(chan sample, 25*8*3)
	start := make(chan struct{})
	var group sync.WaitGroup
	for viewer, principal := range principals {
		group.Go(func() {
			<-start
			for iteration := range 8 {
				n := (viewer + iteration) % len(namespaces)
				began := time.Now()
				page, err := store.ListJobs(ctx, principal, namespaces[n], domain.JobListOptions{Limit: 50})
				if err == nil && (len(page.Jobs) != 50 || page.NextCursor == nil || page.AsOf.IsZero()) {
					err = fmt.Errorf("invalid bounded list")
				}
				results <- sample{"list", time.Since(began), err}
				began = time.Now()
				job, err := store.GetJob(ctx, principal, namespaces[n], seedIDs[n])
				if err == nil && (job.ID != seedIDs[n] || job.Namespace != namespaces[n] || job.Imported) {
					err = fmt.Errorf("invalid authorized detail")
				}
				results <- sample{"detail", time.Since(began), err}
				began = time.Now()
				summary, err := store.NamespaceSummary(ctx, principal, namespaces[n], nil, nil)
				if err == nil && (summary.Total != "10050" || summary.Active != "50" || summary.AwaitingExecution != "50" || summary.ByOutcome["success"] != "10000" || summary.MissingCompletionTime != "0") {
					err = fmt.Errorf("summary differs from complete fixed dataset")
				}
				results <- sample{"summary", time.Since(began), err}
			}
		})
	}
	close(start)
	group.Wait()
	close(results)
	timings := map[string][]time.Duration{}
	for result := range results {
		if result.err != nil {
			t.Fatalf("scale %s failed: %v", result.operation, result.err)
		}
		timings[result.operation] = append(timings[result.operation], result.duration)
	}
	for _, operation := range []string{"list", "detail", "summary"} {
		values := timings[operation]
		slices.Sort(values)
		if len(values) != 200 {
			t.Fatal("missing viewer samples")
		}
		p95 := values[(len(values)*95+99)/100-1]
		t.Logf("source repository scale: operation=%s samples=%d p50=%s p95=%s max=%s viewers=25 namespaces=10 jobs=100500 active=500 poolMax=%d", operation, len(values), values[99], p95, values[len(values)-1], pool.Config().MaxConns)
		if p95 > 2*time.Second {
			t.Errorf("%s source read p95 exceeds the2s upper bound before network/client overhead", operation)
		}
	}

	// Record the real projection's analyzed plan, separately from the timed
	// authorized method above. No raw request document or user data is logged.
	var plan []byte
	if err := pool.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) `+jobSelect+` WHERE n.name=$1 ORDER BY j.created_at DESC,j.id DESC LIMIT 200`, namespaces[0]).Scan(&plan); err != nil || len(plan) > 1<<20 {
		t.Fatal("bounded projection plan unavailable:", err)
	}
	var parsed []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if json.Unmarshal(plan, &parsed) != nil || len(parsed) != 1 {
		t.Fatal("invalid projection plan")
	}
	indexes := map[string]bool{}
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if name, ok := node["Index Name"].(string); ok {
			indexes[name] = true
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if next, ok := child.(map[string]any); ok {
					visit(next)
				}
			}
		}
	}
	visit(parsed[0].Plan)
	names := make([]string, 0, len(indexes))
	for name := range indexes {
		names = append(names, name)
	}
	slices.Sort(names)
	t.Logf("bounded200-row projection: analyzedMillis=%.3f indexes=%v", parsed[0].ExecutionTime, names)
}
