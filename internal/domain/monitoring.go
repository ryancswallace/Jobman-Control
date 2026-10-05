package domain

import "time"

// JobOwner is the verified original submitting principal, absent for imports
// whose original owner is unknown. An importing principal is not the submitter.
type JobOwner struct {
	ID          string `json:"id"`
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
}

// JobLifecycle separates observed lifecycle times from Control recording times.
type JobLifecycle struct {
	StartedAt           *time.Time `json:"startedAt,omitempty"`
	StartedRecordedAt   *time.Time `json:"startedRecordedAt,omitempty"`
	StartedProvenance   string     `json:"startedProvenance,omitempty"`
	CompletedAt         *time.Time `json:"completedAt,omitempty"`
	CompletedRecordedAt *time.Time `json:"completedRecordedAt,omitempty"`
	CompletedProvenance string     `json:"completedProvenance,omitempty"`
}

// RunReference identifies the current persisted run and execution.
type RunReference struct {
	ID          string `json:"id"`
	Number      string `json:"number"`
	ExecutionID string `json:"executionId,omitempty"`
}

// JobGroupReference preserves immutable wrapper and node identity.
type JobGroupReference struct {
	CollectionID     string `json:"collectionId,omitempty"`
	CollectionIndex  *int   `json:"collectionIndex,omitempty"`
	GraphID          string `json:"graphId,omitempty"`
	GraphIndex       *int   `json:"graphIndex,omitempty"`
	GraphDisposition string `json:"graphDisposition,omitempty"`
}

// ControlCapabilities describes implemented monitoring surfaces only.
type ControlCapabilities struct {
	InstanceID       string    `json:"instanceId"`
	RecoveryEpoch    string    `json:"recoveryEpoch"`
	ServerTime       time.Time `json:"serviceTime"`
	ContractVersions []string  `json:"contractVersions"`
	Features         []string  `json:"features"`
	MaximumPageSize  int       `json:"maximumPageSize"`
}

// NamespaceSummary contains complete source counts at one statement snapshot.
// Outcome counts use the half-open completion window; phase counts use all jobs.
type NamespaceSummary struct {
	NamespaceID           string            `json:"namespaceId"`
	Namespace             string            `json:"namespace"`
	AsOf                  time.Time         `json:"asOf"`
	CompletedFrom         time.Time         `json:"completedFrom"`
	CompletedBefore       time.Time         `json:"completedBefore"`
	Total                 string            `json:"total"`
	Active                string            `json:"active"`
	AwaitingExecution     string            `json:"awaitingExecution"`
	EvidenceAttention     string            `json:"evidenceAttention"`
	MissingCompletionTime string            `json:"missingCompletionTime"`
	ByPhase               map[string]string `json:"byPhase"`
	ByOutcome             map[string]string `json:"byOutcome"`
}
