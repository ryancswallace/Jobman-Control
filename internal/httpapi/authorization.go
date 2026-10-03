package httpapi

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type currentPrincipalResponse struct {
	APIVersion             string                   `json:"apiVersion"`
	Kind                   string                   `json:"kind"`
	Principal              principalIdentity        `json:"principal"`
	AuthorizationCheckedAt time.Time                `json:"authorizationCheckedAt"`
	Namespaces             []domain.NamespaceAccess `json:"namespaces"`
	NextPageToken          string                   `json:"nextPageToken,omitempty"`
}

type principalIdentity struct {
	ID          string `json:"id,omitempty"`
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName,omitempty"`
}

func (service *api) currentPrincipal(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	query := request.URL.Query()
	limit := domain.DefaultJobListLimit
	afterID := ""
	for key, values := range query {
		if len(values) != 1 || (key != "limit" && key != "pageToken") {
			writeError(writer, http.StatusBadRequest, "invalid_request", "namespace discovery query is invalid")
			return
		}
	}
	if value, exists := query["limit"]; exists {
		parsed, err := strconv.Atoi(value[0])
		if err != nil || parsed < 1 || parsed > domain.MaximumJobListLimit {
			writeError(writer, http.StatusBadRequest, "invalid_request", "namespace discovery limit is invalid")
			return
		}
		limit = parsed
	}
	if value, exists := query["pageToken"]; exists {
		decoded, err := base64.RawURLEncoding.DecodeString(value[0])
		if len(value[0]) > 48 || err != nil || !domain.IsID(string(decoded)) {
			writeError(writer, http.StatusBadRequest, "invalid_request", "namespace discovery page token is invalid")
			return
		}
		afterID = string(decoded)
	}
	access, err := service.repository.CurrentPrincipal(request.Context(), principal, afterID, limit)
	if err != nil {
		service.writeRepositoryError(writer, request, "discover current principal", err)
		return
	}
	response := currentPrincipalResponse{APIVersion: apiVersion, Kind: "CurrentPrincipal", Principal: principalIdentity{ID: access.PrincipalID, Issuer: access.Principal.Issuer, Subject: access.Principal.Subject, DisplayName: access.DisplayName}, AuthorizationCheckedAt: access.AuthorizationCheckedAt, Namespaces: access.Namespaces}
	if response.Namespaces == nil {
		response.Namespaces = []domain.NamespaceAccess{}
	}
	if access.NextNamespaceID != "" {
		response.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(access.NextNamespaceID))
	}
	writeJSON(writer, http.StatusOK, response)
}

func (service *api) putMembershipGrant(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	if !domain.IsID(request.PathValue("grantID")) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "membership grant ID is invalid")
		return
	}
	var document membershipRequest
	_, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	grant := domain.MembershipGrant{Issuer: document.Spec.Principal.Issuer, Subject: document.Spec.Principal.Subject, DisplayName: document.Spec.Principal.DisplayName, Role: document.Spec.Role}
	if document.APIVersion != apiVersion || document.Kind != "MembershipGrant" || domain.ValidateMembershipGrant(grant) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "membership grant request is invalid")
		return
	}
	result, err := service.repository.PutMembershipGrant(request.Context(), principal, request.PathValue("namespace"), request.PathValue("grantID"), grant)
	if err != nil {
		service.writeRepositoryError(writer, request, "put membership grant", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "MembershipGrant", "grant": result})
}

func (service *api) revokeMembershipGrant(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	if !domain.IsID(request.PathValue("grantID")) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "membership grant ID is invalid")
		return
	}
	result, err := service.repository.RevokeMembershipGrant(request.Context(), principal, request.PathValue("namespace"), request.PathValue("grantID"))
	if err != nil {
		service.writeRepositoryError(writer, request, "revoke membership grant", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "MembershipGrant", "grant": result})
}
