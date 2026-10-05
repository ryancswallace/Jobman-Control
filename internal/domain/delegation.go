package domain

import (
	"context"
	"slices"
	"time"
)

// DelegatedActor is set only by the verified service authenticator. It never
// replaces current repository authorization with a reusable access grant.
type DelegatedActor struct {
	ServiceOnly           bool
	Audience              string
	ServiceID             string
	KeyID                 string
	CertificateThumbprint string
	DirectoryID           string
	Operation             string
	NamespaceIDs          []string
	Mode                  string
	AssertionID           string
	AssertionDigest       string
	IssuedAt              time.Time
	ExpiresAt             time.Time
}

// DelegationKey is an operator-registered public key and its narrow service
// policy. Namespace and operation sets are checked again by repository reads.
type DelegationKey struct {
	ServiceID              string   `json:"serviceId"`
	KeyID                  string   `json:"keyId"`
	Audience               string   `json:"audience"`
	PublicKey              []byte   `json:"publicKey"`
	CertificateThumbprints []string `json:"certificateThumbprints"`
	NamespaceIDs           []string `json:"namespaceIds"`
	Operations             []string `json:"operations"`
	Enabled                bool     `json:"enabled"`
}

// DelegationRegistry reads current public service registration and atomically
// records one accepted assertion for replay protection and service/actor audit.
type DelegationRegistry interface {
	DelegationKey(context.Context, string, string) (DelegationKey, error)
	AcceptDelegationAssertion(context.Context, Principal) error
}

// DelegationReadOperation deliberately excludes execution, administration,
// audit, generic proxying, and service-only event-feed capabilities.
func DelegationReadOperation(operation string) bool {
	return slices.Contains([]string{CapabilityNamespaceRead, CapabilityJobsRead, CapabilityGroupsRead, CapabilityTargetsRead, CapabilityLogsRead, CapabilityArtifactsRead, CapabilityEvidenceRead}, operation)
}

// DelegationOperation includes the separate service-only monitoring capability.
// DelegationReadOperation remains restricted to represented-user reads.
func DelegationOperation(operation string) bool {
	return DelegationReadOperation(operation) || operation == CapabilityEventsRead
}
