package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type monitoringEventRepository struct {
	domain.ControlRepository
	err    error
	limit  int
	cursor string
}

func (r *monitoringEventRepository) MonitoringCheckpoint(context.Context, domain.Principal) (domain.MonitoringCheckpoint, error) {
	return domain.MonitoringCheckpoint{RecoveryEpoch: 9007199254740993, HeadCursor: "head", RetentionSeconds: 2592000}, r.err
}

func (r *monitoringEventRepository) ReadMonitoringEvents(_ context.Context, _ domain.Principal, cursor string, limit int) (domain.MonitoringEventPage, error) {
	r.limit, r.cursor = limit, cursor
	return domain.MonitoringEventPage{Items: []domain.MonitoringEvent{{Position: 9007199254740993, JobRevision: "9007199254740993"}}, NextCursor: "next"}, r.err
}

func TestMonitoringEventHTTPBoundaries(t *testing.T) {
	t.Parallel()
	repository := &monitoringEventRepository{}
	service := &api{repository: repository}
	principal := domain.Principal{Delegation: &domain.DelegatedActor{ServiceOnly: true, Operation: domain.CapabilityEventsRead, Mode: "worker"}}
	response := httptest.NewRecorder()
	service.monitoringCheckpoint(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitoring-events/checkpoint", nil), principal)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"recoveryEpoch":"9007199254740993"`) {
		t.Fatalf("checkpoint=%d,%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	service.monitoringEvents(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitoring-events?cursor=opaque&limit=200", nil), principal)
	if response.Code != 200 || repository.cursor != "opaque" || repository.limit != 200 || !strings.Contains(response.Body.String(), `"position":"9007199254740993"`) {
		t.Fatalf("events=%d,%s", response.Code, response.Body.String())
	}
	for _, query := range []string{"", "cursor=", "cursor=x&cursor=y", "cursor=x&limit=0", "cursor=x&limit=201", "cursor=x&limit=", "cursor=x&scope=all", "cursor=%zz", "cursor=x;limit=1"} {
		response = httptest.NewRecorder()
		service.monitoringEvents(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitoring-events?"+query, nil), principal)
		if response.Code != 400 {
			t.Fatalf("query %q=%d", query, response.Code)
		}
	}
	for _, test := range []struct {
		err    error
		code   string
		status int
	}{{domain.ErrEventCursorExpired, "event_cursor_expired", 409}, {domain.ErrEventRecoveryChanged, "source_recovery_changed", 409}, {domain.ErrEventScopeChanged, "event_cursor_scope_changed", 409}, {domain.ErrEventCursorInvalid, "invalid_request", 400}, {domain.ErrForbidden, "forbidden", 403}} {
		repository.err = test.err
		response = httptest.NewRecorder()
		service.monitoringEvents(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitoring-events?cursor=old", nil), principal)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
			t.Fatalf("gap=%d,%s", response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/v1/monitoring-events/checkpoint", "/v1/monitoring-events?cursor=x"} {
		response = httptest.NewRecorder()
		newTestHandler(t, repository, 1024).ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if response.Code != 403 {
			t.Fatal("OIDC principal accessed feed")
		}
	}
	for _, route := range []string{"GET /v1/monitoring-events", "GET /v1/monitoring-events/checkpoint"} {
		if delegationRouteOperation(route) != domain.CapabilityEventsRead {
			t.Fatal("wrong service-only operation")
		}
	}
}
