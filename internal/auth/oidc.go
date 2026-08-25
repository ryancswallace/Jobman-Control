package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type oidcTokenVerifier interface {
	Verify(context.Context, string) (*oidc.IDToken, error)
}

// OIDCAuthenticator validates signed, audience-bound OIDC bearer JWTs.
type OIDCAuthenticator struct {
	verifier oidcTokenVerifier
}

// DiscoverOIDC performs issuer discovery and constructs a verifier. Discovery
// is startup I/O and never occurs in a PostgreSQL transaction.
func DiscoverOIDC(
	ctx context.Context,
	issuer string,
	audience string,
	client *http.Client,
) (OIDCAuthenticator, error) {
	if client == nil {
		return OIDCAuthenticator{}, errors.New("OIDC HTTP client is required")
	}
	discoveryContext := oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(discoveryContext, issuer)
	if err != nil {
		return OIDCAuthenticator{}, fmt.Errorf("discover OIDC issuer: %w", err)
	}
	verifierContext := oidc.ClientContext(context.WithoutCancel(ctx), client)

	return OIDCAuthenticator{
		verifier: provider.VerifierContext(verifierContext, &oidc.Config{ClientID: audience}),
	}, nil
}

// Authenticate verifies the token signature, issuer, audience, expiry, and
// stable subject before returning the `(issuer, subject)` identity pair.
func (authenticator OIDCAuthenticator) Authenticate(
	ctx context.Context,
	authorization string,
) (domain.Principal, error) {
	token, valid := bearerToken(authorization)
	if !valid || authenticator.verifier == nil {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	verified, err := authenticator.verifier.Verify(ctx, token)
	if err != nil {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if verified.Issuer == "" || verified.Subject == "" ||
		len(verified.Issuer) > 512 || len(verified.Subject) > 512 {
		return domain.Principal{}, domain.ErrUnauthenticated
	}

	return domain.Principal{Issuer: verified.Issuer, Subject: verified.Subject}, nil
}

func bearerToken(value string) (string, bool) {
	scheme, token, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" ||
		strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}

	return token, true
}
