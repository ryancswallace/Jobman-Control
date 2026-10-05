package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func parseRunOptions(raw string) (domain.RunListOptions, error) {
	q := domain.RunListOptions{Limit: 50}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return q, errors.New("run query is malformed")
	}
	if err := knownGroupQuery(values, "limit", "pageToken"); err != nil {
		return q, err
	}
	if values.Has("limit") {
		n, e := strconv.Atoi(values.Get("limit"))
		if e != nil || n < 1 || n > 100 || strconv.Itoa(n) != values.Get("limit") {
			return q, errors.New("run limit is invalid")
		}
		q.Limit = n
	}
	q.PageToken = values.Get("pageToken")
	if len(q.PageToken) > 1024 || values.Has("pageToken") && q.PageToken == "" {
		return q, errors.New("run cursor is invalid")
	}
	return q, nil
}

func (service *api) listRuns(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	q, err := parseRunOptions(r.URL.RawQuery)
	if err != nil || !domain.IsID(r.PathValue("jobID")) {
		writeError(w, 400, "invalid_request", "run query is invalid")
		return
	}
	repo, ok := service.repository.(domain.RunRepository)
	if !ok {
		writeError(w, 501, "feature_unavailable", "bounded runs are unavailable")
		return
	}
	out, err := repo.ListRuns(r.Context(), p, r.PathValue("namespace"), r.PathValue("jobID"), q)
	if err != nil {
		service.writeRepositoryError(w, r, "list runs", err)
		return
	}
	writeJSON(w, 200, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.RunPage
	}{apiVersion, "RunList", out})
}

func (service *api) getRun(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if r.URL.RawQuery != "" || !domain.IsID(r.PathValue("jobID")) || !domain.IsID(r.PathValue("runID")) {
		writeError(w, 400, "invalid_request", "run selector is invalid")
		return
	}
	repo, ok := service.repository.(domain.RunRepository)
	if !ok {
		writeError(w, 501, "feature_unavailable", "bounded runs are unavailable")
		return
	}
	out, err := repo.GetRun(r.Context(), p, r.PathValue("namespace"), r.PathValue("jobID"), r.PathValue("runID"))
	if err != nil {
		service.writeRepositoryError(w, r, "read run", err)
		return
	}
	writeJSON(w, 200, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.RunDetail
	}{apiVersion, "RunDetail", out})
}
