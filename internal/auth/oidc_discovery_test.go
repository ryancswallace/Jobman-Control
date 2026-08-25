package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDiscoverOIDCVerifiesIssuerAudienceSignatureAndExpiry(t *testing.T) {
	t.Parallel()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	const keyID = "test-key"
	keySet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &privateKey.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}}
	const issuer = "https://identity.example.edu"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var response any
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			response = map[string]any{
				"issuer":                                issuer,
				"jwks_uri":                              issuer + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			}
		case "/keys":
			response = keySet
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header),
				Request: request,
			}, nil
		}
		encoded, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			return nil, encodeErr
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(encoded)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    request,
		}, nil
	})}
	authenticator, err := DiscoverOIDC(t.Context(), issuer, "jobman-control", client)
	if err != nil {
		t.Fatalf("DiscoverOIDC() error = %v", err)
	}
	valid := signTestToken(t, privateKey, keyID, jwt.Claims{
		Issuer: issuer, Subject: "stable-subject", Audience: jwt.Audience{"jobman-control"},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	})
	principal, err := authenticator.Authenticate(t.Context(), "Bearer "+valid)
	if err != nil || principal != (domain.Principal{Issuer: issuer, Subject: "stable-subject"}) {
		t.Fatalf("Authenticate() = %#v, %v", principal, err)
	}
	wrongAudience := signTestToken(t, privateKey, keyID, jwt.Claims{
		Issuer: issuer, Subject: "stable-subject", Audience: jwt.Audience{"different-client"},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	})
	if _, err = authenticator.Authenticate(t.Context(), "Bearer "+wrongAudience); err == nil {
		t.Fatal("Authenticate() accepted the wrong audience")
	}
}

func signTestToken(t *testing.T, privateKey *rsa.PrivateKey, keyID string, claims jwt.Claims) string {
	t.Helper()
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), keyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: privateKey}, options)
	if err != nil {
		t.Fatalf("create test signer: %v", err)
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}

	return token
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
