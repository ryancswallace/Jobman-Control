package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type authorizationRepository struct {
	domain.ControlRepository
	access  domain.PrincipalAccess
	err     error
	afterID string
	limit   int
	actor   domain.Principal
	grantID string
	grant   domain.MembershipGrant
}

func (repository *authorizationRepository) CurrentPrincipal(_ context.Context, actor domain.Principal, afterID string, limit int) (domain.PrincipalAccess, error) {
	repository.afterID, repository.limit, repository.actor = afterID, limit, actor
	return repository.access, repository.err
}

func (repository *authorizationRepository) PutMembershipGrant(_ context.Context, actor domain.Principal, _, id string, grant domain.MembershipGrant) (domain.ContributingGrant, error) {
	repository.actor, repository.grantID, repository.grant = actor, id, grant
	return domain.ContributingGrant{ID: id, Role: grant.Role, Provenance: "manual"}, repository.err
}

func (repository *authorizationRepository) RevokeMembershipGrant(_ context.Context, actor domain.Principal, _, id string) (domain.ContributingGrant, error) {
	repository.actor, repository.grantID = actor, id
	return domain.ContributingGrant{ID: id, Provenance: "manual"}, repository.err
}

func TestCurrentPrincipalContract(t *testing.T) {
	t.Parallel()
	principal := domain.Principal{Issuer: "test-issuer", Subject: "test-subject"}
	repository := &authorizationRepository{access: domain.PrincipalAccess{Principal: principal, PrincipalID: testJobID, AuthorizationCheckedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), Namespaces: []domain.NamespaceAccess{{ID: testJobID, Name: "research", Roles: []string{"operator", "viewer"}, Capabilities: []string{"jobs.read", "logs.read"}, AuthorizationVersion: "9007199254740993"}}, NextNamespaceID: testJobID}}
	handler := newTestHandler(t, repository, 1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me?limit=1", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	var body currentPrincipalResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kind != "CurrentPrincipal" || body.Principal.ID != testJobID || body.Namespaces[0].AuthorizationVersion != "9007199254740993" || body.NextPageToken != base64.RawURLEncoding.EncodeToString([]byte(testJobID)) {
		t.Fatalf("body = %#v", body)
	}
	if repository.actor != principal || repository.limit != 1 {
		t.Fatalf("repository arguments = %#v", repository)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me?pageToken="+body.NextPageToken, nil))
	if response.Code != http.StatusOK || repository.afterID != testJobID {
		t.Fatalf("pagination = %d, %q", response.Code, repository.afterID)
	}
	repository.access.Namespaces = nil
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil))
	if !strings.Contains(response.Body.String(), `"namespaces":[]`) {
		t.Fatalf("empty namespace list = %s", response.Body.String())
	}
}

func TestCurrentPrincipalRejectsInvalidQueries(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"limit=0", "limit=201", "limit=", "limit=no", "limit=1&limit=2", "pageToken=bad", "pageToken=", "scope=all", "limit=%zz", "pageToken=%zz", "%zz=value", "limit=1;pageToken=anything"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			repository := &authorizationRepository{}
			response := httptest.NewRecorder()
			newTestHandler(t, repository, 1024).ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me?"+query, nil))
			if response.Code != http.StatusBadRequest || repository.limit != 0 {
				t.Fatalf("response = %d, called limit=%d", response.Code, repository.limit)
			}
		})
	}
}

func TestGrantHTTPContractAndAuthority(t *testing.T) {
	t.Parallel()
	body := `{"apiVersion":"jobman.control/v1alpha1","kind":"MembershipGrant","spec":{"principal":{"issuer":"issuer","subject":"member","displayName":"Member"},"role":"operator"}}`
	repository := &authorizationRepository{}
	handler := newTestHandler(t, repository, 2048)
	path := "/v1/namespaces/research/membership-grants/" + testJobID
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || repository.grantID != testJobID || repository.grant.Role != domain.RoleOperator || repository.actor.Subject != "test-subject" {
		t.Fatalf("grant result = %d %s", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		err    error
		status int
	}{{domain.ErrForbidden, http.StatusForbidden}, {domain.ErrNotFound, http.StatusNotFound}, {domain.ErrConflict, http.StatusConflict}, {errors.New("internal safe test"), http.StatusInternalServerError}} {
		repository.err = test.err
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, nil))
		if response.Code != test.status {
			t.Fatalf("revoke status = %d, want %d", response.Code, test.status)
		}
	}
	for _, invalid := range []string{strings.Replace(body, `"operator"`, `"superuser"`, 1), strings.Replace(body, `"MembershipGrant"`, `"Membership"`, 1), strings.Replace(body, `"role":"operator"`, `"role":"operator","provenance":"directory"`, 1)} {
		request = httptest.NewRequestWithContext(t.Context(), http.MethodPut, path, strings.NewReader(invalid))
		request.Header.Set("Content-Type", "application/json")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid grant response = %d", response.Code)
		}
	}
}
