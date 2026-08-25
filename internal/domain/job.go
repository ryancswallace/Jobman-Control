// Package domain contains the shared control-plane model and effect boundaries.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrForbidden means the principal cannot access the requested namespace.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound means the authorized lookup found no resource.
	ErrNotFound = errors.New("not found")
	// ErrIdempotencyConflict means an idempotency key was reused for different intent.
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	// ErrConflict means a durable resource conflicts with current state.
	ErrConflict = errors.New("resource conflicts with current state")
	// ErrInvalidPlacement means the selected target cannot satisfy the workload.
	ErrInvalidPlacement = errors.New("target cannot satisfy workload placement")
	// ErrUnauthenticated means no valid client or agent identity was established.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrQuotaExceeded means namespace policy rejected new durable work.
	ErrQuotaExceeded = errors.New("namespace quota exceeded")
)

const (
	// JobPhaseAccepted is durable intent that has not been assigned for execution.
	JobPhaseAccepted = "accepted"
	// JobDesiredStateRun records the submitter's initial desired state.
	JobDesiredStateRun = "run"
	// DefaultJobListLimit is the default page size for shared job history.
	DefaultJobListLimit = 50
	// MaximumJobListLimit bounds one shared job history response.
	MaximumJobListLimit = 200
)

var knownJobPhases = map[string]struct{}{
	"accepted":           {},
	"assigning":          {},
	"accepted_execution": {},
	"running":            {},
	"terminal":           {},
}

// Principal is the stable identity asserted by the authentication layer.
type Principal struct {
	Issuer  string
	Subject string
}

// DevelopmentIdentity describes the one explicitly configured development
// principal and namespace. It is not a production authentication mechanism.
type DevelopmentIdentity struct {
	Principal   Principal
	DisplayName string
	Namespace   string
}

// BootstrapIdentity describes an operator-configured initial namespace
// administrator. It is applied only at service startup.
type BootstrapIdentity struct {
	Principal   Principal
	DisplayName string
	Namespace   string
	Mode        string
}

// JobSubmission contains validated, canonical submission intent.
type JobSubmission struct {
	Namespace         string
	Name              string
	Labels            map[string]string
	Target            string
	Partition         string
	RuntimeKind       string
	OperatingSystems  []string
	Architectures     []string
	Capabilities      []string
	ArtifactStores    []string
	WorkloadDigest    string
	WorkloadDocument  json.RawMessage
	RequestDigest     string
	RequestDocument   json.RawMessage
	ExecutionFeatures ExecutionFeatures
}

// CollectionSubmission is one atomically accepted bounded set of independent
// jobs. Items are already decoded and canonically sealed by the protocol
// boundary.
type CollectionSubmission struct {
	Namespace       string
	Name            string
	Labels          map[string]string
	MaxActive       int
	FailurePolicy   string
	ArrayPolicy     string
	RequestDigest   string
	RequestDocument json.RawMessage
	Items           []JobSubmission
}

// ExecutionFeatures records the portable features relevant to target
// admission. The canonical workload remains the source of truth.
type ExecutionFeatures struct {
	DirectCommand                bool
	Resources                    bool
	TemporaryStorage             bool
	Artifacts                    bool
	Extensions                   bool
	EnvironmentProfile           bool
	Secrets                      bool
	RetryMaxRuns                 int
	SchedulerEnvironmentOverride bool
}

