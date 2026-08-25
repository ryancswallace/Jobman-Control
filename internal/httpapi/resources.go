package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type membershipRequest struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Spec       membershipRequestSpec `json:"spec"`
}

type membershipRequestSpec struct {
	Principal membershipPrincipal `json:"principal"`
	Role      string              `json:"role"`
}

type membershipPrincipal struct {
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
}

type targetRequest struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   targetMetadata `json:"metadata"`
	Spec       targetSpec     `json:"spec"`
}

type targetStateRequest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		State string `json:"state"`
	} `json:"spec"`
}

type targetGenerationRequest struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Spec       targetSpec `json:"spec"`
}

type targetMetadata struct {
	Name string `json:"name"`
}

type targetSpec struct {
	Kind             string                   `json:"kind"`
	ExecutionBackend string                   `json:"executionBackend"`
	Runtimes         []string                 `json:"runtimes"`
	OperatingSystems []string                 `json:"operatingSystems,omitempty"`
	Architectures    []string                 `json:"architectures,omitempty"`
	Capabilities     []string                 `json:"capabilities,omitempty"`
	Partitions       []domain.PartitionSpec   `json:"partitions,omitempty"`
	LogStore         *artifactStoreReference  `json:"logStore,omitempty"`
	ArtifactStores   []artifactStoreReference `json:"artifactStores,omitempty"`
	Provider         domain.TargetProvider    `json:"provider,omitempty"`
}

type artifactStoreReference struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

type enrollmentTokenRequest struct {
	APIVersion string                     `json:"apiVersion"`
	Kind       string                     `json:"kind"`
	Spec       enrollmentTokenRequestSpec `json:"spec"`
}

type enrollmentTokenRequestSpec struct {
	Principal    enrollmentPrincipal `json:"principal"`
	ExpectedUser string              `json:"expectedUser"`
}

type enrollmentPrincipal struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

func (service *api) putMembership(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	var document membershipRequest
	digest, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	grant := domain.MembershipGrant{
		Issuer: document.Spec.Principal.Issuer, Subject: document.Spec.Principal.Subject,
		DisplayName: document.Spec.Principal.DisplayName, Role: document.Spec.Role,
	}
	if document.APIVersion != apiVersion || document.Kind != "Membership" ||
		domain.ValidateMembershipGrant(grant) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "membership request is invalid")
		return
	}
	result, err := service.repository.PutMembership(
		request.Context(), principal, request.PathValue("namespace"),
		idempotencyKey, digest, grant,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "put membership", err)
		return
	}
	if result.Replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, newMembershipResponse(result.Value))
}

func (service *api) createTarget(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	var document targetRequest
	_, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	if document.APIVersion != apiVersion || document.Kind != "Target" {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target request is invalid")
		return
	}
	spec := normalizeTargetSpec(document)
	if err = domain.ValidateTargetSpec(spec); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target request is invalid")
		return
	}
	digest, err := semanticDigest(documentFromTargetSpec(spec))
	if err != nil {
		service.writeRepositoryError(writer, request, "digest target", err)
		return
	}
	result, err := service.repository.CreateTarget(
		request.Context(), principal, request.PathValue("namespace"),
		idempotencyKey, digest, spec,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "create target", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", targetLocation(result.Value))
	writer.Header().Set("ETag", revisionETag(result.Value.Revision))
	writeJSON(writer, status, newTargetResponse(result.Value))
}

func (service *api) getTarget(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	if !domain.ValidName(request.PathValue("target")) {
		writeError(writer, http.StatusBadRequest, "invalid_target", "target name is invalid")
		return
	}
	target, err := service.repository.GetTarget(
		request.Context(), principal, request.PathValue("namespace"), request.PathValue("target"),
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get target", err)
		return
	}
	writer.Header().Set("ETag", revisionETag(target.Revision))
	writeJSON(writer, http.StatusOK, newTargetResponse(target))
}

