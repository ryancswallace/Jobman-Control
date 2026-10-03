package domain

import (
	"context"
	"time"
)

// LogChunkOptions selects a bounded contiguous manifest range without paths.
type LogChunkOptions struct {
	RunNumber     int64
	ExecutionID   string
	Stream        string
	TailBytes     int64
	FromOffset    *int64
	AfterSequence *int64
	Limit         int
}

// ManifestAuthority identifies the exact current read and authorization snapshot.
type ManifestAuthority struct {
	AsOf                   time.Time  `json:"asOf"`
	Namespace              string     `json:"namespace"`
	NamespaceID            string     `json:"namespaceId"`
	JobID                  string     `json:"jobId"`
	RecoveryEpoch          int64      `json:"recoveryEpoch,string"`
	AuthorizationVersion   int64      `json:"authorizationVersion,string"`
	AuthorizationCheckedAt time.Time  `json:"authorizationCheckedAt"`
	AuthorizationExpiresAt *time.Time `json:"authorizationExpiresAt,omitempty"`
}

// BoundedLogChunk is immutable source metadata, never an authorized byte path.
type BoundedLogChunk struct {
	Sequence     int64     `json:"sequence,string"`
	ByteOffset   int64     `json:"byteOffset,string"`
	ByteLength   int64     `json:"byteLength,string"`
	Checksum     string    `json:"checksum"`
	StoreName    string    `json:"storeName"`
	StoreVersion int64     `json:"storeVersion,string"`
	ObjectKey    string    `json:"objectKey"`
	CapturedAt   time.Time `json:"capturedAt"`
	Complete     bool      `json:"complete"`
	Truncated    bool      `json:"truncated"`
}

// LogChunkPage exposes only the selected execution's contiguous published prefix.
// A zero manifest revision means no stored stream exists.
type LogChunkPage struct {
	ManifestAuthority
	RunID              string            `json:"runId,omitempty"`
	RunNumber          int64             `json:"runNumber,string,omitempty"`
	ExecutionID        string            `json:"executionId,omitempty"`
	TargetGenerationID string            `json:"targetGenerationId,omitempty"`
	Stream             string            `json:"stream"`
	State              string            `json:"state"`
	ManifestRevision   int64             `json:"manifestRevision,string"`
	ByteLength         int64             `json:"byteLength,string"`
	LastSequence       int64             `json:"lastSequence,string"`
	FromOffset         int64             `json:"fromOffset,string"`
	Truncated          bool              `json:"truncated"`
	Chunks             []BoundedLogChunk `json:"chunks"`
	NextAfterSequence  *int64            `json:"nextAfterSequence,string,omitempty"`
}

// ArtifactListOptions is a stable bounded tuple cursor over immutable metadata.
type ArtifactListOptions struct {
	RunNumber        int64
	AfterExecutionID string
	AfterName        string
	Limit            int
}

// BoundedArtifact includes actual run and target-generation source identity.
type BoundedArtifact struct {
	RunID              string    `json:"runId"`
	RunNumber          int64     `json:"runNumber,string"`
	ExecutionID        string    `json:"executionId"`
	TargetGenerationID string    `json:"targetGenerationId"`
	Name               string    `json:"name"`
	StoreName          string    `json:"storeName"`
	StoreVersion       int64     `json:"storeVersion,string"`
	ObjectKey          string    `json:"objectKey"`
	ByteLength         int64     `json:"byteLength,string"`
	Checksum           string    `json:"checksum"`
	PublishedAt        time.Time `json:"publishedAt"`
}

// ArtifactPage reports full filtered counts and a bounded immutable metadata page.
type ArtifactPage struct {
	ManifestAuthority
	Total         int64             `json:"total,string"`
	Items         []BoundedArtifact `json:"items"`
	NextPageToken string            `json:"nextPageToken,omitempty"`
}

// ManifestRepository is an additive monitoring extension to legacy manifests.
type ManifestRepository interface {
	ListLogChunks(context.Context, Principal, string, string, LogChunkOptions) (LogChunkPage, error)
	ListArtifacts(context.Context, Principal, string, string, ArtifactListOptions) (ArtifactPage, error)
}
