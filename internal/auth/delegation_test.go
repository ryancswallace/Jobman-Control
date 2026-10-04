package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	delegationNamespace = "11111111-1111-4111-8111-111111111111"
	delegationDirectory = "22222222-2222-4222-8222-222222222222"
)

type testDelegationRegistry struct {
	key       domain.DelegationKey
	lookupErr error
	acceptErr error
	accepted  []domain.Principal
	seen      map[string]bool
}

func (registry *testDelegationRegistry) DelegationKey(_ context.Context, serviceID, keyID string) (domain.DelegationKey, error) {
	if registry.lookupErr != nil {
		return domain.DelegationKey{}, registry.lookupErr
	}
	if serviceID != registry.key.ServiceID || keyID != registry.key.KeyID {
		return domain.DelegationKey{}, domain.ErrNotFound
	}
	return registry.key, nil
}

func (registry *testDelegationRegistry) AcceptDelegationAssertion(_ context.Context, principal domain.Principal) error {
	if registry.acceptErr != nil {
		return registry.acceptErr
	}
	if registry.seen[principal.Delegation.AssertionID] {
		return domain.ErrUnauthenticated
	}
	registry.seen[principal.Delegation.AssertionID] = true
	registry.accepted = append(registry.accepted, principal)
	return nil
}

type delegationFixture struct {
	now        time.Time
	private    ed25519.PrivateKey
	registry   *testDelegationRegistry
	connection *tls.ConnectionState
	claims     delegationClaims
}

func newDelegationFixture(t *testing.T) delegationFixture {
	t.Helper()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	thumbprint := base64.RawURLEncoding.EncodeToString(sum[:])
	key := domain.DelegationKey{ServiceID: "dashboard", KeyID: "key-1", Audience: "control-instance", PublicKey: public, CertificateThumbprints: []string{thumbprint}, NamespaceIDs: []string{delegationNamespace}, Operations: []string{domain.CapabilityJobsRead}, Enabled: true}
	claims := delegationClaims{Claims: jwt.Claims{Issuer: key.ServiceID, Subject: delegationDirectory, Audience: jwt.Audience{key.Audience}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Minute)), ID: base64.RawURLEncoding.EncodeToString(make([]byte, 24))}, Confirmation: map[string]string{"x5t#S256": thumbprint}, Actor: &delegationIdentity{DirectoryID: delegationDirectory, Issuer: "https://adfs.example.test/adfs", Subject: "stable-subject"}, Operation: domain.CapabilityJobsRead, NamespaceIDs: []string{delegationNamespace}, Mode: "interactive"}
	return delegationFixture{now: now, private: private, registry: &testDelegationRegistry{key: key, seen: map[string]bool{}}, connection: &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}, claims: claims}
}

func (fixture delegationFixture) token(t *testing.T, mutate func(*jose.SignerOptions)) string {
	t.Helper()
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", fixture.registry.key.KeyID)
	if mutate != nil {
		mutate(options)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: fixture.private}, options)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := jwt.Signed(signer).Claims(fixture.claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return DelegationScheme + " " + compact
}

func (fixture delegationFixture) authenticate(t *testing.T, authorization, operation string) (domain.Principal, error) {
	t.Helper()
	authenticator := DelegationAuthenticator{Registry: fixture.registry, Now: func() time.Time { return fixture.now }}
	return authenticator.Authenticate(t.Context(), authorization, fixture.connection, operation)
}

func TestDelegationAuthenticatesBoundActorAndRejectsReplay(t *testing.T) {
	t.Parallel()
	fixture := newDelegationFixture(t)
	token := fixture.token(t, nil)
	principal, err := fixture.authenticate(t, token, domain.CapabilityJobsRead)
	if err != nil || principal.Issuer != fixture.claims.Actor.Issuer || principal.Subject != fixture.claims.Actor.Subject || principal.Delegation == nil || principal.Delegation.ServiceID != "dashboard" || principal.Delegation.DirectoryID != delegationDirectory || principal.Delegation.AssertionDigest == "" || len(fixture.registry.accepted) != 1 {
		t.Fatalf("delegated actor=%#v,%v", principal, err)
	}
	if _, err = fixture.authenticate(t, token, domain.CapabilityJobsRead); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("replayed assertion=%v", err)
	}
	fixture.registry.key.Enabled = false
	if _, err = fixture.authenticate(t, token, domain.CapabilityJobsRead); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("disabled service=%v", err)
	}
}

