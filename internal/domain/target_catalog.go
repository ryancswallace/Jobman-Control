package domain

import (
	"context"
	"time"
)

// NamespaceReadAuthority is one authorized repeatable-read source snapshot.
type NamespaceReadAuthority struct {
	AsOf                   time.Time  `json:"asOf"`
	Namespace              string     `json:"namespace"`
	NamespaceID            string     `json:"namespaceId"`
	RecoveryEpoch          int64      `json:"recoveryEpoch,string"`
	AuthorizationVersion   int64      `json:"authorizationVersion,string"`
	AuthorizationCheckedAt time.Time  `json:"authorizationCheckedAt"`
	AuthorizationExpiresAt *time.Time `json:"authorizationExpiresAt,omitempty"`
}

// TargetCatalogOptions keeps immutable creation ordering separate from state.
type TargetCatalogOptions struct {
	Limit         int
	CreatedBefore *time.Time
	Before        *JobCursor
}

// CatalogStoreReference preserves 64-bit immutable mapping versions on the wire.
type CatalogStoreReference struct {
	Name    string `json:"name"`
	Version int64  `json:"version,string"`
}

// CatalogTargetGeneration exposes approved configuration, not inferred capacity.
type CatalogTargetGeneration struct {
	ID                  string                  `json:"id"`
	Number              int64                   `json:"number,string"`
	ExecutionBackend    string                  `json:"executionBackend"`
	Transport           string                  `json:"transport"`
	Runtimes            []string                `json:"runtimes"`
	OperatingSystems    []string                `json:"operatingSystems"`
	Architectures       []string                `json:"architectures"`
	Capabilities        []string                `json:"capabilities"`
	Partitions          []PartitionSpec         `json:"partitions"`
	PartitionCount      int64                   `json:"partitionCount,string"`
	PartitionsTruncated bool                    `json:"partitionsTruncated"`
	LogStore            *CatalogStoreReference  `json:"logStore,omitempty"`
	ArtifactStores      []CatalogStoreReference `json:"artifactStores"`
	Provider            TargetProvider          `json:"provider"`
}

// CatalogTarget is a target and its currently selected immutable generation.
type CatalogTarget struct {
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	Kind       string                  `json:"kind"`
	State      string                  `json:"state"`
	Revision   int64                   `json:"revision,string"`
	CreatedAt  time.Time               `json:"createdAt"`
	UpdatedAt  time.Time               `json:"updatedAt"`
	Generation CatalogTargetGeneration `json:"generation"`
}

// TargetCatalog has complete cutoff counts and bounded count/byte pages.
type TargetCatalog struct {
	NamespaceReadAuthority
	CreatedBefore time.Time       `json:"createdBefore"`
	Total         int64           `json:"total,string"`
	Items         []CatalogTarget `json:"items"`
	NextCursor    *JobCursor      `json:"-"`
}

// TargetSnapshot addresses one actual target by UUID within its namespace.
type TargetSnapshot struct {
	NamespaceReadAuthority
	Target CatalogTarget `json:"target"`
}

// TargetCatalogRepository adds bounded monitoring without legacy API changes.
type TargetCatalogRepository interface {
	ListTargetCatalog(context.Context, Principal, string, TargetCatalogOptions) (TargetCatalog, error)
	GetTargetSnapshot(context.Context, Principal, string, string) (TargetSnapshot, error)
	ListTargetPartitions(context.Context, Principal, string, string, TargetPartitionOptions) (TargetPartitionPage, error)
}

// TargetPartitionOptions pins traversal to the currently selected generation.
type TargetPartitionOptions struct {
	GenerationID string
	AfterName    string
	Limit        int
}

// TargetPartitionPage supplies complete counts without unbounded configuration.
type TargetPartitionPage struct {
	NamespaceReadAuthority
	TargetID     string          `json:"targetId"`
	GenerationID string          `json:"generationId"`
	Total        int64           `json:"total,string"`
	Items        []PartitionSpec `json:"items"`
	NextName     string          `json:"-"`
}
