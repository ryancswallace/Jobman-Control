// Package resources plans and applies bounded declarative administrative changes
// through the authorized Control API. It never accesses PostgreSQL directly.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/ryancswallace/jobman-control/internal/configjson"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

const maximumBytes = 2 << 20

// Document pins desired changes to one service and immutable namespace IDs.
type Document struct {
	SchemaVersion         int        `json:"schemaVersion"`
	Endpoint              string     `json:"endpoint"`
	ExpectedInstanceID    string     `json:"expectedInstanceId"`
	ExpectedRecoveryEpoch string     `json:"expectedRecoveryEpoch"`
	Resources             []Resource `json:"resources"`
}

// Resource is one explicit operation. Omission never means delete or revoke.
type Resource struct {
	Kind                 string  `json:"kind"`
	Namespace            string  `json:"namespace"`
	NamespaceID          string  `json:"namespaceId"`
	Name                 string  `json:"name,omitempty"`
	ID                   string  `json:"id,omitempty"`
	ExpectedGenerationID string  `json:"expectedGenerationId,omitempty"`
	ExpectedRevision     int64   `json:"expectedRevision,omitempty"`
	Policy               *Policy `json:"policy,omitempty"`
	Target               *Target `json:"target,omitempty"`
	Grant                *Grant  `json:"grant,omitempty"`
}

// Policy contains the existing namespace admission/retention API fields.
type Policy struct {
	MaxActiveJobs            int    `json:"maxActiveJobs"`
	MaxQueuedJobs            int    `json:"maxQueuedJobs"`
	MaxCollectionItems       int    `json:"maxCollectionItems"`
	MaxGraphNodes            int    `json:"maxGraphNodes"`
	IdempotencyRetention     string `json:"idempotencyRetention"`
	PublishedOutboxRetention string `json:"publishedOutboxRetention"`
}

// Store identifies an immutable logical store mapping.
type Store struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

// Target contains the existing target API fields, not physical storage secrets.
type Target struct {
	Kind             string                 `json:"kind"`
	ExecutionBackend string                 `json:"executionBackend"`
	Runtimes         []string               `json:"runtimes"`
	OperatingSystems []string               `json:"operatingSystems,omitempty"`
	Architectures    []string               `json:"architectures,omitempty"`
	Capabilities     []string               `json:"capabilities,omitempty"`
	Partitions       []domain.PartitionSpec `json:"partitions,omitempty"`
	LogStore         *Store                 `json:"logStore,omitempty"`
	ArtifactStores   []Store                `json:"artifactStores,omitempty"`
	Provider         domain.TargetProvider  `json:"provider"`
}

// Grant is an immutable manual membership contribution. Reusing a revoked ID
// remains forbidden by the server, including during repeated apply operations.
type Grant struct {
	Principal Principal `json:"principal"`
	Role      string    `json:"role"`
}

// Principal supplies the independently approved issuer/subject identity.
type Principal struct {
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
}

// Decode validates a bounded document without network or secret access.
func Decode(reader io.Reader) (Document, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximumBytes+1))
	if err != nil || len(data) > maximumBytes {
		return Document{}, errors.New("resource document exceeds input bounds")
	}
	var document Document
	if err := configjson.Decode(data, &document); err != nil {
		return Document{}, err
	}
	return document, document.Validate()
}

// Validate rejects unsupported operations and incomplete identity fences.
func (document Document) Validate() error {
	endpoint, err := url.Parse(document.Endpoint)
	epoch, epochErr := strconv.ParseUint(document.ExpectedRecoveryEpoch, 10, 63)
	if document.SchemaVersion != 1 || !domain.IsID(document.ExpectedInstanceID) || epochErr != nil || epoch < 1 || strconv.FormatUint(epoch, 10) != document.ExpectedRecoveryEpoch || err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" || len(document.Resources) < 1 || len(document.Resources) > 320 {
		return errors.New("resource document identity or bounds are invalid")
	}
	seen := map[string]bool{}
	for _, resource := range document.Resources {
		if !domain.ValidName(resource.Namespace) || !domain.IsID(resource.NamespaceID) {
			return errors.New("resource namespace identity is invalid")
		}
		if err := resource.validate(); err != nil {
			return err
		}
		kind := resource.Kind
		if kind == "TargetGeneration" {
			kind = "Target"
		}
		key := resource.Namespace + "/" + kind + "/" + resource.Name
		if kind == "MembershipGrant" {
			key += "/" + resource.ID
		}
		if seen[key] {
			return errors.New("resource document repeats a managed resource")
		}
		seen[key] = true
	}
	return nil
}