func (service *api) createTargetGeneration(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")

		return
	}
	name := request.PathValue("target")
	if !domain.ValidName(name) {
		writeError(writer, http.StatusBadRequest, "invalid_target", "target name is invalid")

		return
	}
	expectedRevision, valid := readIfMatchRevision(request.Header)
	if !valid {
		writeError(writer, http.StatusPreconditionRequired, "revision_required", "exactly one target revision If-Match header is required")

		return
	}
	var document targetGenerationRequest
	_, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)

		return
	}
	spec := normalizeTargetGenerationSpec(name, document.Spec)
	change := domain.TargetGenerationChange{Spec: spec, ExpectedRevision: expectedRevision}
	if document.APIVersion != apiVersion || document.Kind != "TargetGeneration" ||
		domain.ValidateTargetGenerationChange(change) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target generation request is invalid")

		return
	}
	digest, err := semanticDigest(targetGenerationRequest{
		APIVersion: apiVersion, Kind: "TargetGeneration",
		Spec: documentFromTargetSpec(spec).Spec,
	})
	if err != nil {
		service.writeRepositoryError(writer, request, "digest target generation", err)

		return
	}
	result, err := service.repository.CreateTargetGeneration(
		request.Context(), principal, request.PathValue("namespace"), name,
		idempotencyKey, digest, change,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "create target generation", err)

		return
	}
	if result.Replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", targetLocation(result.Value))
	writer.Header().Set("ETag", revisionETag(result.Value.Revision))
	writeJSON(writer, http.StatusOK, newTargetResponse(result.Value))
}

func (service *api) updateTargetState(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	name := request.PathValue("target")
	if !domain.ValidName(name) {
		writeError(writer, http.StatusBadRequest, "invalid_target", "target name is invalid")
		return
	}
	expectedRevision, valid := readIfMatchRevision(request.Header)
	if !valid {
		writeError(writer, http.StatusPreconditionRequired, "revision_required", "exactly one target revision If-Match header is required")
		return
	}
	var document targetStateRequest
	_, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	change := domain.TargetStateChange{State: document.Spec.State, ExpectedRevision: expectedRevision}
	if document.APIVersion != apiVersion || document.Kind != "TargetStateChange" ||
		domain.ValidateTargetStateChange(change) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "target state request is invalid")
		return
	}
	digest, err := semanticDigest(document)
	if err != nil {
		service.writeRepositoryError(writer, request, "digest target state", err)
		return
	}
	result, err := service.repository.UpdateTargetState(
		request.Context(), principal, request.PathValue("namespace"), name,
		idempotencyKey, digest, change,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "update target state", err)
		return
	}
	if result.Replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("ETag", revisionETag(result.Value.Revision))
	writeJSON(writer, http.StatusOK, newTargetResponse(result.Value))
}

func readIfMatchRevision(header http.Header) (int64, bool) {
	values := header.Values("If-Match")
	if len(values) != 1 || !strings.HasPrefix(values[0], `"revision-`) || !strings.HasSuffix(values[0], `"`) {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(values[0], `"revision-`), `"`)
	revision, err := strconv.ParseInt(value, 10, 64)

	return revision, err == nil && revision > 0
}

func (service *api) listTargets(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	targets, err := service.repository.ListTargets(
		request.Context(), principal, request.PathValue("namespace"),
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "list targets", err)
		return
	}
	items := make([]targetResponse, 0, len(targets))
	for _, target := range targets {
		items = append(items, newTargetResponse(target))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": apiVersion, "kind": "TargetList", "items": items,
	})
}

func (service *api) createEnrollmentToken(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	var document enrollmentTokenRequest
	digest, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	enrollment := domain.EnrollmentRequest{
		Principal: domain.Principal{
			Issuer: document.Spec.Principal.Issuer, Subject: document.Spec.Principal.Subject,
		},
		ExpectedUser: document.Spec.ExpectedUser, Lifetime: service.enrollmentLifetime,
	}
	if document.APIVersion != apiVersion || document.Kind != "AgentEnrollmentToken" ||
		domain.ValidateEnrollmentRequest(enrollment) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "enrollment-token request is invalid")
		return
	}
	result, err := service.repository.CreateEnrollmentToken(
		request.Context(), principal, request.PathValue("namespace"),
		request.PathValue("target"), idempotencyKey, digest, enrollment,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "create enrollment token", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, status, newEnrollmentTokenResponse(result))
}

func (service *api) decodeControlJSON(
	writer http.ResponseWriter,
	request *http.Request,
	destination any,
) (string, error) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		return "", errUnsupportedMediaType
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.maxRequestBytes)
	encoded, err := io.ReadAll(request.Body)
	if err != nil {
		return "", err
	}
	if validationErr := validateJSONDocument(encoded, 32); validationErr != nil {
		return "", validationErr
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("trailing JSON value")
		}
		return "", err
	}

	return semanticDigest(destination)
}

func validateJSONDocument(encoded []byte, maximumDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0, maximumDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}

	return nil
}

