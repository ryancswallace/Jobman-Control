package main

// cspell:words SQLSTATE

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func TestGraphCeilingQuotaPreservesHeadroomAndOtherPolicy(t *testing.T) {
	before := domain.NamespacePolicy{Namespace: diagnosticNamespace, MaxActiveJobs: 7, MaxQueuedJobs: 10000, MaxCollectionItems: 89, MaxGraphNodes: 10000, IdempotencyRetention: 24 * time.Hour, PublishedOutboxRetention: 48 * time.Hour, Revision: 6}
	for _, test := range []struct{ current, maximum, want int }{{0, 10000, 10000}, {1, 10000, 20000}, {300, 10000, 20000}, {300, 20000, 20000}} {
		baseline := before
		baseline.MaxQueuedJobs = test.maximum
		got, err := graphCeilingQuota(baseline, test.current)
		if err != nil || got != test.want {
			t.Fatal("quota plan changed headroom", got, err)
		}
		actual := ceilingPolicyChange(baseline, got)
		expected := domain.NamespacePolicyChange{MaxActiveJobs: baseline.MaxActiveJobs, MaxQueuedJobs: test.want, MaxCollectionItems: baseline.MaxCollectionItems, MaxGraphNodes: baseline.MaxGraphNodes, IdempotencyRetention: baseline.IdempotencyRetention, PublishedOutboxRetention: baseline.PublishedOutboxRetention, ExpectedRevision: baseline.Revision}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("unrelated policy change")
		}
	}
	for _, mutate := range []func(*domain.NamespacePolicy){func(p *domain.NamespacePolicy) { p.Namespace = "dashboard-research" }, func(p *domain.NamespacePolicy) { p.MaxGraphNodes = 9999 }, func(p *domain.NamespacePolicy) { p.Revision = 0 }, func(p *domain.NamespacePolicy) { p.MaxQueuedJobs = 1000001 }} {
		invalid := before
		mutate(&invalid)
		if _, err := graphCeilingQuota(invalid, 1); err == nil {
			t.Fatal("unsafe baseline accepted")
		}
	}
	maximum := before
	maximum.MaxQueuedJobs = 1000000
	if _, err := graphCeilingQuota(maximum, 1000000); err == nil {
		t.Fatal("out-of-bounds ceiling quota increase accepted")
	}
	for _, count := range []int{-1, 10001} {
		if _, err := graphCeilingQuota(before, count); err == nil {
			t.Fatal("invalid current count accepted")
		}
	}
}

func TestGraphCeilingIndependentTopologyAndSealedAdmission(t *testing.T) {
	request, err := graphCeilingSubmission()
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Nodes) != 10000 || len(request.Edges) != 100000 || request.Namespace != diagnosticNamespace || request.MaxActive != 1 || request.UnsatisfiedPolicy != "skip" || len(request.RequestDocument) < 2<<20 || len(request.RequestDocument) > 32<<20 {
		t.Fatal("ceiling envelope differs")
	}
	seen := map[[2]int]bool{}
	for _, edge := range request.Edges {
		from, errFrom := strconv.Atoi(edge.From[5:])
		to, errTo := strconv.Atoi(edge.To[5:])
		pair := [2]int{from, to}
		if errFrom != nil || errTo != nil || from >= to || from < 0 || to >= 10000 || seen[pair] || edge.Predicate != "success" || len(edge.Outcomes) != 0 {
			t.Fatal("invalid topology")
		}
		seen[pair] = true
	}
	if len(seen) != 100000 {
		t.Fatal("incomplete topology")
	}
	// Independent complete forward relation of the first twelve nodes.
	for from := 0; from < 12; from++ {
		for to := from + 1; to < 12; to++ {
			if !seen[[2]int{from, to}] {
				t.Fatal("one-hop neighborhood differs")
			}
		}
	}
	for index, node := range request.Nodes {
		if node.Name != graphCeilingNodeName(index) || node.Namespace != diagnosticNamespace || node.Target != graphCeilingTarget || !json.Valid(node.RequestDocument) || node.RequestDigest == "" {
			t.Fatal("node admission projection differs")
		}
	}
}

