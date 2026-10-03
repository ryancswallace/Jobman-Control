package httpapi

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/ryancswallace/jobman/diagnostic"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func parseDiagnosticSelection(raw, jobID string) (diagnostic.SharedSelection, error) {
	selection := diagnostic.SharedSelection{JobID: jobID}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return selection, errors.New("diagnostic query is malformed")
	}
	if err := knownGroupQuery(query, "deploymentId", "controlInstanceId", "namespaceId", "expectedJobRevision", "runId"); err != nil {
		return selection, err
	}
	selection.DeploymentID, selection.ControlInstanceID, selection.NamespaceID = query.Get("deploymentId"), query.Get("controlInstanceId"), query.Get("namespaceId")
	selection.RunID = query.Get("runId")
	for _, id := range []string{selection.DeploymentID, selection.ControlInstanceID, selection.NamespaceID, selection.JobID} {
		if !domain.IsID(id) {
			return selection, errors.New("diagnostic source pins are required")
		}
	}
	if query.Has("runId") && !domain.IsID(selection.RunID) {
		return selection, errors.New("diagnostic run is invalid")
	}
	if query.Has("expectedJobRevision") {
		revision, parseErr := decimalQuery(query, "expectedJobRevision", true)
		if parseErr != nil || revision < 1 || query.Get("expectedJobRevision")[0] == '0' {
			return selection, errors.New("diagnostic revision is invalid")
		}
		selection.ExpectedJobRevision = uint64(revision)
	}
	return selection, nil
}

func (service *api) diagnosticSnapshot(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	selection, err := parseDiagnosticSelection(request.URL.RawQuery, request.PathValue("jobID"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "diagnostic snapshot query is invalid")
		return
	}
	repository, ok := service.repository.(domain.DiagnosticRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "diagnostic snapshots are unavailable")
		return
	}
	snapshot, err := repository.ReadDiagnosticSnapshot(request.Context(), principal, request.PathValue("namespace"), selection)
	if errors.Is(err, domain.ErrFeatureUnavailable) {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "diagnostic snapshots are unavailable")
		return
	}
	if err != nil {
		service.writeRepositoryError(writer, request, "read diagnostic snapshot", err)
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.DiagnosticSnapshot
	}{APIVersion: apiVersion, Kind: "DiagnosticSnapshot", DiagnosticSnapshot: snapshot})
}
