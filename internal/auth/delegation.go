package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	// DelegationScheme is distinct from OIDC bearer authentication.
	DelegationScheme       = "Jobman-Delegation"
	maximumDelegationBytes = 32768
	delegationClockSkew    = 5 * time.Second
)

type (
	delegationIdentity struct {
		DirectoryID string `json:"directoryId"`
		Issuer      string `json:"issuer"`
		Subject     string `json:"subject"`
	}
	delegationClaims struct {
		jwt.Claims
		Confirmation map[string]string  `json:"cnf"`
		Actor        delegationIdentity `json:"actor"`
		Operation    string             `json:"operation"`
		NamespaceIDs []string           `json:"namespaceIds"`
		Mode         string             `json:"mode"`
	}
)

// DelegationAuthenticator verifies pinned Ed25519 assertions against a current
// registry and the actual TLS peer. Proxy certificate headers are never used.
type DelegationAuthenticator struct {
	Registry domain.DelegationRegistry
	Now      func() time.Time
}

// Authenticate verifies one exact allowed read route and atomically consumes
// the assertion before returning a service actor. Authorization is still
// performed by every repository read using current grants and directory state.
func (authenticator DelegationAuthenticator) Authenticate(ctx context.Context, authorization string, connection *tls.ConnectionState, operation string) (domain.Principal, error) {
	scheme, compact, found := strings.Cut(authorization, " ")
	if !found || !strings.EqualFold(scheme, DelegationScheme) || compact == "" || len(compact) > maximumDelegationBytes || strings.ContainsAny(compact, " \t\r\n") || strings.Count(compact, ".") != 2 || !domain.DelegationReadOperation(operation) || authenticator.Registry == nil {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if connection == nil || !connection.HandshakeComplete || len(connection.PeerCertificates) == 0 || len(connection.VerifiedChains) == 0 {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	token, err := jwt.ParseSigned(compact, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(token.Headers) != 1 {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	header := token.Headers[0]
	if header.Algorithm != string(jose.EdDSA) || header.KeyID == "" || len(header.KeyID) > 128 || header.ExtraHeaders[jose.HeaderType] != "JWT" {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	// Unverified issuer is used only as a bounded database lookup key. It confers
	// no identity or authority until signature and all registered policy checks pass.
	var selector jwt.Claims
	if err = token.UnsafeClaimsWithoutVerification(&selector); err != nil || selector.Issuer == "" || len(selector.Issuer) > 128 {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	key, err := authenticator.Registry.DelegationKey(ctx, selector.Issuer, header.KeyID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrUnauthenticated) {
			return domain.Principal{}, domain.ErrUnauthenticated
		}
		return domain.Principal{}, domain.ErrAuthorizationUnavailable
	}
	if !key.Enabled || key.ServiceID != selector.Issuer || key.KeyID != header.KeyID || len(key.PublicKey) != ed25519.PublicKeySize {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	var claims delegationClaims
	if err = token.Claims(ed25519.PublicKey(key.PublicKey), &claims); err != nil {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	now := time.Now().UTC()
	if authenticator.Now != nil {
		now = authenticator.Now().UTC()
	}
	leaf := connection.PeerCertificates[0]
	certificateHash := sha256.Sum256(leaf.Raw)
	thumbprint := base64.RawURLEncoding.EncodeToString(certificateHash[:])
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || claims.Confirmation["x5t#S256"] != thumbprint || !slices.Contains(key.CertificateThumbprints, thumbprint) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if !validDelegationClaims(claims, key, now, operation, leaf.NotAfter) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(compact))
	principal := domain.Principal{Issuer: claims.Actor.Issuer, Subject: claims.Actor.Subject, Delegation: &domain.DelegatedActor{
		Audience: key.Audience, ServiceID: claims.Issuer, KeyID: header.KeyID, CertificateThumbprint: thumbprint, DirectoryID: claims.Actor.DirectoryID,
		Operation: claims.Operation, NamespaceIDs: slices.Clone(claims.NamespaceIDs), Mode: claims.Mode, AssertionID: claims.ID,
		AssertionDigest: base64.RawURLEncoding.EncodeToString(digest[:]), IssuedAt: claims.IssuedAt.Time(), ExpiresAt: claims.Expiry.Time(),
	}}
	if acceptErr := authenticator.Registry.AcceptDelegationAssertion(ctx, principal); acceptErr != nil {
		if errors.Is(acceptErr, domain.ErrForbidden) || errors.Is(acceptErr, domain.ErrUnauthenticated) || errors.Is(acceptErr, domain.ErrAuthorizationUnavailable) {
			return domain.Principal{}, acceptErr
		}
		return domain.Principal{}, domain.ErrAuthorizationUnavailable
	}
	return principal, nil
}

func validDelegationClaims(claims delegationClaims, key domain.DelegationKey, now time.Time, operation string, certificateExpires time.Time) bool {
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.Expiry == nil || claims.Subject != claims.Actor.DirectoryID || !domain.IsID(claims.Actor.DirectoryID) || claims.Actor.Issuer == "" || len(claims.Actor.Issuer) > 512 || claims.Actor.Subject == "" || len(claims.Actor.Subject) > 512 {
		return false
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != key.Audience || claims.Operation != operation || !slices.Contains(key.Operations, operation) || !slices.Contains([]string{"interactive", "worker"}, claims.Mode) {
		return false
	}
	issued, notBefore, expires := claims.IssuedAt.Time(), claims.NotBefore.Time(), claims.Expiry.Time()
	if !issued.Before(expires) || expires.Sub(issued) > time.Minute || notBefore.Before(issued.Add(-delegationClockSkew)) || !notBefore.Before(expires) || expires.After(certificateExpires) {
		return false
	}
	if claims.ValidateWithLeeway(jwt.Expected{Issuer: key.ServiceID, AnyAudience: jwt.Audience{key.Audience}, Time: now}, delegationClockSkew) != nil {
		return false
	}
	random, err := base64.RawURLEncoding.DecodeString(claims.ID)
	if err != nil || len(random) < 16 || len(random) > 64 {
		return false
	}
	if len(claims.NamespaceIDs) == 0 || len(claims.NamespaceIDs) > 320 {
		return false
	}
	seen := make(map[string]bool, len(claims.NamespaceIDs))
	for _, id := range claims.NamespaceIDs {
		if !domain.IsID(id) || seen[id] || !slices.Contains(key.NamespaceIDs, id) {
			return false
		}
		seen[id] = true
	}
	return true
}
