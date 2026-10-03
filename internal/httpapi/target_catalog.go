package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type targetPageToken struct {
	CreatedBefore time.Time `json:"createdBefore"`
	CreatedAt     time.Time `json:"createdAt"`
	ID            string    `json:"id"`
}

func parseTargetCatalogOptions(raw string) (domain.TargetCatalogOptions, error) {
	options := domain.TargetCatalogOptions{Limit: 100}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return options, errors.New("target catalog query is malformed")
	}
	if queryErr := knownGroupQuery(query, "limit", "pageToken", "createdBefore"); queryErr != nil {
		return options, queryErr
	}
	if query.Has("limit") {
		limit, parseErr := decimalQuery(query, "limit", true)
		if parseErr != nil || limit < 1 || limit > 200 {
			return options, errors.New("target catalog limit is invalid")
		}
		options.Limit = int(limit)
	}
	if query.Has("createdBefore") {
		parsed, parseErr := time.Parse(time.RFC3339Nano, query.Get("createdBefore"))
		if parseErr != nil || parsed.IsZero() {
			return options, errors.New("target creation cutoff is invalid")
		}
		value := parsed.UTC()
		options.CreatedBefore = &value
	}
	if query.Has("pageToken") {
		rawToken := query.Get("pageToken")
		if len(rawToken) > 1024 {
			return options, errors.New("target cursor exceeds bound")
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(rawToken)
		if decodeErr != nil {
			return options, errors.New("invalid target cursor")
		}
		var token targetPageToken
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decodeErr = decoder.Decode(&token); decodeErr != nil {
			return options, errors.New("invalid target cursor")
		}
		if decodeErr = decoder.Decode(new(any)); !errors.Is(decodeErr, io.EOF) {
			return options, errors.New("invalid target cursor suffix")
		}
		if token.CreatedBefore.IsZero() || token.CreatedAt.IsZero() || token.CreatedAt.After(token.CreatedBefore) || !domain.IsID(token.ID) || options.CreatedBefore != nil && !options.CreatedBefore.Equal(token.CreatedBefore) {
			return options, errors.New("target cursor cutoff or identity is invalid")
		}
		cutoff := token.CreatedBefore.UTC()
		options.CreatedBefore = &cutoff
		options.Before = &domain.JobCursor{CreatedAt: token.CreatedAt.UTC(), ID: token.ID}
	}
	return options, nil
}

func (service *api) listTargetCatalog(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	options, err := parseTargetCatalogOptions(request.URL.RawQuery)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target catalog query is invalid")
		return
	}
	repository, ok := service.repository.(domain.TargetCatalogRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "target catalogs are unavailable")
		return
	}
	page, err := repository.ListTargetCatalog(request.Context(), principal, request.PathValue("namespace"), options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list target catalog", err)
		return
	}
	next := ""
	if page.NextCursor != nil {
		encoded, encodeErr := json.Marshal(targetPageToken{CreatedBefore: page.CreatedBefore, CreatedAt: page.NextCursor.CreatedAt, ID: page.NextCursor.ID})
		if encodeErr != nil {
			service.writeRepositoryError(writer, request, "encode target cursor", encodeErr)
			return
		}
		next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.TargetCatalog
		NextPageToken string `json:"nextPageToken,omitempty"`
	}{APIVersion: apiVersion, Kind: "TargetCatalog", TargetCatalog: page, NextPageToken: next})
}

func (service *api) getTargetSnapshot(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(query) != 0 || !domain.IsID(request.PathValue("targetID")) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target snapshot query is invalid")
		return
	}
	repository, ok := service.repository.(domain.TargetCatalogRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "target catalogs are unavailable")
		return
	}
	snapshot, err := repository.GetTargetSnapshot(request.Context(), principal, request.PathValue("namespace"), request.PathValue("targetID"))
	if err != nil {
		service.writeRepositoryError(writer, request, "get target snapshot", err)
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.TargetSnapshot
	}{APIVersion: apiVersion, Kind: "TargetSnapshot", TargetSnapshot: snapshot})
}

type targetPartitionToken struct {
	GenerationID string `json:"generationId"`
	AfterName    string `json:"afterName"`
}

func parseTargetPartitionOptions(raw string) (domain.TargetPartitionOptions, error) {
	options := domain.TargetPartitionOptions{Limit: 100}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return options, errors.New("target partition query is malformed")
	}
	if queryErr := knownGroupQuery(query, "limit", "pageToken", "generationId"); queryErr != nil {
		return options, queryErr
	}
	if query.Has("limit") {
		limit, parseErr := decimalQuery(query, "limit", true)
		if parseErr != nil || limit < 1 || limit > 200 {
			return options, errors.New("target partition limit is invalid")
		}
		options.Limit = int(limit)
	}
	if query.Has("generationId") {
		options.GenerationID = query.Get("generationId")
		if !domain.IsID(options.GenerationID) {
			return options, errors.New("target generation is invalid")
		}
	}
	if query.Has("pageToken") {
		rawToken := query.Get("pageToken")
		if len(rawToken) > 1024 {
			return options, errors.New("target partition cursor exceeds bound")
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(rawToken)
		if decodeErr != nil {
			return options, errors.New("invalid target partition cursor")
		}
		var token targetPartitionToken
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decodeErr = decoder.Decode(&token); decodeErr != nil {
			return options, errors.New("invalid target partition cursor")
		}
		if decodeErr = decoder.Decode(new(any)); !errors.Is(decodeErr, io.EOF) {
			return options, errors.New("invalid target partition cursor suffix")
		}
		if !domain.IsID(token.GenerationID) || !domain.ValidName(token.AfterName) || options.GenerationID != "" && options.GenerationID != token.GenerationID {
			return options, errors.New("target partition cursor generation or name is invalid")
		}
		options.GenerationID, options.AfterName = token.GenerationID, token.AfterName
	}
	if !domain.IsID(options.GenerationID) {
		return options, errors.New("target generation is required")
	}
	return options, nil
}

func (service *api) listTargetPartitions(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	options, err := parseTargetPartitionOptions(request.URL.RawQuery)
	if err != nil || !domain.IsID(request.PathValue("targetID")) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target partition query is invalid")
		return
	}
	repository, ok := service.repository.(domain.TargetCatalogRepository)
	if !ok {
		writeError(writer, http.StatusNotImplemented, "feature_unavailable", "target catalogs are unavailable")
		return
	}
	page, err := repository.ListTargetPartitions(request.Context(), principal, request.PathValue("namespace"), request.PathValue("targetID"), options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list target partitions", err)
		return
	}
	next := ""
	if page.NextName != "" {
		encoded, encodeErr := json.Marshal(targetPartitionToken{GenerationID: page.GenerationID, AfterName: page.NextName})
		if encodeErr != nil {
			service.writeRepositoryError(writer, request, "encode target partition cursor", encodeErr)
			return
		}
		next = base64.RawURLEncoding.EncodeToString(encoded)
	}
	writeJSON(writer, http.StatusOK, struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		domain.TargetPartitionPage
		NextPageToken string `json:"nextPageToken,omitempty"`
	}{APIVersion: apiVersion, Kind: "TargetPartitionList", TargetPartitionPage: page, NextPageToken: next})
}
