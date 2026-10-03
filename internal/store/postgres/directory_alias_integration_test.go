package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDirectoryAdoptsOnlyDeclaredLegacyAliasesIntegration(t *testing.T) {
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
	owner := domain.Principal{Issuer: "synthetic-issuer", Subject: "canonical"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: owner, DisplayName: "Synthetic", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	access, err := store.CurrentPrincipal(ctx, owner, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	principalID, namespaceID := access.PrincipalID, access.Namespaces[0].ID
	directoryID := "11111111-1111-4111-8111-111111111111"
	groupID := "22222222-2222-4222-8222-222222222222"
	if _, err = pool.Exec(ctx, `INSERT INTO directory_accounts(directory_id,principal_id,enabled,last_verified_at) VALUES($1,$2,true,statement_timestamp())`, directoryID, principalID); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"kept-alias", "omitted-alias"} {
		if _, err = pool.Exec(ctx, `INSERT INTO principal_aliases(issuer,subject,directory_id,principal_id,provenance) VALUES($1,$2,$3,$4,'operator')`, owner.Issuer, subject, directoryID, principalID); err != nil {
			t.Fatal(err)
		}
	}
	mapping := domain.DirectoryMapping{SourceID: "synthetic-directory", Revision: 1, Namespaces: []string{namespaceID}, ApprovedTransitions: []string{namespaceID}, Bindings: []domain.DirectoryBinding{{GroupID: groupID, NamespaceID: namespaceID, Role: domain.RoleViewer}}, Identities: []domain.DirectoryIdentity{{DirectoryID: directoryID, PrincipalID: principalID, Issuer: owner.Issuer, Subject: owner.Subject, DisplayName: "Synthetic", Aliases: []domain.DirectoryAlias{{Issuer: owner.Issuer, Subject: "kept-alias"}}}}}
	fingerprint := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	key := domain.DelegationKey{ServiceID: "dashboard", KeyID: "synthetic", Audience: "control", PublicKey: make([]byte, 32), CertificateThumbprints: []string{fingerprint}, NamespaceIDs: []string{namespaceID}, Operations: []string{domain.CapabilityNamespaceRead}, Enabled: true}
	if err = store.RegisterDelegationKeys(ctx, []domain.DelegationKey{key}); err != nil {
		t.Fatal(err)
	}
	apply := func() {
		t.Helper()
		if applyErr := store.ConfigureDirectory(ctx, mapping); applyErr != nil {
			t.Fatal(applyErr)
		}
		digest, digestErr := directory.Digest(mapping)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		var now time.Time
		if clockErr := pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); clockErr != nil {
			t.Fatal(clockErr)
		}
		if applyErr := store.ApplyDirectorySnapshot(ctx, domain.DirectorySnapshot{SourceID: mapping.SourceID, Revision: mapping.Revision, Digest: digest, RecoveryEpoch: 1, VerifiedAt: now, Accounts: []domain.DirectoryAccountObservation{{DirectoryID: directoryID, Exists: true, Enabled: true}}, Groups: []domain.DirectoryGroupObservation{{GroupID: groupID, Exists: true, DirectoryIDs: []string{directoryID}}}}); applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	assertAccess := func(subject string, allowed bool) {
		t.Helper()
		actor := domain.Principal{Issuer: owner.Issuer, Subject: subject}
		ordinary, readErr := store.CurrentPrincipal(ctx, actor, "", 50)
		if readErr != nil || (len(ordinary.Namespaces) > 0) != allowed {
			t.Fatalf("ordinary alias %s access=%#v,%v", subject, ordinary, readErr)
		}
		var now time.Time
		if clockErr := pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); clockErr != nil {
			t.Fatal(clockErr)
		}
		actor.Delegation = &domain.DelegatedActor{ServiceID: key.ServiceID, KeyID: key.KeyID, Audience: key.Audience, CertificateThumbprint: fingerprint, DirectoryID: directoryID, Operation: domain.CapabilityNamespaceRead, NamespaceIDs: []string{namespaceID}, Mode: "interactive", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
		delegated, readErr := store.CurrentPrincipal(ctx, actor, "", 50)
		if allowed {
			if readErr != nil || len(delegated.Namespaces) != 1 {
				t.Fatalf("declared delegated alias=%#v,%v", delegated, readErr)
			}
		} else if !errors.Is(readErr, domain.ErrForbidden) {
			t.Fatalf("undeclared delegated alias remained valid=%#v,%v", delegated, readErr)
		}
	}
	apply()
	assertAccess("omitted-alias", false)
	assertAccess("kept-alias", true)
	mapping.Revision++
	mapping.Identities[0].Aliases = nil
	apply()
	assertAccess("omitted-alias", false)
	assertAccess("kept-alias", false)
	assertAccess(owner.Subject, true)
}
