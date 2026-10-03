package domain

import "time"

// DirectoryMapping is an operator-approved, versioned identity and role map.
// Principal IDs are preserved; names and email never establish identity.
type DirectoryMapping struct {
	SourceID            string              `json:"sourceId"`
	Revision            int64               `json:"revision"`
	Namespaces          []string            `json:"namespaces"`
	ApprovedTransitions []string            `json:"approvedTransitions"`
	Bindings            []DirectoryBinding  `json:"bindings"`
	Identities          []DirectoryIdentity `json:"identities"`
}

// DirectoryBinding maps one immutable group to exactly one namespace role.
type DirectoryBinding struct {
	GroupID     string `json:"groupId"`
	NamespaceID string `json:"namespaceId"`
	Role        string `json:"role"`
}

// DirectoryAlias is an independently operator-approved OIDC alias.
type DirectoryAlias struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// DirectoryIdentity binds a directory object to a preserved or explicit new ID.
type DirectoryIdentity struct {
	DirectoryID string           `json:"directoryId"`
	PrincipalID string           `json:"principalId"`
	Issuer      string           `json:"issuer"`
	Subject     string           `json:"subject"`
	DisplayName string           `json:"displayName"`
	Aliases     []DirectoryAlias `json:"aliases"`
}

// DirectoryAccountObservation is a complete authoritative object lookup.
type DirectoryAccountObservation struct {
	DirectoryID string
	Exists      bool
	Enabled     bool
}

// DirectoryGroupObservation contains only resolved direct user members.
type DirectoryGroupObservation struct {
	GroupID      string
	Exists       bool
	DirectoryIDs []string
}

// DirectorySnapshot is complete for the exact configuration and recovery fence.
// VerifiedAt is the start of external reads, never the later database write time.
type DirectorySnapshot struct {
	SourceID             string
	Revision             int64
	Digest               string
	RecoveryEpoch        int64
	VerifiedAt           time.Time
	Accounts             []DirectoryAccountObservation
	Groups               []DirectoryGroupObservation
	IgnoredDirectMembers int
}

// DirectoryPlan summarizes consequences without changing directory authority.
type DirectoryPlan struct {
	SourceID                   string `json:"sourceId"`
	Revision                   int64  `json:"revision"`
	NamespaceCount             int    `json:"namespaceCount"`
	NewManagedNamespaces       int    `json:"newManagedNamespaces"`
	RetainedNonDirectoryGrants int64  `json:"retainedNonDirectoryGrants"`
	IdentityCount              int    `json:"identityCount"`
	BindingCount               int    `json:"bindingCount"`
}