func (resource Resource) validate() error {
	switch resource.Kind {
	case "NamespacePolicy":
		if resource.Policy == nil || resource.Target != nil || resource.Grant != nil || resource.Name != "" || resource.ID != "" || resource.ExpectedGenerationID != "" || resource.ExpectedRevision < 1 {
			return errors.New("namespace policy needs a revision and only policy fields")
		}
		p := resource.Policy
		idempotency, err := time.ParseDuration(p.IdempotencyRetention)
		outbox, outboxErr := time.ParseDuration(p.PublishedOutboxRetention)
		if err != nil || outboxErr != nil || p.MaxActiveJobs < 1 || p.MaxActiveJobs > 2147483647 || p.MaxQueuedJobs < 1 || p.MaxQueuedJobs > 2147483647 || p.MaxCollectionItems < 1 || p.MaxCollectionItems > 10000 || p.MaxGraphNodes < 1 || p.MaxGraphNodes > 10000 || idempotency < time.Hour || idempotency > 365*24*time.Hour || outbox < time.Hour || outbox > 365*24*time.Hour || idempotency%time.Second != 0 || outbox%time.Second != 0 {
			return errors.New("namespace policy is outside supported bounds")
		}
	case "Target", "TargetGeneration":
		if resource.Target == nil || resource.Policy != nil || resource.Grant != nil || !domain.ValidName(resource.Name) {
			return errors.New("target resource fields are invalid")
		}
		if resource.Kind == "Target" {
			if resource.ID != "" || resource.ExpectedGenerationID != "" || resource.ExpectedRevision != 0 {
				return errors.New("new target cannot contain existing identity")
			}
		} else if !domain.IsID(resource.ID) || !domain.IsID(resource.ExpectedGenerationID) || resource.ExpectedRevision < 1 {
			return errors.New("target generation needs existing target identity, generation and revision")
		}
		if err := domain.ValidateTargetSpec(resource.targetSpec()); err != nil {
			return errors.New("target policy is invalid")
		}
	case "MembershipGrant":
		if resource.Grant == nil || resource.Policy != nil || resource.Target != nil || !domain.IsID(resource.ID) || resource.Name != "" || resource.ExpectedGenerationID != "" || resource.ExpectedRevision != 0 {
			return errors.New("membership resource needs an immutable grant ID and only grant fields")
		}
		p := resource.Grant.Principal
		if err := domain.ValidateMembershipGrant(domain.MembershipGrant{Issuer: p.Issuer, Subject: p.Subject, DisplayName: p.DisplayName, Role: resource.Grant.Role}); err != nil {
			return errors.New("membership grant is invalid")
		}
	default:
		return errors.New("unsupported resource kind; namespace creation, deletion and recovery changes are not supported")
	}
	return nil
}

func (resource Resource) targetSpec() domain.TargetSpec {
	t := resource.Target
	spec := domain.TargetSpec{Name: resource.Name, Kind: t.Kind, ExecutionBackend: t.ExecutionBackend, Runtimes: t.Runtimes, OperatingSystems: t.OperatingSystems, Architectures: t.Architectures, Capabilities: t.Capabilities, Partitions: t.Partitions, Provider: t.Provider}
	if t.LogStore != nil {
		spec.LogStoreName = t.LogStore.Name
		spec.LogStoreVersion = t.LogStore.Version
	}
	for _, s := range t.ArtifactStores {
		spec.ArtifactStores = append(spec.ArtifactStores, domain.ArtifactStoreSpec{Name: s.Name, Version: s.Version})
	}
	return spec
}

// Digest binds review/apply to all values, identities and operation ordering.
func (document Document) Digest() (string, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return "", errors.New("cannot encode resource document")
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
