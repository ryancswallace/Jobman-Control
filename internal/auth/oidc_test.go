package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type fakeVerifier struct {
	token *oidc.IDToken
	err   error
}

func (verifier fakeVerifier) Verify(context.Context, string) (*oidc.IDToken, error) {
	return verifier.token, verifier.err
}

func TestOIDCAuthenticator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		authorization string
		verifier      oidcTokenVerifier
		want          domain.Principal
		wantError     bool
	}{
		{
			name: "valid", authorization: "Bearer signed.jwt.value",
			verifier: fakeVerifier{token: &oidc.IDToken{
				Issuer: "https://issuer.example", Subject: "stable-subject",
			}},
			want: domain.Principal{Issuer: "https://issuer.example", Subject: "stable-subject"},
		},
		{name: "missing", verifier: fakeVerifier{}, wantError: true},
		{name: "wrong scheme", authorization: "Basic value", verifier: fakeVerifier{}, wantError: true},
		{name: "space in token", authorization: "Bearer a b", verifier: fakeVerifier{}, wantError: true},
		{name: "verification", authorization: "Bearer token", verifier: fakeVerifier{err: errors.New("invalid")}, wantError: true},
		{name: "empty subject", authorization: "Bearer token", verifier: fakeVerifier{token: &oidc.IDToken{Issuer: "issuer"}}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			authenticator := OIDCAuthenticator{verifier: test.verifier}
			principal, err := authenticator.Authenticate(t.Context(), test.authorization)
			if test.wantError {
				if !errors.Is(err, domain.ErrUnauthenticated) {
					t.Fatalf("Authenticate() error = %v", err)
				}
				return
			}
			if err != nil || principal != test.want {
				t.Fatalf("Authenticate() = %#v, %v", principal, err)
			}
		})
	}
}
