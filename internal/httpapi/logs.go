package httpapi

import (
	"encoding/json"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const maximumLogChunkBytes int64 = 256 * 1024

var (
	logStoreNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9._-]{0,62}[a-z0-9])?$`)
	logDigestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type logChunkRequest struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Metadata   logChunkRequestMetadata `json:"metadata"`
	Spec       logChunkRequestSpec     `json:"spec"`
}

type logChunkRequestMetadata struct {
	ExecutionID string `json:"executionId"`
	Stream      string `json:"stream"`
	Sequence    int64  `json:"sequence"`
}

type logChunkRequestSpec struct {
	StoreName    string    `json:"storeName"`
	StoreVersion int64     `json:"storeVersion"`
	ObjectKey    string    `json:"objectKey"`
	ByteOffset   int64     `json:"byteOffset"`
	ByteLength   int64     `json:"byteLength"`
	Checksum     string    `json:"checksum"`
	CapturedAt   time.Time `json:"capturedAt"`
	Complete     bool      `json:"complete"`
	Truncated    bool      `json:"truncated"`
}

func (service *api) commitAgentLogChunk(writer http.ResponseWriter, request *http.Request) {
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	var document logChunkRequest
	digest, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	sequence, sequenceErr := strconv.ParseInt(request.PathValue("sequence"), 10, 64)
	if sequenceErr != nil || !validLogChunkRequest(document) ||
		document.Metadata.ExecutionID != request.PathValue("executionID") ||
		document.Metadata.Stream != request.PathValue("stream") || document.Metadata.Sequence != sequence {
		writeError(writer, http.StatusBadRequest, "invalid_request", "log chunk metadata is invalid")
		return
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "encode log chunk", err)
		return
	}
	replayed, err := service.repository.CommitLogChunk(
		request.Context(), identity,
		domain.LogChunk{
			ExecutionID: document.Metadata.ExecutionID, AgentID: identity.AgentID,
			Stream: document.Metadata.Stream, Sequence: document.Metadata.Sequence,
			StoreName: document.Spec.StoreName, StoreVersion: document.Spec.StoreVersion,
			ObjectKey: document.Spec.ObjectKey, ByteOffset: document.Spec.ByteOffset,
			ByteLength: document.Spec.ByteLength, Checksum: document.Spec.Checksum,
			CapturedAt: document.Spec.CapturedAt.UTC(), Complete: document.Spec.Complete,
			Truncated: document.Spec.Truncated, DocumentDigest: digest, Document: encoded,
		},
	)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "commit log chunk", err)
		return
	}
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": apiVersion, "kind": "LogChunkReceipt",
		"executionId": document.Metadata.ExecutionID, "stream": document.Metadata.Stream,
		"sequence": document.Metadata.Sequence,
	})
}

func validLogChunkRequest(document logChunkRequest) bool {
	objectKey := document.Spec.ObjectKey
	cleanKey := path.Clean(objectKey)
	return document.APIVersion == apiVersion && document.Kind == "LogChunk" &&
		domain.IsID(document.Metadata.ExecutionID) &&
		(document.Metadata.Stream == "stdout" || document.Metadata.Stream == "stderr") &&
		document.Metadata.Sequence > 0 && logStoreNamePattern.MatchString(document.Spec.StoreName) &&
		document.Spec.StoreVersion > 0 && len(objectKey) <= 1024 && objectKey == cleanKey &&
		!strings.HasPrefix(objectKey, "/") && !strings.ContainsAny(objectKey, `\:`) &&
		document.Spec.ByteOffset >= 0 && document.Spec.ByteLength >= 0 &&
		document.Spec.ByteLength <= maximumLogChunkBytes &&
		(document.Spec.ByteLength > 0 || document.Spec.Complete) &&
		logDigestPattern.MatchString(document.Spec.Checksum) && !document.Spec.CapturedAt.IsZero() &&
		(!document.Spec.Truncated || document.Spec.Complete)
}

func (service *api) getJobLogs(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	jobID := request.PathValue("jobID")
	if !domain.IsID(jobID) {
		writeError(writer, http.StatusBadRequest, "invalid_job_id", "job ID is invalid")
		return
	}
	streams, err := service.repository.GetJobLogs(
		request.Context(), principal, request.PathValue("namespace"), jobID,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get job logs", err)
		return
	}
	items := make([]logStreamResponse, 0, len(streams))
	for _, stream := range streams {
		chunks := make([]logChunkResponse, 0, len(stream.Chunks))
		for _, chunk := range stream.Chunks {
			chunks = append(chunks, logChunkResponse{
				Sequence: chunk.Sequence, StoreName: chunk.StoreName,
				StoreVersion: chunk.StoreVersion, ObjectKey: chunk.ObjectKey,
				ByteOffset: chunk.ByteOffset, ByteLength: chunk.ByteLength,
				Checksum: chunk.Checksum, CapturedAt: chunk.CapturedAt,
			})
		}
		items = append(items, logStreamResponse{
			ExecutionID: stream.ExecutionID, RunNumber: stream.RunNumber,
			Stream: stream.Stream, State: stream.State, ByteLength: stream.ByteLength,
			Truncated: stream.Truncated, Chunks: chunks,
		})
	}
	writeJSON(writer, http.StatusOK, jobLogManifestResponse{
		APIVersion: apiVersion, Kind: "JobLogManifest", Namespace: request.PathValue("namespace"),
		JobID: jobID, Items: items,
	})
}

type jobLogManifestResponse struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Namespace  string              `json:"namespace"`
	JobID      string              `json:"jobId"`
	Items      []logStreamResponse `json:"items"`
}

type logStreamResponse struct {
	ExecutionID string             `json:"executionId"`
	RunNumber   int                `json:"runNumber"`
	Stream      string             `json:"stream"`
	State       string             `json:"state"`
	ByteLength  int64              `json:"byteLength"`
	Truncated   bool               `json:"truncated"`
	Chunks      []logChunkResponse `json:"chunks"`
}

type logChunkResponse struct {
	Sequence     int64     `json:"sequence"`
	StoreName    string    `json:"storeName"`
	StoreVersion int64     `json:"storeVersion"`
	ObjectKey    string    `json:"objectKey"`
	ByteOffset   int64     `json:"byteOffset"`
	ByteLength   int64     `json:"byteLength"`
	Checksum     string    `json:"checksum"`
	CapturedAt   time.Time `json:"capturedAt"`
}

func (service *api) getJobArtifacts(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	jobID := request.PathValue("jobID")
	if !domain.IsID(jobID) {
		writeError(writer, http.StatusBadRequest, "invalid_job_id", "job ID is invalid")
		return
	}
	artifacts, err := service.repository.GetJobArtifacts(
		request.Context(), principal, request.PathValue("namespace"), jobID,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get job artifacts", err)
		return
	}
	items := make([]publishedArtifactResponse, 0, len(artifacts))
	for _, artifact := range artifacts {
		items = append(items, publishedArtifactResponse{
			ExecutionID: artifact.ExecutionID, RunNumber: artifact.RunNumber,
			Name: artifact.Name, StoreName: artifact.StoreName,
			StoreVersion: artifact.StoreVersion, ObjectKey: artifact.ObjectKey,
			ByteLength: artifact.ByteLength, Checksum: artifact.Checksum,
			PublishedAt: artifact.PublishedAt,
		})
	}
	writeJSON(writer, http.StatusOK, jobArtifactManifestResponse{
		APIVersion: apiVersion, Kind: "JobArtifactManifest",
		Namespace: request.PathValue("namespace"), JobID: jobID, Items: items,
	})
}

type jobArtifactManifestResponse struct {
	APIVersion string                      `json:"apiVersion"`
	Kind       string                      `json:"kind"`
	Namespace  string                      `json:"namespace"`
	JobID      string                      `json:"jobId"`
	Items      []publishedArtifactResponse `json:"items"`
}

type publishedArtifactResponse struct {
	ExecutionID  string    `json:"executionId"`
	RunNumber    int       `json:"runNumber"`
	Name         string    `json:"name"`
	StoreName    string    `json:"storeName"`
	StoreVersion int64     `json:"storeVersion"`
	ObjectKey    string    `json:"objectKey"`
	ByteLength   int64     `json:"byteLength"`
	Checksum     string    `json:"checksum"`
	PublishedAt  time.Time `json:"publishedAt"`
}
