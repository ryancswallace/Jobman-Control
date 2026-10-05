package domain

import (
	"context"
	"time"
)

// JobRun is a factual run reference, not an execution event timeline. Empty
// execution fields mean no execution was assigned; no command or path is exposed.
type JobRun struct {
	ID                 string    `json:"id"`
	Number             int64     `json:"number,string"`
	Phase              string    `json:"phase"`
	DesiredState       string    `json:"desiredState"`
	Outcome            string    `json:"outcome,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	ExecutionID        string    `json:"executionId,omitempty"`
	ExecutionPhase     string    `json:"executionPhase,omitempty"`
	TargetID           string    `json:"targetId,omitempty"`
	TargetGenerationID string    `json:"targetGenerationId,omitempty"`
	Backend            string    `json:"backend,omitempty"`
	Confidence         string    `json:"confidence,omitempty"`
}

// RunListOptions selects one bounded source page.
type RunListOptions struct {
	Limit     int
	PageToken string
}

// RunPage contains current run facts under a stable run-number ceiling.
type RunPage struct {
	ManifestAuthority
	Items         []JobRun `json:"items"`
	Total         int64    `json:"total,string"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

// RunDetail contains one authorized run and its source snapshot.
type RunDetail struct {
	ManifestAuthority
	Run JobRun `json:"run"`
}

// RunRepository is the additive jobs.read run selection surface.
type RunRepository interface {
	ListRuns(context.Context, Principal, string, string, RunListOptions) (RunPage, error)
	GetRun(context.Context, Principal, string, string, string) (RunDetail, error)
}