func TestDelegationRejectsInvalidClaimsAndTransport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*delegationFixture)
	}{
		{"missing-tls", func(f *delegationFixture) { f.connection = nil }},
		{"unverified-peer", func(f *delegationFixture) { f.connection.VerifiedChains = nil }},
		{"incomplete-handshake", func(f *delegationFixture) { f.connection.HandshakeComplete = false }},
		{"wrong-binding", func(f *delegationFixture) { f.claims.Confirmation["x5t#S256"] = "other" }},
		{"unregistered-certificate", func(f *delegationFixture) { f.registry.key.CertificateThumbprints = []string{"other"} }},
		{"wrong-signature", func(f *delegationFixture) { f.registry.key.PublicKey = make([]byte, 32) }},
		{"wrong-audience", func(f *delegationFixture) { f.claims.Audience = jwt.Audience{"another"} }},
		{"multiple-audiences", func(f *delegationFixture) { f.claims.Audience = append(f.claims.Audience, "another") }},
		{"long-lifetime", func(f *delegationFixture) { f.claims.Expiry = jwt.NewNumericDate(f.now.Add(61 * time.Second)) }},
		{"expired", func(f *delegationFixture) { f.now = f.now.Add(66 * time.Second) }},
		{"future-issued", func(f *delegationFixture) {
			f.claims.IssuedAt = jwt.NewNumericDate(f.now.Add(6 * time.Second))
			f.claims.NotBefore = f.claims.IssuedAt
		}},
		{"future-not-before", func(f *delegationFixture) { f.claims.NotBefore = jwt.NewNumericDate(f.now.Add(6 * time.Second)) }},
		{"missing-issued", func(f *delegationFixture) { f.claims.IssuedAt = nil }},
		{"missing-expiry", func(f *delegationFixture) { f.claims.Expiry = nil }},
		{"missing-not-before", func(f *delegationFixture) { f.claims.NotBefore = nil }},
		{"short-jti", func(f *delegationFixture) { f.claims.ID = base64.RawURLEncoding.EncodeToString([]byte("short")) }},
		{"invalid-jti", func(f *delegationFixture) { f.claims.ID = "spaces are invalid" }},
		{"mismatched-actor", func(f *delegationFixture) { f.claims.Subject = delegationNamespace }},
		{"invalid-directory-id", func(f *delegationFixture) {
			f.claims.Actor.DirectoryID = "an-email@example.test"
			f.claims.Subject = f.claims.Actor.DirectoryID
		}},
		{"missing-issuer", func(f *delegationFixture) { f.claims.Actor.Issuer = "" }},
		{"missing-subject", func(f *delegationFixture) { f.claims.Actor.Subject = "" }},
		{"wrong-operation", func(f *delegationFixture) { f.claims.Operation = domain.CapabilityLogsRead }},
		{"unregistered-operation", func(f *delegationFixture) { f.registry.key.Operations = []string{domain.CapabilityLogsRead} }},
		{"empty-scope", func(f *delegationFixture) { f.claims.NamespaceIDs = nil }},
		{"foreign-scope", func(f *delegationFixture) { f.claims.NamespaceIDs = []string{delegationDirectory} }},
		{"duplicate-scope", func(f *delegationFixture) { f.claims.NamespaceIDs = append(f.claims.NamespaceIDs, delegationNamespace) }},
		{"bad-mode", func(f *delegationFixture) { f.claims.Mode = "administrator" }},
		{"certificate-expired", func(f *delegationFixture) { f.connection.PeerCertificates[0].NotAfter = f.now }},
		{"assertion-outlives-certificate", func(f *delegationFixture) { f.connection.PeerCertificates[0].NotAfter = f.now.Add(30 * time.Second) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newDelegationFixture(t)
			test.change(&fixture)
			_, err := fixture.authenticate(t, fixture.token(t, nil), domain.CapabilityJobsRead)
			if !errors.Is(err, domain.ErrUnauthenticated) || len(fixture.registry.accepted) != 0 {
				t.Fatalf("invalid delegation accepted: %v", err)
			}
		})
	}
}

