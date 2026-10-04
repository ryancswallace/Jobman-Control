package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestRunCursorBounds(t *testing.T) {
	t.Parallel()
	store := New(nil, []byte("0123456789abcdef0123456789abcdef"))
	for _, value := range []string{"", "bad", strings.Repeat("a", 1025), base64.RawURLEncoding.EncodeToString([]byte(`{"unknown":1}`))} {
		if _, err := store.decodeRunCursor(value); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("unsafe cursor accepted")
		}
	}
}

func TestRunCatalogIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	p := domain.Principal{Issuer: "runs-test", Subject: "owner"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: p, DisplayName: "Run owner", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTarget(ctx, p, "research", "runs-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SubmitJob(ctx, p, "runs-first", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitJob(ctx, p, "runs-second", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.ListRuns(ctx, p, "research", first.Job.ID, domain.RunListOptions{Limit: 50})
	if err != nil || len(empty.Items) != 0 || empty.Total != 0 {
		t.Fatalf("unassigned job run list=%+v,error=%v", empty, err)
	}
	if _, err = store.CancelJob(ctx, p, "research", first.Job.ID, "run-cancel", "sha256:"+strings.Repeat("4", 64)); err != nil {
		t.Fatal(err)
	}
	// No agent/execution is invented by run selection. Seed additional factual
	// run rows only inside this disposable schema to exercise historical paging.
	if _, err = pool.Exec(ctx, `INSERT INTO runs(id,namespace_id,job_id,run_number,phase,desired_state,outcome) SELECT gen_random_uuid(),namespace_id,id,n,'terminal','cancel','cancelled' FROM jobs CROSS JOIN generate_series(1,103) n WHERE id=$1`, first.Job.ID); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ListRuns(ctx, p, "research", first.Job.ID, domain.RunListOptions{Limit: 50})
	if err != nil || len(initial.Items) != 50 || initial.Items[0].Number != 103 || initial.Total != 103 || initial.NextPageToken == "" {
		t.Fatalf("initial count=%d,total=%d,error=%v", len(initial.Items), initial.Total, err)
	}
	detail, err := store.GetRun(ctx, p, "research", first.Job.ID, initial.Items[0].ID)
	if err != nil || detail.Run.Number != 103 || detail.Run.ExecutionID != "" || detail.Run.TargetID != "" || detail.JobID != first.Job.ID || detail.Run.CreatedAt.IsZero() {
		t.Fatalf("run detail=%+v,error=%v", detail, err)
	}
	if _, err = store.GetRun(ctx, p, "research", second.Job.ID, detail.Run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-job detail=%v", err)
	}
	// Later admissions do not change the frozen run ceiling or total.
	if _, err = pool.Exec(ctx, `INSERT INTO runs(id,namespace_id,job_id,run_number,phase,desired_state,outcome) SELECT gen_random_uuid(),namespace_id,id,104,'terminal','cancel','cancelled' FROM jobs WHERE id=$1`, first.Job.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	page := initial
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("unbounded run pagination")
		}
		if page.Total != 103 {
			t.Fatal("ceiling count changed")
		}
		for _, run := range page.Items {
			if seen[run.Number] || run.Number > 103 {
				t.Fatal("duplicate or post-ceiling row")
			}
			seen[run.Number] = true
		}
		if page.NextPageToken == "" {
			break
		}
		page, err = store.ListRuns(ctx, p, "research", first.Job.ID, domain.RunListOptions{Limit: 50, PageToken: page.NextPageToken})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 103 {
		t.Fatalf("run traversal=%d", len(seen))
	}
	if _, err = store.ListRuns(ctx, p, "research", second.Job.ID, domain.RunListOptions{Limit: 50, PageToken: initial.NextPageToken}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("cross-job cursor=%v", err)
	}
	outsider := domain.Principal{Issuer: p.Issuer, Subject: "outsider"}
	if _, err = store.ListRuns(ctx, outsider, "research", first.Job.ID, domain.RunListOptions{Limit: 1, PageToken: initial.NextPageToken}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("outsider=%v", err)
	}
	cursor, err := store.decodeRunCursor(initial.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Expires = initial.AsOf.Add(-time.Second)
	if _, err = store.ListRuns(ctx, p, "research", first.Job.ID, domain.RunListOptions{Limit: 50, PageToken: store.encodeRunCursor(cursor)}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired cursor=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE service_recovery_state SET restore_epoch=restore_epoch+1`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListRuns(ctx, p, "research", first.Job.ID, domain.RunListOptions{Limit: 50, PageToken: initial.NextPageToken}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restored cursor=%v", err)
	}
	// Use a real normal assignment for the execution reference projection.
	enrollment, err := store.CreateEnrollmentToken(ctx, p, "research", "workstation-a", "runs-enroll", "sha256:"+strings.Repeat("2", 64), domain.EnrollmentRequest{Principal: p, ExpectedUser: "researcher", Lifetime: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{TargetGenerationID: target.Value.GenerationID, AgentVersion: "0.1.0", ProtocolVersions: []string{"jobman/v1alpha1"}, OperatingSystem: "linux", Architecture: "amd64", Hostname: "run-test", ExecutionUser: "researcher", ExecutionBackends: []string{"subprocess"}, Runtimes: []string{"native"}, RequestDigest: "sha256:" + strings.Repeat("3", 64)}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReconcileAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	current, err := store.ListRuns(ctx, p, "research", second.Job.ID, domain.RunListOptions{Limit: 1})
	if err != nil || len(current.Items) != 1 || current.Items[0].ExecutionID == "" || current.Items[0].TargetGenerationID != target.Value.GenerationID || current.Items[0].Backend != "subprocess" {
		t.Fatalf("assigned run=%+v,error=%v", current, err)
	}
	// Removing the principal's last contribution also revokes previously issued pages.
	if _, err = pool.Exec(ctx, `DELETE FROM membership_grants WHERE principal_id IN (SELECT id FROM principals WHERE issuer=$1 AND subject=$2)`, p.Issuer, p.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListRuns(ctx, p, "research", first.Job.ID, domain.RunListOptions{Limit: 1, PageToken: initial.NextPageToken}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked cursor=%v", err)
	}
	for _, q := range []domain.RunListOptions{{Limit: 0}, {Limit: 101}, {Limit: 1, PageToken: strings.Repeat("x", 1025)}} {
		if _, e := store.ListRuns(ctx, p, "research", first.Job.ID, q); e == nil {
			t.Fatalf("invalid run options accepted: %+v", q)
		}
	}
}

func TestRunCursorAuthentication(t *testing.T) {
	t.Parallel()
	store := New(nil, []byte("0123456789abcdef0123456789abcdef"))
	id := "79000000-0000-4000-8000-000000000001"
	cursor := runPageCursor{NamespaceID: id, JobID: id, PrincipalID: id, InstanceID: id, Epoch: 1, Grant: 1, Ceiling: 103, Before: 53, Expires: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	encoded := store.encodeRunCursor(cursor)
	if got, err := store.decodeRunCursor(encoded); err != nil || got != cursor {
		t.Fatalf("signed cursor: %#v %v", got, err)
	}
	payload, signature, _ := strings.Cut(encoded, ".")
	for _, field := range []string{"namespace", "job", "principal", "instance", "epoch", "grant", "ceiling", "before", "expiry"} {
		t.Run(field, func(t *testing.T) {
			changed := cursor
			switch field {
			case "namespace":
				changed.NamespaceID = "79000000-0000-4000-8000-000000000002"
			case "job":
				changed.JobID = "79000000-0000-4000-8000-000000000002"
			case "principal":
				changed.PrincipalID = "79000000-0000-4000-8000-000000000002"
			case "instance":
				changed.InstanceID = "79000000-0000-4000-8000-000000000002"
			case "epoch":
				changed.Epoch++
			case "grant":
				changed.Grant++
			case "ceiling":
				changed.Ceiling++
			case "before":
				changed.Before++
			case "expiry":
				changed.Expires = changed.Expires.Add(time.Hour)
			}
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			token := base64.RawURLEncoding.EncodeToString(raw) + "." + signature
			if _, err = store.decodeRunCursor(token); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("tampered selector accepted")
			}
		})
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	wrongDomain := hmac.New(sha256.New, store.tokenKey)
	_, _ = wrongDomain.Write(raw)
	for _, value := range []string{payload, payload + "." + base64.RawURLEncoding.EncodeToString(wrongDomain.Sum(nil)), encoded + "=", encoded + ".extra"} {
		if _, err = store.decodeRunCursor(value); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("wrong-purpose or noncanonical token accepted")
		}
	}
	for _, raw := range []string{`null`, `{}`, `invalid`, `{"extra":true}`, string(raw) + `{}`} {
		signed := base64.RawURLEncoding.EncodeToString([]byte(raw)) + "." + base64.RawURLEncoding.EncodeToString(store.runCursorMAC([]byte(raw)))
		if _, err = store.decodeRunCursor(signed); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("authenticated malformed selector accepted")
		}
	}
	replica := New(nil, store.tokenKey)
	if got, e := replica.decodeRunCursor(encoded); e != nil || got != cursor {
		t.Fatal("same-key replica rejected cursor")
	}
	rotated := New(nil, []byte("fedcba9876543210fedcba9876543210"))
	if _, e := rotated.decodeRunCursor(encoded); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("retired key accepted")
	}
	missing := New(nil, nil)
	if missing.encodeRunCursor(cursor) != "" {
		t.Fatal("missing key signed cursor")
	}
	if _, e := missing.decodeRunCursor(encoded); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("missing key verified cursor")
	}
}