func validateJSONValue(decoder *json.Decoder, depth, maximumDepth int) error {
	if depth > maximumDepth {
		return errors.New("JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, tokenErr := decoder.Token()
			if tokenErr != nil {
				return tokenErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("JSON object contains a duplicate key")
			}
			seen[key] = struct{}{}
			if validationErr := validateJSONValue(decoder, depth+1, maximumDepth); validationErr != nil {
				return validationErr
			}
		}
	case '[':
		for decoder.More() {
			if validationErr := validateJSONValue(decoder, depth+1, maximumDepth); validationErr != nil {
				return validationErr
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter == '{' && closing != json.Delim('}') {
		return errors.New("unterminated JSON object")
	}
	if delimiter == '[' && closing != json.Delim(']') {
		return errors.New("unterminated JSON array")
	}

	return nil
}

var errUnsupportedMediaType = errors.New("unsupported media type")

func (service *api) writeDecodeError(writer http.ResponseWriter, err error) {
	var maximumBytesError *http.MaxBytesError
	switch {
	case errors.Is(err, errUnsupportedMediaType):
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	case errors.As(err, &maximumBytesError):
		writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request exceeds the configured size limit")
	default:
		writeError(writer, http.StatusBadRequest, "invalid_request", "request is invalid")
	}
}

func semanticDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode request digest: %w", err)
	}
	sum := sha256.Sum256(encoded)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func normalizeTargetSpec(document targetRequest) domain.TargetSpec {
	spec := domain.TargetSpec{
		Name: document.Metadata.Name, Kind: document.Spec.Kind,
		ExecutionBackend: document.Spec.ExecutionBackend,
		Runtimes:         append([]string(nil), document.Spec.Runtimes...),
		OperatingSystems: append([]string(nil), document.Spec.OperatingSystems...),
		Architectures:    append([]string(nil), document.Spec.Architectures...),
		Capabilities:     append([]string(nil), document.Spec.Capabilities...),
		Partitions:       append([]domain.PartitionSpec(nil), document.Spec.Partitions...),
		Provider:         domain.NormalizeTargetProvider(document.Spec.Provider),
	}
	if document.Spec.LogStore != nil {
		spec.LogStoreName = document.Spec.LogStore.Name
		spec.LogStoreVersion = document.Spec.LogStore.Version
	}
	for _, store := range document.Spec.ArtifactStores {
		spec.ArtifactStores = append(spec.ArtifactStores, domain.ArtifactStoreSpec{
			Name: store.Name, Version: store.Version,
		})
	}
	sort.Strings(spec.Runtimes)
	sort.Strings(spec.OperatingSystems)
	sort.Strings(spec.Architectures)
	sort.Strings(spec.Capabilities)
	sort.Slice(spec.Partitions, func(left, right int) bool {
		return spec.Partitions[left].Name < spec.Partitions[right].Name
	})
	sort.Slice(spec.ArtifactStores, func(left, right int) bool {
		return spec.ArtifactStores[left].Name < spec.ArtifactStores[right].Name
	})

	return spec
}

func normalizeTargetGenerationSpec(name string, document targetSpec) domain.TargetSpec {
	return normalizeTargetSpec(targetRequest{
		APIVersion: apiVersion, Kind: "Target", Metadata: targetMetadata{Name: name}, Spec: document,
	})
}

func documentFromTargetSpec(spec domain.TargetSpec) targetRequest {
	return targetRequest{
		APIVersion: apiVersion, Kind: "Target", Metadata: targetMetadata{Name: spec.Name},
		Spec: targetSpec{
			Kind: spec.Kind, ExecutionBackend: spec.ExecutionBackend,
			Runtimes: spec.Runtimes, OperatingSystems: spec.OperatingSystems,
			Architectures: spec.Architectures, Capabilities: spec.Capabilities,
			Partitions: spec.Partitions,
			Provider:   domain.NormalizeTargetProvider(spec.Provider),
			ArtifactStores: func() []artifactStoreReference {
				stores := make([]artifactStoreReference, 0, len(spec.ArtifactStores))
				for _, store := range spec.ArtifactStores {
					stores = append(stores, artifactStoreReference{Name: store.Name, Version: store.Version})
				}
				return stores
			}(),
			LogStore: func() *artifactStoreReference {
				if spec.LogStoreName == "" {
					return nil
				}
				return &artifactStoreReference{Name: spec.LogStoreName, Version: spec.LogStoreVersion}
			}(),
		},
	}
}

type membershipResponse struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   membershipMetadata     `json:"metadata"`
	Spec       membershipResponseSpec `json:"spec"`
}

type membershipMetadata struct {
	PrincipalID string    `json:"principalId"`
	Namespace   string    `json:"namespace"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type membershipResponseSpec struct {
	Principal membershipPrincipal `json:"principal"`
	Role      string              `json:"role"`
}

func newMembershipResponse(value domain.Membership) membershipResponse {
	return membershipResponse{
		APIVersion: apiVersion, Kind: "Membership",
		Metadata: membershipMetadata{
			PrincipalID: value.PrincipalID, Namespace: value.Namespace,
			CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		},
		Spec: membershipResponseSpec{
			Principal: membershipPrincipal{
				Issuer: value.Issuer, Subject: value.Subject, DisplayName: value.DisplayName,
			},
			Role: value.Role,
		},
	}
}

type targetResponse struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   targetResponseMetadata `json:"metadata"`
	Spec       targetResponseSpec     `json:"spec"`
	Status     targetStatus           `json:"status"`
}

type targetResponseMetadata struct {
	ID           string    `json:"id"`
	GenerationID string    `json:"generationId"`
	Generation   int64     `json:"generation"`
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Revision     int64     `json:"revision"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type targetResponseSpec struct {
	Kind             string                   `json:"kind"`
	ExecutionBackend string                   `json:"executionBackend"`
	ControlTransport string                   `json:"controlTransport"`
	Runtimes         []string                 `json:"runtimes"`
	OperatingSystems []string                 `json:"operatingSystems,omitempty"`
	Architectures    []string                 `json:"architectures,omitempty"`
	Capabilities     []string                 `json:"capabilities,omitempty"`
	Partitions       []domain.PartitionSpec   `json:"partitions,omitempty"`
	LogStore         *artifactStoreReference  `json:"logStore,omitempty"`
	ArtifactStores   []artifactStoreReference `json:"artifactStores,omitempty"`
	Provider         domain.TargetProvider    `json:"provider"`
}

type targetStatus struct {
	State string `json:"state"`
}

func newTargetResponse(value domain.Target) targetResponse {
	var logStore *artifactStoreReference
	if value.LogStoreName != "" {
		logStore = &artifactStoreReference{Name: value.LogStoreName, Version: value.LogStoreVersion}
	}
	artifactStores := make([]artifactStoreReference, 0, len(value.ArtifactStores))
	for _, store := range value.ArtifactStores {
		artifactStores = append(artifactStores, artifactStoreReference{Name: store.Name, Version: store.Version})
	}
	return targetResponse{
		APIVersion: apiVersion, Kind: "Target",
		Metadata: targetResponseMetadata{
			ID: value.ID, GenerationID: value.GenerationID, Generation: value.Generation,
			Namespace: value.Namespace, Name: value.Name, Revision: value.Revision,
			CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		},
		Spec: targetResponseSpec{
			Kind: value.Kind, ExecutionBackend: value.ExecutionBackend,
			ControlTransport: value.Transport, Runtimes: value.Runtimes,
			OperatingSystems: value.OperatingSystems, Architectures: value.Architectures,
			Capabilities: value.Capabilities, Partitions: value.Partitions,
			LogStore: logStore, ArtifactStores: artifactStores,
			Provider: domain.NormalizeTargetProvider(value.Provider),
		},
		Status: targetStatus{State: value.State},
	}
}

func targetLocation(target domain.Target) string {
	return "/v1/namespaces/" + target.Namespace + "/targets/" + target.Name
}

type enrollmentTokenResponse struct {
	APIVersion string                      `json:"apiVersion"`
	Kind       string                      `json:"kind"`
	Metadata   enrollmentTokenMetadata     `json:"metadata"`
	Spec       enrollmentTokenResponseSpec `json:"spec"`
}

type enrollmentTokenMetadata struct {
	ID        string    `json:"id"`
	Namespace string    `json:"namespace"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type enrollmentTokenResponseSpec struct {
	Target             string              `json:"target"`
	TargetGenerationID string              `json:"targetGenerationId"`
	Principal          enrollmentPrincipal `json:"principal"`
	ExpectedUser       string              `json:"expectedUser"`
	Token              string              `json:"token,omitempty"`
}

func newEnrollmentTokenResponse(value domain.EnrollmentToken) enrollmentTokenResponse {
	return enrollmentTokenResponse{
		APIVersion: apiVersion, Kind: "AgentEnrollmentToken",
		Metadata: enrollmentTokenMetadata{
			ID: value.ID, Namespace: value.Namespace, ExpiresAt: value.ExpiresAt,
		},
		Spec: enrollmentTokenResponseSpec{
			Target: value.Target, TargetGenerationID: value.TargetGenerationID,
			Principal: enrollmentPrincipal{
				Issuer: value.Principal.Issuer, Subject: value.Principal.Subject,
			},
			ExpectedUser: value.ExpectedUser, Token: value.Token,
		},
	}
}

func readLimit(request *http.Request, fallback int) (int, bool) {
	value := request.URL.Query().Get("limit")
	if value == "" {
		return fallback, true
	}
	if strings.TrimSpace(value) != value {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)

	return parsed, err == nil && parsed >= 1 && parsed <= 100
}
