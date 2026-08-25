package domain

import (
	"context"
	"encoding/json"
	"time"
)

const (
	// RoleViewer grants namespace reads.
	RoleViewer = "viewer"
	// RoleSubmitter grants namespace reads and job submission.
	RoleSubmitter = "submitter"
	// RoleOperator grants operational control of namespace work and targets.
	RoleOperator = "operator"
	// RoleNamespaceAdmin grants namespace membership and target administration.
	RoleNamespaceAdmin = "namespace_admin"
)

// MembershipGrant describes one namespace role binding.
type MembershipGrant struct {
	Issuer      string
	Subject     string
	DisplayName string
	Role        string
}

// Membership is one current namespace role binding.
type Membership struct {
	Namespace   string
	PrincipalID string
	Issuer      string
	Subject     string
	DisplayName string
	Role        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TargetSpec is administrator-approved target policy. Agent observations can
// narrow but never expand these capabilities.
type TargetSpec struct {
	Name             string
	Kind             string
	ExecutionBackend string
	Runtimes         []string
	OperatingSystems []string
	Architectures    []string
	Capabilities     []string
	Partitions       []PartitionSpec
	LogStoreName     string
	LogStoreVersion  int64
	ArtifactStores   []ArtifactStoreSpec
	Provider         TargetProvider
}

// TargetProvider identifies the infrastructure control plane behind one
// immutable target generation. Credentials and endpoints remain deployment
// configuration and are never stored in portable workloads.
type TargetProvider struct {
	Kind        string `json:"kind"`
	Region      string `json:"region,omitempty"`
	ClusterName string `json:"clusterName,omitempty"`
}

// ArtifactStoreSpec pins one logical artifact name to an immutable target
// mapping version. Physical storage configuration remains target-side.
type ArtifactStoreSpec struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

// PartitionSpec configures one scheduler partition beneath a Slurm target.
type PartitionSpec struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

// Target is a namespace-visible target and its immutable current generation.
type Target struct {
	ID               string
	GenerationID     string
	Generation       int64
	Namespace        string
	Name             string
	Kind             string
	State            string
	ExecutionBackend string
	Transport        string
	Runtimes         []string
	OperatingSystems []string
	Architectures    []string
	Capabilities     []string
	Partitions       []PartitionSpec
	LogStoreName     string
	LogStoreVersion  int64
	ArtifactStores   []ArtifactStoreSpec
	Provider         TargetProvider
	Revision         int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TargetStateChange requests a revision-checked lifecycle transition. A
// draining target stops new assignment while preserving its agent control
// channel for accepted work.
type TargetStateChange struct {
	State            string
	ExpectedRevision int64
}

// TargetGenerationChange replaces mutable target policy by selecting a new
// immutable generation. Existing jobs and agents remain pinned to their old
// generation.
type TargetGenerationChange struct {
	Spec             TargetSpec
	ExpectedRevision int64
}

// CreateResult distinguishes a new resource from an idempotent replay.
type CreateResult[T any] struct {
	Value    T
	Replayed bool
}

// EnrollmentRequest binds a one-time enrollment token to a human principal.
type EnrollmentRequest struct {
	Principal    Principal
	ExpectedUser string
	Lifetime     time.Duration
}

// EnrollmentToken is returned only to its authorized creator. The clear token
// is derived from server key material and never persisted.
type EnrollmentToken struct {
	ID                 string
	Namespace          string
	Target             string
	TargetGenerationID string
	Principal          Principal
	ExpectedUser       string
	Token              string
	ExpiresAt          time.Time
	Replayed           bool
}

// AgentRegistration contains the bounded facts presented during enrollment.
type AgentRegistration struct {
	TargetGenerationID string
	AgentVersion       string
	ProtocolVersions   []string
	OperatingSystem    string
	Architecture       string
	Hostname           string
	ExecutionUser      string
	ExecutionBackends  []string
	Runtimes           []string
	Capabilities       []string
	RequestDigest      string
}

// AgentCapabilities is one immutable, replay-safe observation of the data
// plane capabilities currently available to an enrolled agent.
type AgentCapabilities struct {
	AgentID              string
	ObservedAt           time.Time
	AcceptingAssignments bool
	AgentVersion         string
	OperatingSystem      string
	Architecture         string
	Hostname             string
	ExecutionUser        string
	ExecutionBackends    []string
	Runtimes             []string
	Capabilities         []string
	DocumentDigest       string
	Document             json.RawMessage
}

// AgentCapabilitySnapshot is the durable projection of an accepted
// capability observation.
type AgentCapabilitySnapshot struct {
	AgentCapabilities
	Revision int64
	Replayed bool
}

// AgentSession contains a stable agent identity and one rotating opaque
// compatibility token. Execution endpoints require an agent certificate; the
// opaque token never authorizes launch.
type AgentSession struct {
	AgentID   string
	SessionID string
	Token     string
	ExpiresAt time.Time
	Replayed  bool
}

// AgentIdentity is the authorization result for an agent session token.
type AgentIdentity struct {
	AgentID            string
	NamespaceID        string
	Namespace          string
	PrincipalID        string
	TargetID           string
	TargetGenerationID string
	CertificateSerial  string
}

// AgentCertificate is one short-lived mTLS credential bound to an agent-owned
// public key.
type AgentCertificate struct {
	Serial          string
	PublicKeyDigest string
	ExpiresAt       time.Time
}

// Assignment is a redeliverable request that still requires durable
// acceptance before target-side effects.
type Assignment struct {
	DeliveryID               string
	ExecutionID              string
	EffectiveExecutionDigest string
	Document                 json.RawMessage
	CreatedAt                time.Time
}

// Acceptance contains the immutable compare-and-swap facts supplied by an
// agent after journaling an assignment.
type Acceptance struct {
	DeliveryID               string
	ExecutionID              string
	AgentID                  string
	TargetGenerationID       string
	EffectiveExecutionDigest string
	RequestDigest            string
	RequestDocument          json.RawMessage
}

// LaunchAuthorization is the durable result of accepting one execution.
type LaunchAuthorization struct {
	AuthorizationID          string
	ExecutionID              string
	AgentID                  string
	TargetGenerationID       string
	EffectiveExecutionDigest string
	Revision                 int64
	AcceptedAt               time.Time
	Replayed                 bool
}

// ExecutionObservation is one replay-safe ordered event from an accepted
// execution.
type ExecutionObservation struct {
	EventID          string
	ExecutionID      string
	AgentID          string
	Sequence         int64
	ObservedAt       time.Time
	Type             string
	NativeID         string
	Outcome          string
	SchedulerBackend string
	SchedulerState   string
	SchedulerReason  string
	SchedulerCluster string
	DocumentDigest   string
	Document         json.RawMessage
}

// DesiredAction is durable control intent for one accepted execution.
type DesiredAction struct {
	ActionID    string
	ExecutionID string
	AgentID     string
	Revision    int64
	Document    json.RawMessage
	CreatedAt   time.Time
}

// ActionAcknowledgement binds an agent acknowledgement to an action revision.
type ActionAcknowledgement struct {
	ActionID    string
	ExecutionID string
	AgentID     string
	Revision    int64
	ObservedAt  time.Time
}

// LogChunk is one immutable, checksummed filesystem object published by an
// execution agent. PostgreSQL stores this metadata but never the object bytes.
type LogChunk struct {
	ExecutionID    string
	AgentID        string
	Stream         string
	Sequence       int64
	StoreName      string
	StoreVersion   int64
	ObjectKey      string
	ByteOffset     int64
	ByteLength     int64
	Checksum       string
	CapturedAt     time.Time
	Complete       bool
	Truncated      bool
	DocumentDigest string
	Document       json.RawMessage
}

// LogStream is the authorized manifest projection for one execution stream.
type LogStream struct {
	ExecutionID string
	RunNumber   int
	Stream      string
	State       string
	ByteLength  int64
	Truncated   bool
	Chunks      []LogChunk
}

// PublishedArtifact is immutable metadata for one output committed by an
// execution. Artifact bytes remain in the target-approved artifact store.
type PublishedArtifact struct {
	ExecutionID  string
	RunNumber    int
	Name         string
	StoreName    string
	StoreVersion int64
	ObjectKey    string
	ByteLength   int64
	Checksum     string
	PublishedAt  time.Time
}

// NamespacePolicy contains administrator-controlled admission, concurrency,
// and bounded operational retention limits.
type NamespacePolicy struct {
	Namespace                string
	MaxActiveJobs            int
	MaxQueuedJobs            int
	MaxCollectionItems       int
	MaxGraphNodes            int
	IdempotencyRetention     time.Duration
	PublishedOutboxRetention time.Duration
	Revision                 int64
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// NamespacePolicyChange is a revision-checked complete policy replacement.
type NamespacePolicyChange struct {
	MaxActiveJobs            int
	MaxQueuedJobs            int
	MaxCollectionItems       int
	MaxGraphNodes            int
	IdempotencyRetention     time.Duration
	PublishedOutboxRetention time.Duration
	ExpectedRevision         int64
}

// AuditEvent is one append-only, redacted namespace audit record.
type AuditEvent struct {
	ID               int64
	Namespace        string
	ActorKind        string
	ActorPrincipalID string
	ActorAgentID     string
	Action           string
	ResourceType     string
	ResourceID       string
	RequestDigest    string
	IdempotencyKey   string
	Details          json.RawMessage
	OccurredAt       time.Time
}

// AuditPage is an ascending export page. NextAfterID is zero at EOF.
type AuditPage struct {
	Items       []AuditEvent
	NextAfterID int64
}

// OperationalSnapshot contains bounded-cardinality service metrics.
type OperationalSnapshot struct {
	JobsByPhase       map[string]int64
	AgentsByStatus    map[string]int64
	UnpublishedOutbox int64
	StaleExecutions   int64
	OldestQueueAge    time.Duration
	RecoveryHold      bool
	RestoreEpoch      int64
}

// RecoveryState controls conservative assignment after a database restore.
type RecoveryState struct {
	Hold         bool
	RestoreEpoch int64
	Reason       string
	UpdatedAt    time.Time
}

// ControlRepository is the complete persistence boundary used by this slice.
type ControlRepository interface {
	JobRepository
	PutMembership(context.Context, Principal, string, string, string, MembershipGrant) (CreateResult[Membership], error)
	CreateTarget(context.Context, Principal, string, string, string, TargetSpec) (CreateResult[Target], error)
	CreateTargetGeneration(context.Context, Principal, string, string, string, string, TargetGenerationChange) (CreateResult[Target], error)
	UpdateTargetState(context.Context, Principal, string, string, string, string, TargetStateChange) (CreateResult[Target], error)
	GetTarget(context.Context, Principal, string, string) (Target, error)
	ListTargets(context.Context, Principal, string) ([]Target, error)
	CreateEnrollmentToken(context.Context, Principal, string, string, string, string, EnrollmentRequest) (EnrollmentToken, error)
	EnrollAgent(context.Context, string, AgentRegistration, time.Duration) (AgentSession, error)
	RenewAgentSession(context.Context, string, time.Duration) (AgentSession, error)
	AuthenticateAgent(context.Context, string) (AgentIdentity, error)
	RecordAgentCertificate(context.Context, string, AgentCertificate) error
	RotateAgentCertificate(context.Context, AgentIdentity, AgentCertificate) error
	AuthenticateAgentCertificate(context.Context, string, string, string) (AgentIdentity, error)
	RecordAgentCapabilities(context.Context, AgentIdentity, AgentCapabilities) (AgentCapabilitySnapshot, error)
	ListAssignments(context.Context, AgentIdentity, int) ([]Assignment, error)
	AcceptAssignment(context.Context, AgentIdentity, Acceptance) (LaunchAuthorization, error)
	RecordExecutionEvent(context.Context, AgentIdentity, ExecutionObservation) (bool, error)
	CommitLogChunk(context.Context, AgentIdentity, LogChunk) (bool, error)
	GetJobLogs(context.Context, Principal, string, string) ([]LogStream, error)
	GetJobArtifacts(context.Context, Principal, string, string) ([]PublishedArtifact, error)
	ListDesiredActions(context.Context, AgentIdentity, int) ([]DesiredAction, error)
	AcknowledgeDesiredAction(context.Context, AgentIdentity, ActionAcknowledgement) (bool, error)
	CancelJob(context.Context, Principal, string, string, string, string) (Job, error)
	GetNamespacePolicy(context.Context, Principal, string) (NamespacePolicy, error)
	UpdateNamespacePolicy(context.Context, Principal, string, NamespacePolicyChange) (NamespacePolicy, error)
	ExportAudit(context.Context, Principal, string, int64, int) (AuditPage, error)
	OperationalSnapshot(context.Context) (OperationalSnapshot, error)
	PruneOperationalData(context.Context, int) (int, error)
	ReconcileAssignments(context.Context, int) (int, error)
	ReconcileStaleExecutions(context.Context, time.Duration, int) (int, error)
}
