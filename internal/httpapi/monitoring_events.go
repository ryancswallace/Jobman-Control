package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func monitoringServicePrincipal(principal domain.Principal) bool {
	actor := principal.Delegation
	return actor != nil && actor.ServiceOnly && actor.Operation == domain.CapabilityEventsRead && actor.Mode == "worker" && actor.DirectoryID == "" && principal.Issuer == "" && principal.Subject == ""
}

func (service *api) monitoringCheckpoint(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	if !monitoringServicePrincipal(principal) {
		writeError(writer, http.StatusForbidden, "forbidden", "service monitoring authority is required")
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_request", "checkpoint accepts no query parameters")
		return
	}
	repository, ok := service.repository.(domain.MonitoringEventRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "monitoring events are unavailable")
		return
	}
	result, err := repository.MonitoringCheckpoint(request.Context(), principal)
	if err != nil {
		service.writeRepositoryError(writer, request, "read monitoring checkpoint", err)
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.MonitoringCheckpoint
	}{apiVersion, "MonitoringCheckpoint", result})
}

func (service *api) monitoringEvents(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	if !monitoringServicePrincipal(principal) {
		writeError(writer, http.StatusForbidden, "forbidden", "service monitoring authority is required")
		return
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || knownGroupQuery(query, "cursor", "limit") != nil || query.Get("cursor") == "" || len(query.Get("cursor")) > 1024 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "monitoring event query is invalid")
		return
	}
	limit := 100
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 200 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "monitoring event limit is invalid")
		return
	}
	repository, ok := service.repository.(domain.MonitoringEventRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "monitoring events are unavailable")
		return
	}
	result, err := repository.ReadMonitoringEvents(request.Context(), principal, query.Get("cursor"), limit)
	for _, gap := range []struct {
		err  error
		code string
	}{{domain.ErrEventCursorExpired, "event_cursor_expired"}, {domain.ErrEventRecoveryChanged, "source_recovery_changed"}, {domain.ErrEventScopeChanged, "event_cursor_scope_changed"}} {
		if errors.Is(err, gap.err) {
			writeError(writer, http.StatusConflict, gap.code, "monitoring checkpoint must be explicitly re-established")
			return
		}
	}
	if errors.Is(err, domain.ErrEventCursorInvalid) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "monitoring event cursor is invalid")
		return
	}
	if err != nil {
		service.writeRepositoryError(writer, request, "read monitoring events", err)
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.MonitoringEventPage
	}{apiVersion, "MonitoringEventList", result})
}
