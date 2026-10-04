package domain

import (
	"context"
	"errors"
	"time"
)

// CapabilityEventsRead is service-only and confers no represented-user access.
const CapabilityEventsRead = "events.read"

var (
	// ErrEventCursorExpired requires explicit recovery past a retention gap.
	ErrEventCursorExpired = errors.New("event cursor expired")
	// ErrEventRecoveryChanged fences restored or substituted sources.
	ErrEventRecoveryChanged = errors.New("source recovery changed")
	// ErrEventScopeChanged requires a checkpoint for the new service scope.
	ErrEventScopeChanged = errors.New("event cursor scope changed")
	// ErrEventCursorInvalid rejects malformed or future continuations.
	ErrEventCursorInvalid = errors.New("event cursor invalid")
)

// MonitoringEvent deliberately contains no display name or workload contents.
type MonitoringEvent struct {
	EventID             string     `json:"eventId"`
	Position            int64      `json:"position,string"`
	NamespaceID         string     `json:"namespaceId"`
	JobID               string     `json:"jobId"`
	RunID               string     `json:"runId,omitempty"`
	RunNumber           string     `json:"runNumber,omitempty"`
	OwnerPrincipalID    string     `json:"ownerPrincipalId"`
	OldPhase            string     `json:"oldPhase"`
	NewPhase            string     `json:"newPhase"`
	Outcome             string     `json:"outcome"`
	JobRevision         string     `json:"jobRevision"`
	ObservedCompletedAt *time.Time `json:"observedCompletedAt,omitempty"`
	RecordedAt          time.Time  `json:"recordedAt"`
	Imported            bool       `json:"imported"`
	Reconciliation      bool       `json:"reconciliation"`
}

// MonitoringCheckpoint is a source-clock activation boundary and retained range.
// Backlog metadata is restricted to the assertion's exact namespace set.
type MonitoringCheckpoint struct {
	ControlInstanceID           string     `json:"controlInstanceId"`
	RecoveryEpoch               int64      `json:"recoveryEpoch,string"`
	AsOf                        time.Time  `json:"asOf"`
	HeadCursor                  string     `json:"headCursor"`
	OldestCursor                string     `json:"oldestCursor"`
	RetentionSeconds            int64      `json:"retentionSeconds,string"`
	BacklogCount                int64      `json:"backlogCount,string"`
	OldestUnpublishedRecordedAt *time.Time `json:"oldestUnpublishedRecordedAt,omitempty"`
}

// MonitoringEventPage contains an ordered bounded page and source checkpoint.
type MonitoringEventPage struct {
	MonitoringCheckpoint
	Items      []MonitoringEvent `json:"items"`
	NextCursor string            `json:"nextCursor"`
	HasMore    bool              `json:"hasMore"`
}

// MonitoringEventRepository uses independently registered service authority.
// Its principal must be verified service-only events.read, never an OIDC user.
type MonitoringEventRepository interface {
	MonitoringCheckpoint(context.Context, Principal) (MonitoringCheckpoint, error)
	ReadMonitoringEvents(context.Context, Principal, string, int) (MonitoringEventPage, error)
}
