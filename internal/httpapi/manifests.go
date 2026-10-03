package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func decimalQuery(query url.Values, name string, required bool) (int64, error) {
	raw := query.Get(name)
	if raw == "" && !required {
		return 0, nil
	}
	if raw == "" {
		return 0, errors.New("required decimal query is absent")
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, errors.New("query requires an unsigned decimal integer")
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New("decimal query exceeds supported range")
	}
	return value, nil
}

func parseLogChunkOptions(raw string) (domain.LogChunkOptions, error) {
	options := domain.LogChunkOptions{Stream: "stdout", Limit: 100}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return options, errors.New("log query is malformed")
	}
	if queryErr := knownGroupQuery(query, "runNumber", "executionId", "stream", "tailBytes", "fromOffset", "afterSequence", "limit"); queryErr != nil {
		return options, queryErr
	}
	options.ExecutionID = query.Get("executionId")
	if options.ExecutionID != "" && !domain.IsID(options.ExecutionID) {
		return options, errors.New("execution ID is invalid")
	}
	if query.Has("stream") {
		options.Stream = query.Get("stream")
	}
	if options.Stream != "stdout" && options.Stream != "stderr" {
		return options, errors.New("log stream is invalid")
	}
	if options.RunNumber, err = decimalQuery(query, "runNumber", false); err != nil {
		return options, err
	}
	if query.Has("runNumber") && options.RunNumber == 0 {
		return options, errors.New("run number must be positive")
	}
	selectors := 0
	for _, name := range []string{"tailBytes", "fromOffset", "afterSequence"} {
		if !query.Has(name) {
			continue
		}
		selectors++
		value, parseErr := decimalQuery(query, name, true)
		if parseErr != nil {
			return options, parseErr
		}
		switch name {
		case "tailBytes":
			options.TailBytes = value
			if value < 1 || value > 262144 {
				return options, errors.New("tail byte limit is invalid")
			}
		case "fromOffset":
			options.FromOffset = &value
		case "afterSequence":
			options.AfterSequence = &value
		}
	}
	if selectors > 1 {
		return options, errors.New("log range selectors are mutually exclusive")
	}
	if query.Has("limit") {
		limit, parseErr := decimalQuery(query, "limit", true)
		if parseErr != nil || limit < 1 || limit > 100 {
			return options, errors.New("manifest limit is invalid")
		}
		options.Limit = int(limit)
	}
	return options, nil
}

func (service *api) listLogChunks(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	jobID := request.PathValue("jobID")
	options, err := parseLogChunkOptions(request.URL.RawQuery)
	if err != nil || !domain.IsID(jobID) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "log manifest query is invalid")
		return
	}
	repository, ok := service.repository.(domain.ManifestRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "bounded manifests are unavailable")
		return
	}
	page, err := repository.ListLogChunks(request.Context(), principal, request.PathValue("namespace"), jobID, options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list log chunks", err)
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.LogChunkPage
	}{APIVersion: apiVersion, Kind: "LogChunkList", LogChunkPage: page})
}

func parseArtifactOptions(raw string) (domain.ArtifactListOptions, error) {
	options := domain.ArtifactListOptions{Limit: 100}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return options, errors.New("artifact query is malformed")
	}
	if queryErr := knownGroupQuery(query, "runNumber", "pageToken", "limit"); queryErr != nil {
		return options, queryErr
	}
	if options.RunNumber, err = decimalQuery(query, "runNumber", false); err != nil {
		return options, err
	}
	if query.Has("runNumber") && options.RunNumber == 0 {
		return options, errors.New("run number must be positive")
	}
	if query.Has("limit") {
		limit, parseErr := decimalQuery(query, "limit", true)
		if parseErr != nil || limit < 1 || limit > 100 {
			return options, errors.New("manifest limit is invalid")
		}
		options.Limit = int(limit)
	}
	if query.Has("pageToken") {
		value := query.Get("pageToken")
		if len(value) > 256 {
			return options, errors.New("artifact cursor is too long")
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		if decodeErr != nil {
			return options, errors.New("artifact cursor is invalid")
		}
		parts := strings.Split(string(decoded), "\n")
		if len(parts) != 2 || !domain.IsID(parts[0]) || parts[1] == "" || len(parts[1]) > 128 {
			return options, errors.New("artifact cursor is invalid")
		}
		options.AfterExecutionID, options.AfterName = parts[0], parts[1]
	}
	return options, nil
}

func (service *api) listArtifactMetadata(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	jobID := request.PathValue("jobID")
	options, err := parseArtifactOptions(request.URL.RawQuery)
	if err != nil || !domain.IsID(jobID) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "artifact manifest query is invalid")
		return
	}
	repository, ok := service.repository.(domain.ManifestRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "bounded manifests are unavailable")
		return
	}
	page, err := repository.ListArtifacts(request.Context(), principal, request.PathValue("namespace"), jobID, options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list artifact metadata", err)
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.ArtifactPage
	}{APIVersion: apiVersion, Kind: "ArtifactList", ArtifactPage: page})
}
