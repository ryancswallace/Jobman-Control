package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ryancswallace/jobman/diagnostic"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDelegatedAuthorityIntegration(t *testing.T) {
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
	owner := domain.Principal{Issuer: "test-issuer", Subject: "original-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: owner, DisplayName: "Test owner", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(ctx, owner, "research", "delegate-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitJob(ctx, owner, "delegation-job", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	namespaceID, principalID := submitted.Job.NamespaceID, submitted.Job.Owner.ID
	directoryID := "22222222-2222-4222-8222-222222222222"
	groupID := "33333333-3333-4333-8333-333333333333"
	alias := domain.Principal{Issuer: "https://adfs.example.test/adfs", Subject: "verified-dashboard-subject"}
	// Explicit synthetic operator provisioning, independent of any assertion.
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO directory_accounts(directory_id,principal_id,enabled,last_verified_at) VALUES($1,$2,true,statement_timestamp())`, []any{directoryID, principalID}},
		{`INSERT INTO principal_aliases(issuer,subject,directory_id,principal_id,provenance) VALUES($1,$2,$3,$4,'operator')`, []any{alias.Issuer, alias.Subject, directoryID, principalID}},
		{`INSERT INTO namespace_directory_state(namespace_id,source_id,last_verified_at) VALUES($1,'synthetic-directory',statement_timestamp())`, []any{namespaceID}},
		{`INSERT INTO directory_role_bindings(group_id,namespace_id,role) VALUES($1,$2,'namespace_admin')`, []any{groupID, namespaceID}},
		{`INSERT INTO membership_grants(id,namespace_id,principal_id,role,provenance,source_key) VALUES(gen_random_uuid(),$1,$2,'namespace_admin','directory',$3)`, []any{namespaceID, principalID, groupID}},
	} {
		if _, err = pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Time{}
	if err = pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	fingerprint := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	registration := domain.DelegationKey{ServiceID: "dashboard", KeyID: "key-one", Audience: "control-audience", PublicKey: make([]byte, 32), CertificateThumbprints: []string{fingerprint}, NamespaceIDs: []string{namespaceID}, Operations: []string{domain.CapabilityNamespaceRead, domain.CapabilityJobsRead, domain.CapabilityGroupsRead, domain.CapabilityTargetsRead, domain.CapabilityLogsRead, domain.CapabilityArtifactsRead, domain.CapabilityEvidenceRead}, Enabled: true}
	if err = store.RegisterDelegationKeys(ctx, []domain.DelegationKey{registration}); err != nil {
		t.Fatal(err)
	}
	principal := alias
	principal.Delegation = &domain.DelegatedActor{ServiceID: registration.ServiceID, KeyID: registration.KeyID, Audience: registration.Audience, CertificateThumbprint: fingerprint, DirectoryID: directoryID, NamespaceIDs: []string{namespaceID}, Operation: domain.CapabilityJobsRead, Mode: "interactive", AssertionID: base64.RawURLEncoding.EncodeToString(make([]byte, 24)), AssertionDigest: fingerprint, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err = store.AcceptDelegationAssertion(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptDelegationAssertion(ctx, principal); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("replay accepted=%v", err)
	}
	job, err := store.GetJob(ctx, principal, "research", submitted.Job.ID)
	if err != nil || job.Owner.ID != principalID || job.AsOf.IsZero() {
		t.Fatalf("delegated preserved owner/read time=%#v,%v", job, err)
	}
	if job.Execution == nil || job.Execution.Command.Executable == "" {
		t.Fatal("authorized delegated detail lacks execution metadata")
	}
	page, err := store.ListJobs(ctx, principal, "research", domain.JobListOptions{Limit: 1, OwnerPrincipalID: principalID})
	if err != nil || len(page.Jobs) != 1 || page.AsOf.IsZero() || !page.Jobs[0].AsOf.Equal(page.AsOf) {
		t.Fatalf("job snapshot=%#v,%v", page, err)
	}
	exerciseDelegationHTTP(ctx, t, store, registration, principal, job.ID)
	// Alias lookup, grant scope and read route are checked again by repositories.
	if _, err = store.GetJobLogs(ctx, principal, "research", job.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("wrong operation=%v", err)
	}
	principal.Delegation.Operation = domain.CapabilityNamespaceRead
	access, err := store.CurrentPrincipal(ctx, principal, "", 50)
	if err != nil || access.PrincipalID != principalID || access.DirectoryID != directoryID || access.Principal.Subject != alias.Subject || len(access.Namespaces) != 1 || access.Namespaces[0].AuthorizationStatus != "verified" || access.Namespaces[0].AuthorizationExpiresAt == nil || !slices.Contains(access.Namespaces[0].Capabilities, domain.CapabilityMembershipsManage) {
		t.Fatalf("verified discovery=%#v,%v", access, err)
	}
	principal.Delegation.Operation = domain.CapabilityTargetsRead
	catalog, catalogErr := store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 1})
	if catalogErr != nil || catalog.Total != 1 || len(catalog.Items) != 1 || catalog.AuthorizationExpiresAt == nil || catalog.AuthorizationVersion < 1 {
		t.Fatalf("delegated target catalog=%#v,%v", catalog, catalogErr)
	}
	principal.Delegation.Operation = domain.CapabilityJobsRead
	if _, catalogErr = store.GetTargetSnapshot(ctx, principal, "research", catalog.Items[0].ID); !errors.Is(catalogErr, domain.ErrForbidden) {
		t.Fatalf("target detail operation bypass=%v", catalogErr)
	}
	deploymentID := "78000000-0000-4000-8000-000000000001"
	if err = store.EnableDiagnosticSnapshots(deploymentID); err != nil {
		t.Fatal(err)
	}
	source, sourceErr := store.Capabilities(ctx)
	if sourceErr != nil {
		t.Fatal(sourceErr)
	}
	selection := diagnostic.SharedSelection{DeploymentID: deploymentID, ControlInstanceID: source.InstanceID, NamespaceID: namespaceID, JobID: job.ID}
	if _, snapshotErr := store.ReadDiagnosticSnapshot(ctx, principal, "research", selection); !errors.Is(snapshotErr, domain.ErrForbidden) {
		t.Fatalf("diagnostic wrong operation=%v", snapshotErr)
	}
	principal.Delegation.Operation = domain.CapabilityEvidenceRead
	diagnosticSnapshot, snapshotErr := store.ReadDiagnosticSnapshot(ctx, principal, "research", selection)
	if snapshotErr != nil || diagnosticSnapshot.AuthorizationExpiresAt == nil || diagnosticSnapshot.AuthorizationVersion < 1 || diagnosticSnapshot.Snapshot.Job.ID != job.ID {
		t.Fatalf("delegated diagnostic=%#v,%v", diagnosticSnapshot, snapshotErr)
	}
	principal.Delegation.Operation = domain.CapabilityJobsRead
	// Even a represented namespace administrator cannot mutate through delegation.
	if _, manifestErr := store.ListLogChunks(ctx, principal, "research", job.ID, domain.LogChunkOptions{Stream: "stdout", Limit: 1}); !errors.Is(manifestErr, domain.ErrForbidden) {
		t.Fatalf("manifest operation bypass=%v", manifestErr)
	}
	principal.Delegation.Operation = domain.CapabilityLogsRead
	manifest, manifestErr := store.ListLogChunks(ctx, principal, "research", job.ID, domain.LogChunkOptions{Stream: "stdout", Limit: 1})
	if manifestErr != nil || manifest.State != "not_captured" || manifest.ManifestRevision != 0 || manifest.AuthorizationExpiresAt == nil || manifest.AuthorizationVersion < 1 {
		t.Fatalf("delegated absent manifest=%#v,%v", manifest, manifestErr)
	}
	principal.Delegation.Operation = domain.CapabilityJobsRead
	for _, mutate := range []func() error{
		func() error {
			_, e := store.CancelJob(ctx, principal, "research", job.ID, "delegated-cancel", "sha256:"+strings.Repeat("2", 64))
			return e
		},
		func() error {
			_, e := store.SubmitJob(ctx, principal, "delegated-submit", integrationSubmission(t))
			return e
		},
		func() error {
			_, e := store.PutMembershipGrant(ctx, principal, "research", "44444444-4444-4444-8444-444444444444", domain.MembershipGrant{Issuer: alias.Issuer, Subject: alias.Subject, Role: domain.RoleViewer, DisplayName: "Verified user"})
			return e
		},
		func() error {
			_, e := store.CancelGraph(ctx, principal, "research", "55555555-5555-4555-8555-555555555555", "delegated-graph-cancel", "sha256:"+strings.Repeat("3", 64))
			return e
		},
		func() error {
			return store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "test", Namespace: "research"})
		},
	} {
		if e := mutate(); !errors.Is(e, domain.ErrForbidden) {
			t.Fatalf("delegated mutation=%v", e)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE directory_accounts SET last_verified_at=statement_timestamp()-interval '121 seconds' WHERE directory_id=$1`, directoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetJob(ctx, principal, "research", job.ID); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("stale directory account=%v", err)
	}
	principal.Delegation.Operation = domain.CapabilityEvidenceRead
	if _, snapshotErr = store.ReadDiagnosticSnapshot(ctx, principal, "research", selection); !errors.Is(snapshotErr, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("diagnostic stale account=%v", snapshotErr)
	}
	principal.Delegation.Operation = domain.CapabilityJobsRead
	if _, err = pool.Exec(ctx, `UPDATE directory_accounts SET last_verified_at=statement_timestamp() WHERE directory_id=$1`, directoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE namespace_directory_state SET last_verified_at=statement_timestamp()-interval '121 seconds' WHERE namespace_id=$1`, namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetJob(ctx, principal, "research", job.ID); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("stale namespace authority=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE namespace_directory_state SET last_verified_at=statement_timestamp() WHERE namespace_id=$1`, namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE service_recovery_state SET restore_epoch=restore_epoch+1`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetJob(ctx, principal, "research", job.ID); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("restored directory proof remained valid=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE namespace_directory_state SET last_verified_at=statement_timestamp(); UPDATE directory_accounts SET last_verified_at=statement_timestamp()`); err != nil {
		t.Fatal(err)
	}
	// A restored replay ledger can omit a consumed assertion. Re-verifying the
	// directory must not make that still-unexpired pre-recovery assertion usable.
	if _, err = pool.Exec(ctx, `DELETE FROM delegation_assertions`); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptDelegationAssertion(ctx, principal); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("pre-recovery assertion replay after fresh directory proof=%v", err)
	}
	var futureFloor bool
	if err = pool.QueryRow(ctx, `SELECT delegation_issued_after>statement_timestamp() FROM service_recovery_state`).Scan(&futureFloor); err != nil || !futureFloor {
		t.Fatalf("restore did not include future assertion clock skew: %v,%v", futureFloor, err)
	}
	// Simulate the end of the recovery clock-skew interval without a test sleep.
	if _, err = pool.Exec(ctx, `UPDATE service_recovery_state SET delegation_issued_after=statement_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&principal.Delegation.IssuedAt); err != nil {
		t.Fatal(err)
	}
	principal.Delegation.ExpiresAt = principal.Delegation.IssuedAt.Add(time.Minute)
	if err = store.AcceptDelegationAssertion(ctx, principal); err != nil {
		t.Fatalf("new post-recovery assertion=%v", err)
	}

	if _, err = pool.Exec(ctx, `UPDATE membership_grants SET revoked_at=statement_timestamp() WHERE provenance='directory'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetJob(ctx, principal, "research", job.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("legacy grant survived directory removal=%v", err)
	}
	if _, err = store.GetJob(ctx, owner, "research", job.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ordinary client bypassed managed namespace=%v", err)
	}
	principal.Delegation.Operation = domain.CapabilityLogsRead
	if _, err = store.ListLogChunks(ctx, principal, "research", job.ID, domain.LogChunkOptions{Stream: "stdout", Limit: 1}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("bounded logs survived directory removal=%v", err)
	}
	principal.Delegation.Operation = domain.CapabilityNamespaceRead
	empty, err := store.CurrentPrincipal(ctx, principal, "", 50)
	if err != nil || len(empty.Namespaces) != 0 || empty.PrincipalID != principalID {
		t.Fatalf("revoked discovery=%#v,%v", empty, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE directory_accounts SET enabled=false WHERE directory_id=$1`, directoryID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CurrentPrincipal(ctx, principal, "", 50); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("disabled directory user=%v", err)
	}
	if err = store.RegisterDelegationKeys(ctx, nil); err != nil {
		t.Fatal(err)
	}
	key, err := store.DelegationKey(ctx, "dashboard", "key-one")
	if err != nil || key.Enabled {
		t.Fatalf("service disable=%#v,%v", key, err)
	}
	if _, err = inReadTransaction(ctx, pool, func(tx pgx.Tx) (verifiedDirectoryPrincipal, error) {
		return resolveDelegatedPrincipal(ctx, tx, principal)
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("disabled service repository check=%v", err)
	}
	registration.PublicKey[0] = 1
	if err = store.RegisterDelegationKeys(ctx, []domain.DelegationKey{registration}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("key material replaced in place=%v", err)
	}
}
