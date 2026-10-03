package postgres

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestContributingGrantsIntegration(t *testing.T) {
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
	admin := domain.Principal{Issuer: "test-issuer", Subject: "test-subject"}
	for _, ns := range []string{"research", "other"} {
		if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: admin, DisplayName: "Administrator", Namespace: ns}); err != nil {
			t.Fatal(err)
		}
	}
	member := domain.Principal{Issuer: "test-issuer", Subject: "member"}
	unknown, err := store.CurrentPrincipal(ctx, member, "", 50)
	if err != nil || unknown.PrincipalID != "" || len(unknown.Namespaces) != 0 || unknown.AuthorizationCheckedAt.IsZero() {
		t.Fatalf("unknown principal = %#v, %v", unknown, err)
	}
	viewerID := "10000000-0000-4000-8000-000000000001"
	submitterID := "10000000-0000-4000-8000-000000000002"
	operatorID := "10000000-0000-4000-8000-000000000003"
	grant := domain.MembershipGrant{Issuer: member.Issuer, Subject: member.Subject, DisplayName: "Member", Role: domain.RoleViewer}
	viewer, err := store.PutMembershipGrant(ctx, admin, "research", viewerID, grant)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.PutMembershipGrant(ctx, admin, "research", viewerID, grant)
	if err != nil || replay.ID != viewer.ID || !replay.CreatedAt.Equal(viewer.CreatedAt) {
		t.Fatalf("grant replay = %#v, %v", replay, err)
	}
	grant.Role = domain.RoleSubmitter
	if _, err = store.PutMembershipGrant(ctx, admin, "research", viewerID, grant); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed replay = %v", err)
	}
	if _, err = store.PutMembershipGrant(ctx, admin, "research", submitterID, grant); err != nil {
		t.Fatal(err)
	}
	grant.Role = domain.RoleOperator
	if _, err = store.PutMembershipGrant(ctx, admin, "research", operatorID, grant); err != nil {
		t.Fatal(err)
	}
	access, err := store.CurrentPrincipal(ctx, member, "", 50)
	if err != nil || len(access.Namespaces) != 1 || access.Namespaces[0].Name != "research" || !slices.Equal(access.Namespaces[0].Roles, []string{domain.RoleOperator, domain.RoleSubmitter, domain.RoleViewer}) {
		t.Fatalf("discovery = %#v, %v", access, err)
	}
	version, err := strconv.ParseInt(access.Namespaces[0].AuthorizationVersion, 10, 64)
	if err != nil || version != 3 {
		t.Fatalf("authorization version = %d, %v", version, err)
	}
	if _, err = store.CreateTarget(ctx, admin, "research", "grant-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}, LogStoreName: "department-nfs", LogStoreVersion: 1}); err != nil {
		t.Fatal(err)
	}
	created, err := store.SubmitJob(ctx, admin, "grant-job", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListJobs(ctx, member, "research", domain.JobListOptions{Limit: 50})
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].ID != created.Job.ID {
		t.Fatalf("duplicate job rows: %#v, %v", page, err)
	}
	targets, err := store.ListTargets(ctx, member, "research")
	if err != nil || len(targets) != 1 {
		t.Fatalf("duplicate target rows: %#v, %v", targets, err)
	}
	if _, err = store.GetJob(ctx, member, "other", created.Job.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-namespace read = %v", err)
	}
	if _, err = store.ExportAudit(ctx, member, "research", 0, 50); err != nil {
		t.Fatalf("operator audit = %v", err)
	}
	if _, err = store.RevokeMembershipGrant(ctx, admin, "research", operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ExportAudit(ctx, member, "research", 0, 50); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked operator audit = %v", err)
	}
	if _, err = store.CancelJob(ctx, member, "research", created.Job.ID, "member-cancel", "sha256:"+strings.Repeat("2", 64)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked operator other-owner cancel = %v", err)
	}
	own, err := store.SubmitJob(ctx, member, "member-submit", integrationSubmission(t))
	if err != nil {
		t.Fatalf("remaining submitter = %v", err)
	}
	if _, err = store.RevokeMembershipGrant(ctx, admin, "research", submitterID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SubmitJob(ctx, member, "member-denied", integrationSubmission(t)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("viewer submission = %v", err)
	}
	if _, err = store.CancelJob(ctx, member, "research", own.Job.ID, "own-cancel", "sha256:"+strings.Repeat("3", 64)); err != nil {
		t.Fatalf("retained legacy owner cancellation = %v", err)
	}
	if _, err = store.PutMembershipGrant(ctx, member, "research", "10000000-0000-4000-8000-000000000004", grant); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("viewer admin escalation = %v", err)
	}
	revoked, err := store.RevokeMembershipGrant(ctx, admin, "research", viewerID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke final grant = %#v, %v", revoked, err)
	}
	again, err := store.RevokeMembershipGrant(ctx, admin, "research", viewerID)
	if err != nil || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("revocation replay = %#v, %v", again, err)
	}
	grant.Role = domain.RoleViewer
	if _, err = store.PutMembershipGrant(ctx, admin, "research", viewerID, grant); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("revoked grant resurrection = %v", err)
	}
	if _, err = store.GetJob(ctx, member, "research", created.Job.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("read after final removal = %v", err)
	}
	access, err = store.CurrentPrincipal(ctx, member, "", 50)
	if err != nil || len(access.Namespaces) != 0 {
		t.Fatalf("discovery after removal = %#v, %v", access, err)
	}
	first, err := store.CurrentPrincipal(ctx, admin, "", 1)
	if err != nil || len(first.Namespaces) != 1 || first.NextNamespaceID == "" {
		t.Fatalf("first namespace page = %#v, %v", first, err)
	}
	second, err := store.CurrentPrincipal(ctx, admin, first.NextNamespaceID, 1)
	if err != nil || len(second.Namespaces) != 1 || second.NextNamespaceID != "" || first.Namespaces[0].ID == second.Namespaces[0].ID {
		t.Fatalf("second namespace page = %#v, %v", second, err)
	}
}

