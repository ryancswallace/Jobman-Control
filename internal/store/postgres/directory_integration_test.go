package postgres

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDirectoryReconciliationIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	owner := domain.Principal{Issuer: "test-issuer", Subject: "preserved-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: owner, DisplayName: "Synthetic owner", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	access, err := store.CurrentPrincipal(ctx, owner, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	namespaceID, principalID := access.Namespaces[0].ID, access.PrincipalID
	directoryID := "11111111-1111-4111-8111-111111111111"
	viewerGroup := "22222222-2222-4222-8222-222222222222"
	adminGroup := "33333333-3333-4333-8333-333333333333"
	alias := domain.Principal{Issuer: "https://adfs.example.test/adfs", Subject: "dashboard-subject"}
	mapping := domain.DirectoryMapping{SourceID: "test-directory", Revision: 1, Namespaces: []string{namespaceID}, Bindings: []domain.DirectoryBinding{{GroupID: viewerGroup, NamespaceID: namespaceID, Role: domain.RoleViewer}, {GroupID: adminGroup, NamespaceID: namespaceID, Role: domain.RoleNamespaceAdmin}}, Identities: []domain.DirectoryIdentity{{DirectoryID: directoryID, PrincipalID: principalID, Issuer: owner.Issuer, Subject: owner.Subject, DisplayName: "Synthetic owner", Aliases: []domain.DirectoryAlias{{Issuer: alias.Issuer, Subject: alias.Subject}}}}}
	plan, err := store.PlanDirectory(ctx, mapping)
	if err != nil || plan.NewManagedNamespaces != 1 || plan.RetainedNonDirectoryGrants != 1 {
		t.Fatalf("directory preview=%#v,%v", plan, err)
	}
	if err = store.ConfigureDirectory(ctx, mapping); err == nil {
		t.Fatal("transition allowed without explicit operator approval")
	}
	var managed int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM namespace_directory_state`).Scan(&managed); err != nil || managed != 0 {
		t.Fatalf("preview/failed transition wrote authority=%d,%v", managed, err)
	}
	mapping.ApprovedTransitions = []string{namespaceID}
	if err = store.ConfigureDirectory(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	var aliases int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM principal_aliases`).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatalf("unverified aliases provisioned=%d,%v", aliases, err)
	}
	newSnapshot := func() domain.DirectorySnapshot {
		t.Helper()
		digest, digestErr := directory.Digest(mapping)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		epoch, epochErr := store.DirectoryRecoveryEpoch(ctx)
		if epochErr != nil {
			t.Fatal(epochErr)
		}
		var now time.Time
		if clockErr := pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); clockErr != nil {
			t.Fatal(clockErr)
		}
		groups := make([]domain.DirectoryGroupObservation, 0, len(mapping.Bindings))
		for _, binding := range mapping.Bindings {
			groups = append(groups, domain.DirectoryGroupObservation{GroupID: binding.GroupID, Exists: true, DirectoryIDs: []string{directoryID}})
		}
		return domain.DirectorySnapshot{SourceID: mapping.SourceID, Revision: mapping.Revision, Digest: digest, RecoveryEpoch: epoch, VerifiedAt: now, Accounts: []domain.DirectoryAccountObservation{{DirectoryID: directoryID, Exists: true, Enabled: true}}, Groups: groups}
	}
	full := newSnapshot()
	if err = store.ApplyDirectorySnapshot(ctx, full); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, alias, "", 50)
	if err != nil || access.PrincipalID != principalID || access.DirectoryID != directoryID || len(access.Namespaces) != 1 || !slices.Equal(access.Namespaces[0].Roles, []string{domain.RoleNamespaceAdmin, domain.RoleViewer}) || access.Namespaces[0].AuthorizationStatus != "verified" {
		t.Fatalf("verified union=%#v,%v", access, err)
	}
	version := access.Namespaces[0].AuthorizationVersion
	if err = store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: owner, DisplayName: "Synthetic owner", Namespace: "research"}); err != nil {
		t.Fatalf("canonical bootstrap compatibility after alias provisioning=%v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,display_name) VALUES('66666666-6666-4666-8666-666666666666',$1,$2,'conflict')`, alias.Issuer, alias.Subject); err == nil {
		t.Fatal("ordinary principal creation took over verified alias")
	}
	if _, err = store.PutMembershipGrant(ctx, alias, "research", "44444444-4444-4444-8444-444444444444", domain.MembershipGrant{Issuer: owner.Issuer, Subject: owner.Subject, DisplayName: "Synthetic owner", Role: domain.RoleViewer}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("manual grant in managed namespace=%v", err)
	}
	partial := newSnapshot()
	partial.Groups = partial.Groups[:1]
	if err = store.ApplyDirectorySnapshot(ctx, partial); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	if err = store.RecordDirectoryFailure(ctx, mapping.SourceID, mapping.Revision); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, alias, "", 50)
	if err != nil || access.Namespaces[0].AuthorizationVersion != version || !access.Namespaces[0].LastDirectoryVerifiedAt.Equal(full.VerifiedAt) {
		t.Fatalf("failure changed grants/proof=%#v,%v", access, err)
	}
	one := newSnapshot()
	one.Groups[1].DirectoryIDs = nil
	if err = store.ApplyDirectorySnapshot(ctx, one); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, alias, "", 50)
	if err != nil || !slices.Equal(access.Namespaces[0].Roles, []string{domain.RoleViewer}) || access.Namespaces[0].AuthorizationVersion == version {
		t.Fatalf("one group removal lost union semantics=%#v,%v", access, err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, full); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("out-of-order prior snapshot accepted=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE namespace_directory_state SET last_verified_at=statement_timestamp()-interval '121 seconds'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CurrentPrincipal(ctx, alias, "", 50); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("outage did not expire proof=%v", err)
	}
	removed := newSnapshot()
	for i := range removed.Groups {
		removed.Groups[i].Exists = false
		removed.Groups[i].DirectoryIDs = nil
	}
	if err = store.ApplyDirectorySnapshot(ctx, removed); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, owner, "", 50)
	if err != nil || len(access.Namespaces) != 0 {
		t.Fatalf("legacy grant survived complete group deletion=%#v,%v", access, err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, newSnapshot()); err != nil {
		t.Fatal(err)
	}
	disabled := newSnapshot()
	disabled.Accounts[0].Enabled = false
	if err = store.ApplyDirectorySnapshot(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, owner, "", 50)
	if err != nil || len(access.Namespaces) != 0 {
		t.Fatalf("disabled user retained roles=%#v,%v", access, err)
	}
	beforeRestore := newSnapshot()
	if _, err = pool.Exec(ctx, `UPDATE service_recovery_state SET restore_epoch=restore_epoch+1`); err != nil {
		t.Fatal(err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, beforeRestore); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("pre-recovery directory read accepted=%v", err)
	}
	beforeChange := newSnapshot()
	mapping.Revision = 2
	mapping.Bindings = mapping.Bindings[:1]
	if err = store.ConfigureDirectory(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, beforeChange); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("prior config snapshot accepted=%v", err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, newSnapshot()); err != nil {
		t.Fatal(err)
	}
	access, err = store.CurrentPrincipal(ctx, alias, "", 50)
	if err != nil || len(access.Namespaces) != 1 || !slices.Equal(access.Namespaces[0].Roles, []string{domain.RoleViewer}) {
		t.Fatalf("configured group removal=%#v,%v", access, err)
	}
	if err = store.ConfigureDirectory(ctx, mapping); err != nil {
		t.Fatalf("identical config retry=%v", err)
	}
	conflict := mapping
	conflict.Revision = 3
	conflict.Identities = slices.Clone(mapping.Identities)
	conflict.Identities[0].PrincipalID = "55555555-5555-4555-8555-555555555555"
	if _, err = store.PlanDirectory(ctx, conflict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("principal merge conflict=%v", err)
	}
	var auditCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM directory_audit_events`).Scan(&auditCount); err != nil || auditCount < 5 {
		t.Fatalf("directory audit missing=%d,%v", auditCount, err)
	}
}
