package httpapi

import (
	"net/http"
	"net/url"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func (service *api) capabilities(writer http.ResponseWriter, request *http.Request) {
	result, err := service.repository.Capabilities(request.Context())
	if err != nil {
		service.writeRepositoryError(writer, request, "get capabilities", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "ControlCapabilities", "capabilities": result})
}

func (service *api) namespaceSummary(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	query, parseErr := url.ParseQuery(request.URL.RawQuery)
	if parseErr != nil {
		writeError(writer, http.StatusBadRequest, "invalid_query", "summary query is invalid")
		return
	}
	for name, values := range query {
		if (name != "completedFrom" && name != "completedBefore") || len(values) != 1 {
			writeError(writer, http.StatusBadRequest, "invalid_query", "summary query is invalid")
			return
		}
	}
	var from, before *time.Time
	for name, destination := range map[string]**time.Time{"completedFrom": &from, "completedBefore": &before} {
		if value, exists := query[name]; exists {
			parsed, err := time.Parse(time.RFC3339Nano, value[0])
			if err != nil {
				writeError(writer, http.StatusBadRequest, "invalid_query", "summary timestamps must use RFC3339")
				return
			}
			parsed = parsed.UTC()
			*destination = &parsed
		}
	}
	if (from == nil) != (before == nil) || (from != nil && !from.Before(*before)) {
		writeError(writer, http.StatusBadRequest, "invalid_query", "summary needs a complete increasing completion window")
		return
	}
	result, err := service.repository.NamespaceSummary(request.Context(), principal, request.PathValue("namespace"), from, before)
	if err != nil {
		service.writeRepositoryError(writer, request, "get namespace summary", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "NamespaceSummary", "summary": result})
}