func TestMembershipGrantMigrationIntegration(t *testing.T) {
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
	if err = migrate(ctx, pool, migrations[:12]); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	admin := domain.Principal{Issuer: "test-issuer", Subject: "test-subject"}
	if err = store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: admin, DisplayName: "Admin", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatalf("migration replay = %v", err)
	}
	access, err := store.CurrentPrincipal(ctx, admin, "", 50)
	if err != nil || len(access.Namespaces) != 1 || access.Namespaces[0].AuthorizationVersion != "1" {
		t.Fatalf("migrated discovery = %#v, %v", access, err)
	}
	if err = compareMigrations(migrations[:12], map[string]string{migrations[12].version: migrations[12].checksum}); err == nil {
		t.Fatal("old binary accepted newer grants schema")
	}
	// The original single-role PUT modifies only the legacy contribution.
	grant := domain.MembershipGrant{Issuer: admin.Issuer, Subject: admin.Subject, DisplayName: "Admin", Role: domain.RoleOperator}
	if _, err = store.PutMembershipGrant(ctx, admin, "research", "20000000-0000-4000-8000-000000000001", grant); err != nil {
		t.Fatal(err)
	}
	grant.Role = domain.RoleViewer
	if _, err = store.PutMembership(ctx, admin, "research", "legacy-replace", "sha256:"+strings.Repeat("4", 64), grant); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, admin, "", 50)
	if err != nil || !slices.Equal(access.Namespaces[0].Roles, []string{domain.RoleOperator, domain.RoleViewer}) {
		t.Fatalf("legacy replacement erased contribution: %#v, %v", access, err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM memberships WHERE namespace_id = $1 AND principal_id = $2`, access.Namespaces[0].ID, access.PrincipalID); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, admin, "", 50)
	if err != nil || !slices.Equal(access.Namespaces[0].Roles, []string{domain.RoleOperator}) {
		t.Fatalf("legacy deletion erased contribution: %#v, %v", access, err)
	}
}