// Job is the current durable shared job snapshot.
type Job struct {
	ID                    string
	Namespace             string
	Name                  string
	Labels                map[string]string
	Phase                 string
	DesiredState          string
	Outcome               string
	Target                string
	Partition             string
	TargetID              string
	TargetGenerationID    string
	ExecutionBackend      string
	NativeID              string
	ObservationConfidence string
	ConfidenceUpdatedAt   time.Time
	Scheduler             *SchedulerStatus
	WorkloadDigest        string
	RequestDigest         string
	Revision              int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// SchedulerStatus is the latest normalized scheduler evidence for a job's
// current execution.
type SchedulerStatus struct {
	Backend    string
	State      string
	Reason     string
	Cluster    string
	ObservedAt time.Time
}

// SubmitResult distinguishes a newly accepted request from an idempotent replay.
type SubmitResult struct {
	Job      Job
	Replayed bool
}

// CollectionItem identifies one ordered child job.
type CollectionItem struct {
	Index int
	Name  string
	Job   Job
}

// Collection is the current aggregate snapshot. Child Jobs remain the source
// of truth for execution lifecycle and outcomes.
type Collection struct {
	ID            string
	Namespace     string
	Name          string
	Labels        map[string]string
	MaxActive     int
	FailurePolicy string
	ArrayPolicy   string
	ArrayMode     string
	Phase         string
	Outcome       string
	Revision      int64
	Total         int
	Active        int
	Terminal      int
	Succeeded     int
	Failed        int
	Canceled      int
	Items         []CollectionItem
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CollectionResult distinguishes new acceptance from idempotent replay.
type CollectionResult struct {
	Collection Collection
	Replayed   bool
}

// GraphSubmission is one atomically accepted immutable dependency graph.
type GraphSubmission struct {
	Namespace         string
	Name              string
	Labels            map[string]string
	MaxActive         int
	UnsatisfiedPolicy string
	RequestDigest     string
	RequestDocument   json.RawMessage
	Nodes             []JobSubmission
	Edges             []GraphEdgeSubmission
}

// GraphEdgeSubmission references nodes by their immutable graph-local names.
type GraphEdgeSubmission struct {
	From      string
	To        string
	Predicate string
	Outcomes  []string
}

// GraphDependency is one persisted upstream readiness predicate.
type GraphDependency struct {
	From      string
	Predicate string
	Outcomes  []string
	Satisfied bool
}

// GraphItem binds one stable index/name to an independently observable job.
type GraphItem struct {
	Index        int
	Name         string
	Disposition  string
	Dependencies []GraphDependency
	Job          Job
}

// Graph is the aggregate projection of immutable nodes and edges.
type Graph struct {
	ID                string
	Namespace         string
	Name              string
	Labels            map[string]string
	MaxActive         int
	UnsatisfiedPolicy string
	Phase             string
	Outcome           string
	Revision          int64
	Total             int
	Waiting           int
	Active            int
	Terminal          int
	Succeeded         int
	Failed            int
	Canceled          int
	Skipped           int
	Blocked           int
	Items             []GraphItem
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// GraphResult distinguishes new acceptance from idempotent replay.
type GraphResult struct {
	Graph    Graph
	Replayed bool
}

// CompletedHistoryImport is validated terminal standalone history plus source
// provenance. It never migrates live work or creates an executable run.
type CompletedHistoryImport struct {
	Job             JobSubmission
	Outcome         string
	CompletedAt     time.Time
	SourceStore     string
	SourceSchema    int
	SourceJobID     string
	RequestDigest   string
	RequestDocument json.RawMessage
}

// HistoryImportResult distinguishes validation, acceptance, and replay.
type HistoryImportResult struct {
	Job      Job
	DryRun   bool
	Replayed bool
}

// JobListOptions selects one stable, newest-first page of namespace jobs.
type JobListOptions struct {
	Limit  int
	Phase  string
	Before *JobCursor
}

// JobCursor identifies the exclusive upper boundary of the next page.
type JobCursor struct {
	CreatedAt time.Time
	ID        string
}

// JobPage contains one page and an optional continuation cursor.
type JobPage struct {
	Jobs       []Job
	NextCursor *JobCursor
}

// ValidJobPhase reports whether phase is a stable shared lifecycle phase.
func ValidJobPhase(phase string) bool {
	_, valid := knownJobPhases[phase]

	return valid
}

// JobRepository is the persistence boundary consumed by the client API.
type JobRepository interface {
	Ready(context.Context) error
	SubmitJob(context.Context, Principal, string, JobSubmission) (SubmitResult, error)
	SubmitCollection(context.Context, Principal, string, CollectionSubmission) (CollectionResult, error)
	GetCollection(context.Context, Principal, string, string) (Collection, error)
	SubmitGraph(context.Context, Principal, string, GraphSubmission) (GraphResult, error)
	GetGraph(context.Context, Principal, string, string) (Graph, error)
	CancelGraph(context.Context, Principal, string, string, string, string) (Graph, error)
	ImportCompletedHistory(context.Context, Principal, string, bool, CompletedHistoryImport) (HistoryImportResult, error)
	ListJobs(context.Context, Principal, string, JobListOptions) (JobPage, error)
	GetJob(context.Context, Principal, string, string) (Job, error)
}