func TestDelegationRejectsWrongTypeAndMutationRoutes(t *testing.T) {
	t.Parallel()
	fixture := newDelegationFixture(t)
	for _, operation := range []string{"", domain.CapabilityJobsSubmit, domain.CapabilityJobsCancelOwn, domain.CapabilityMembershipsManage, domain.CapabilityPolicyManage, "events.read"} {
		if _, err := fixture.authenticate(t, fixture.token(t, nil), operation); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Fatalf("operation %q accepted: %v", operation, err)
		}
	}
	token := fixture.token(t, func(options *jose.SignerOptions) { options.WithType("at+jwt") })
	if _, err := fixture.authenticate(t, token, domain.CapabilityJobsRead); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("wrong token type=%v", err)
	}
	fixture.registry.lookupErr = errors.New("database disconnected")
	if _, err := fixture.authenticate(t, fixture.token(t, nil), domain.CapabilityJobsRead); !errors.Is(err, domain.ErrAuthorizationUnavailable) {
		t.Fatalf("registry outage=%v", err)
	}
	fixture.registry.lookupErr = nil
	fixture.registry.acceptErr = domain.ErrForbidden
	if _, err := fixture.authenticate(t, fixture.token(t, nil), domain.CapabilityJobsRead); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unverified alias=%v", err)
	}
}

func TestDelegationAllowsExplicitFiveSecondSkew(t *testing.T) {
	t.Parallel()
	fixture := newDelegationFixture(t)
	fixture.now = fixture.now.Add(64 * time.Second)
	if _, err := fixture.authenticate(t, fixture.token(t, nil), domain.CapabilityJobsRead); err != nil {
		t.Fatalf("bounded expiry skew=%v", err)
	}
}

func TestMonitoringServiceAssertionHasSeparateIdentityShape(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		mutate    func(*delegationFixture)
		operation string
		ok        bool
	}{
		{name: "service", operation: domain.CapabilityEventsRead, ok: true},
		{name: "actor", operation: domain.CapabilityEventsRead, mutate: func(f *delegationFixture) {
			f.claims.Actor = &delegationIdentity{DirectoryID: delegationDirectory, Issuer: "issuer", Subject: "subject"}
		}},
		{name: "interactive", operation: domain.CapabilityEventsRead, mutate: func(f *delegationFixture) { f.claims.Mode = "interactive" }},
		{name: "subject", operation: domain.CapabilityEventsRead, mutate: func(f *delegationFixture) { f.claims.Subject = delegationDirectory }},
		{name: "wrong route", operation: domain.CapabilityJobsRead},
		{name: "no registration", operation: domain.CapabilityEventsRead, mutate: func(f *delegationFixture) { f.registry.key.Operations = []string{domain.CapabilityJobsRead} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDelegationFixture(t)
			f.registry.key.Operations = []string{domain.CapabilityEventsRead}
			f.claims.Actor = nil
			f.claims.Subject = f.claims.Issuer
			f.claims.Operation = domain.CapabilityEventsRead
			f.claims.Mode = "worker"
			if test.mutate != nil {
				test.mutate(&f)
			}
			principal, err := f.authenticate(t, f.token(t, nil), test.operation)
			if test.ok {
				if err != nil || principal.Delegation == nil || !principal.Delegation.ServiceOnly || principal.Issuer != "" || principal.Subject != "" || principal.Delegation.DirectoryID != "" {
					t.Fatalf("service principal=%#v,%v", principal, err)
				}
			} else if err == nil {
				t.Fatal("invalid service assertion accepted")
			}
		})
	}
}

func TestMonitoringServiceRejectsExplicitNullActor(t *testing.T) {
	t.Parallel()
	f := newDelegationFixture(t)
	f.registry.key.Operations = []string{domain.CapabilityEventsRead}
	f.claims.Actor = nil
	f.claims.Subject = f.claims.Issuer
	f.claims.Mode = "worker"
	f.claims.Operation = domain.CapabilityEventsRead
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.private}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", f.registry.key.KeyID))
	if err != nil {
		t.Fatal(err)
	}
	compact, err := jwt.Signed(signer).Claims(f.claims).Claims(map[string]any{"actor": nil}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.authenticate(t, DelegationScheme+" "+compact, domain.CapabilityEventsRead); err == nil {
		t.Fatal("explicit null actor accepted as service-only")
	}
}