func TestGraphCeilingPrivatePublicationAndReadBoundaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private POSIX fixture only")
	}
	root, resolveErr := filepath.EvalSymlinks(t.TempDir())
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := graphCeilingPrivateRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := writeGraphCeiling(root, "receipt.json", map[string]bool{"synthetic": true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "receipt.json")
	if _, err := readGraphCeilingPrivate(path, 1024); err != nil {
		t.Fatal(err)
	}
	if err := writeGraphCeiling(root, "receipt.json", map[string]bool{"synthetic": false}); err == nil {
		t.Fatal("existing receipt overwritten")
	}
	if _, err := readGraphCeilingPrivate(path, 2); err == nil {
		t.Fatal("oversized receipt accepted")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readGraphCeilingPrivate(alias, 1024); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGraphCeilingPrivate(path, 1024); err == nil {
		t.Fatal("public file accepted")
	}
}

func TestGraphCeilingAdmissionIntegration(t *testing.T) {
	if os.Getenv("JOBMAN_CONTROL_TEST_GRAPH_CEILING") != "1" {
		t.Skip("explicit disposable-schema ceiling opt-in required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	pool := secondaryIntegrationPool(ctx, t)
	store := postgres.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "https://oidc.lab.test:8443/realms/jobman-lab", Subject: "synthetic-ceiling"}
	if err := store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{Principal: principal, DisplayName: "Synthetic ceiling", Namespace: diagnosticNamespace, Mode: "synthetic-dashboard-lab"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(ctx, principal, diagnosticNamespace, "existing-target", fixtureDigest("existing-target"), domain.TargetSpec{Name: "existing-inert", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"arm64"}}); err != nil {
		t.Fatal(err)
	}
	submission, err := seedSubmission(diagnosticNamespace, "existing-job", "existing-inert")
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.SubmitJob(ctx, principal, "existing-job", submission)
	if err != nil {
		t.Fatal(err)
	}
	if old.Replayed || old.Job.ID == "" || old.Job.Namespace != diagnosticNamespace || old.Job.Name != "existing-job" || old.Job.Phase != "accepted" {
		t.Fatal("existing job admission identity or state differs")
	}
	// Compare the same enriched repository projection before and after admission.
	// SubmitJob returns admission fields, whereas GetJob also resolves ownership,
	// lifecycle, target generation, and an observation timestamp.
	baseline, err := store.GetJob(ctx, principal, diagnosticNamespace, old.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.ID != old.Job.ID || baseline.Namespace != diagnosticNamespace || baseline.Name != "existing-job" || baseline.Phase != "accepted" || baseline.CurrentRun != nil || baseline.AsOf.IsZero() {
		t.Fatal("existing job baseline identity or no-execution state differs")
	}
	request, err := graphCeilingSubmission()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SubmitGraph(ctx, principal, "ceiling-before-quota", request); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatal("existing queue collision did not fail", err)
	}
	before, err := store.GetNamespacePolicy(ctx, principal, diagnosticNamespace)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := graphCeilingQuota(before, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.UpdateNamespacePolicy(ctx, principal, diagnosticNamespace, ceilingPolicyChange(before, queued)); err != nil {
		t.Fatal(err)
	}
	caps, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var namespace string
	if err = pool.QueryRow(ctx, `SELECT id::text FROM namespaces WHERE name=$1`, diagnosticNamespace).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	manifest, err := seedGraphCeiling(ctx, store, principal, graphCeilingPreflight{InstanceID: caps.InstanceID, RecoveryEpoch: caps.RecoveryEpoch, NamespaceID: namespace})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyGraphCeiling(ctx, pool, store, principal, manifest); err != nil {
		t.Fatal(err)
	}
	preserved, err := store.GetJob(ctx, principal, diagnosticNamespace, old.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.AsOf.IsZero() || preserved.AsOf.Before(baseline.AsOf) {
		t.Fatal("existing job observation time regressed")
	}
	// AsOf is transaction_timestamp(), not a stored job field. Every other
	// projected field must remain identical, including durable timestamps.
	baseline.AsOf, preserved.AsOf = time.Time{}, time.Time{}
	if !reflect.DeepEqual(baseline, preserved) {
		beforeValue, afterValue := reflect.ValueOf(baseline), reflect.ValueOf(preserved)
		for i := 0; i < beforeValue.NumField(); i++ {
			if !reflect.DeepEqual(beforeValue.Field(i).Interface(), afterValue.Field(i).Interface()) {
				t.Errorf("existing job field changed: %s", beforeValue.Type().Field(i).Name)
			}
		}
		t.FailNow()
	}
	for _, test := range []struct{ index, nodes, edges int }{{0, 10000, 100000}, {1, 12, 66}, {9999, 2, 1}} {
		result, readErr := store.GraphNeighborhood(ctx, principal, diagnosticNamespace, manifest.GraphID, manifest.Nodes[test.index].ID, 200, 500)
		if readErr != nil || result.TotalNodes != test.nodes || result.TotalEdges != test.edges {
			t.Fatal("neighborhood semantics differ", readErr)
		}
	}
}

func TestGraphCeilingQuotaReceiptRejectsUnrelatedChanges(t *testing.T) {
	before := graphCeilingPreflight{Version: 1, Synthetic: true, InstanceID: graphCeilingPrimaryInstance, RecoveryEpoch: "1", NamespaceID: "76000000-0000-4000-8000-000000000001", Nonterminal: 1, ProposedMaxQueued: 20000, Policy: graphCeilingPolicy{Namespace: diagnosticNamespace, MaxActiveJobs: 7, MaxQueuedJobs: 10000, MaxGraphNodes: 10000, Revision: 6, UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}}
	valid := graphCeilingQuotaReceipt{Before: before, After: before.Policy}
	valid.After.MaxQueuedJobs = 20000
	valid.After.Revision++
	valid.After.UpdatedAt = valid.After.UpdatedAt.Add(time.Second)
	if err := validateGraphCeilingQuota(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*graphCeilingQuotaReceipt){
		func(r *graphCeilingQuotaReceipt) { r.After.MaxQueuedJobs++ },
		func(r *graphCeilingQuotaReceipt) { r.After.MaxActiveJobs++ },
		func(r *graphCeilingQuotaReceipt) { r.After.MaxGraphNodes++ },
		func(r *graphCeilingQuotaReceipt) { r.After.Revision++ },
		func(r *graphCeilingQuotaReceipt) { r.After.UpdatedAt = r.Before.Policy.UpdatedAt.Add(-time.Second) },
		func(r *graphCeilingQuotaReceipt) { r.Before.Nonterminal = 0 },
		func(r *graphCeilingQuotaReceipt) { r.Before.InstanceID = "76000000-0000-4000-8000-000000000002" },
	} {
		invalid := valid
		mutate(&invalid)
		if validateGraphCeilingQuota(invalid) == nil {
			t.Fatal("unrelated policy or unneeded increase accepted")
		}
	}
	noChange := valid
	noChange.Before.Nonterminal = 0
	noChange.Before.ProposedMaxQueued = 10000
	noChange.After = noChange.Before.Policy
	if err := validateGraphCeilingQuota(noChange); err != nil {
		t.Fatal(err)
	}
}

func TestGraphCeilingCompletedReceiptsBindIntentSourceAndExactManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private POSIX fixture only")
	}
	quota := graphCeilingQuotaReceipt{Before: graphCeilingPreflight{InstanceID: graphCeilingPrimaryInstance, RecoveryEpoch: "1", NamespaceID: "76000000-0000-4000-8000-000000000001"}}
	quotaRaw, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	manifest := graphCeilingManifest{InstanceID: quota.Before.InstanceID, RecoveryEpoch: "1", NamespaceID: quota.Before.NamespaceID, GraphID: "76000000-0000-4000-8000-000000000003"}
	graphRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	graphRaw = append(graphRaw, '\n')
	complete := graphCeilingCompletion{HelperCommit: buildinfo.Commit, QuotaSHA256: scaleSHA(quotaRaw), GraphSHA256: scaleSHA(graphRaw), GraphID: manifest.GraphID}
	for _, mode := range []string{"valid", "missing-completion", "changed-intent", "changed-graph", "changed-quota", "changed-commit"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			intent := quota
			currentManifest := manifest
			currentComplete := complete
			switch mode {
			case "changed-intent":
				intent.Before.RecoveryEpoch = "2"
			case "changed-graph":
				currentManifest.NamespaceID = "76000000-0000-4000-8000-000000000004"
			case "changed-quota":
				currentComplete.QuotaSHA256 = scaleSHA([]byte("different"))
			case "changed-commit":
				currentComplete.HelperCommit = "different"
			}
			if err := writeGraphCeiling(root, "seed.intent.json", intent); err != nil {
				t.Fatal(err)
			}
			if err := writeGraphCeiling(root, "graph.json", currentManifest); err != nil {
				t.Fatal(err)
			}
			if mode != "missing-completion" {
				if err := writeGraphCeiling(root, "seed.complete.json", currentComplete); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readCompletedGraphCeiling(root, quota, quotaRaw)
			if mode == "valid" {
				if err != nil || !reflect.DeepEqual(got, manifest) {
					t.Fatal("completed receipt rejected", err)
				}
			} else if err == nil {
				t.Fatal("incomplete or changed receipt accepted")
			}
		})
	}
}

type graphCeilingQueryError string

func (e graphCeilingQueryError) Error() string    { return "private-driver-detail" }
func (e graphCeilingQueryError) SQLState() string { return string(e) }

func TestGraphCeilingQueryDiagnosticIsFinite(t *testing.T) {
	for _, code := range []string{"42703", "XX000", "private-driver-detail", "42\n03", ""} {
		got := graphCeilingQueryFailure(graphCeilingQueryError(code)).Error()
		if strings.Contains(got, "private-driver-detail") || strings.Contains(got, "\n") {
			t.Fatal("private diagnostic escaped")
		}
		expected := "ceiling fact query failed"
		if code == "42703" || code == "XX000" {
			expected += " (sqlstate=" + code + ")"
		}
		if got != expected {
			t.Fatal("unexpected diagnostic", got)
		}
	}
}

func TestGraphCeilingVerifierSchemaIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := secondaryIntegrationPool(ctx, t)
	store := postgres.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	caps, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "76000000-0000-4000-8000-000000000001"
	manifest := graphCeilingManifest{Version: 1, Synthetic: true, Mode: "admitted-no-execution", DeploymentID: diagnosticDeployment, InstanceID: caps.InstanceID, RecoveryEpoch: caps.RecoveryEpoch, NamespaceID: id, Namespace: diagnosticNamespace, TargetID: id, TargetGenerationID: id, GraphID: id, Revision: "1", TotalNodes: "10000", TotalEdges: "100000", RequestDigest: "sha256:" + strings.Repeat("0", 64), Nodes: make([]graphCeilingNode, graphCeilingNodes)}
	err = verifyGraphCeiling(ctx, pool, store, domain.Principal{}, manifest)
	if err == nil || !strings.HasPrefix(err.Error(), "ceiling graph fact mismatch: nodes=0 edges=0") {
		t.Fatal("schema query did not reach finite row validation", err)
	}
}
