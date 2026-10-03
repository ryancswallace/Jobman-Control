package postgres

import (
	"context"
	"errors"
	"math"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ryancswallace/jobman/diagnostic"

	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

// EnableDiagnosticSnapshots sets immutable process configuration before serving.
// Every replica must use the same operator-assigned Dashboard source UUID.
func (store *Store) EnableDiagnosticSnapshots(deploymentID string) error {
	if deploymentID != "" && !domain.IsID(deploymentID) {
		return errors.New("invalid diagnostic deployment identity")
	}
	store.diagnosticDeploymentID = deploymentID
	return nil
}

func validDiagnosticSelection(selection diagnostic.SharedSelection) bool {
	return domain.IsID(selection.DeploymentID) && domain.IsID(selection.ControlInstanceID) && domain.IsID(selection.NamespaceID) && domain.IsID(selection.JobID) && selection.ExpectedJobRevision <= math.MaxInt64 && (selection.RunID == "" || domain.IsID(selection.RunID))
}

// ReadDiagnosticSnapshot never collects raw commands, paths, environments or log
// bytes. The selected factual metadata and authority share one database snapshot.
func (store *Store) ReadDiagnosticSnapshot(ctx context.Context, principal domain.Principal, namespace string, selection diagnostic.SharedSelection) (domain.DiagnosticSnapshot, error) {
	if !validDiagnosticSelection(selection) {
		return domain.DiagnosticSnapshot{}, errors.New("invalid diagnostic selection")
	}
	if store.diagnosticDeploymentID == "" {
		return domain.DiagnosticSnapshot{}, domain.ErrFeatureUnavailable
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.DiagnosticSnapshot, error) {
		result := domain.DiagnosticSnapshot{}
		authority, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityEvidenceRead)
		if err != nil {
			return result, err
		}
		result.NamespaceReadAuthority, err = readNamespaceAuthority(ctx, tx, authority)
		if err != nil {
			return result, err
		}
		result.AsOf = result.AsOf.UTC()
		result.AuthorizationCheckedAt = result.AuthorizationCheckedAt.UTC()
		var instance string
		if sourceErr := tx.QueryRow(ctx, `SELECT id::text FROM control_instance WHERE singleton`).Scan(&instance); sourceErr != nil {
			return result, sourceErr
		}
		if selection.DeploymentID != store.diagnosticDeploymentID || selection.ControlInstanceID != instance || selection.NamespaceID != authority.namespaceID {
			return result, domain.ErrConflict
		}
		builder := newDiagnosticBuilder(store.diagnosticDeploymentID, instance, authority.namespaceID, result.AsOf)
		if readErr := builder.readJob(ctx, tx, selection); readErr != nil {
			return result, readErr
		}
		if readErr := builder.readRuns(ctx, tx, selection); readErr != nil {
			return result, readErr
		}
		if readErr := builder.readEvents(ctx, tx); readErr != nil {
			return result, readErr
		}
		if readErr := builder.readDependencies(ctx, tx, authority.namespaceID); readErr != nil {
			return result, readErr
		}
		if readErr := builder.readLogs(ctx, tx); readErr != nil {
			return result, readErr
		}
		result.Snapshot, err = builder.finish()
		return result, err
	})
}

type diagnosticBuilder struct {
	snapshot  diagnostic.SharedSnapshot
	omissions map[string][]string
	err       error
}

func newDiagnosticBuilder(deploymentID, instanceID, namespaceID string, asOf time.Time) *diagnosticBuilder {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == "github.com/ryancswallace/jobman" {
				version = dependency.Version
				break
			}
		}
	}
	return &diagnosticBuilder{snapshot: diagnostic.SharedSnapshot{
		Kind: diagnostic.SharedSnapshotKind, SchemaVersion: diagnostic.SharedSnapshotVersion, CapturedAt: asOf.UTC(),
		Source:        diagnostic.SharedSource{Kind: diagnostic.SharedSourceControl, DeploymentID: deploymentID, ControlInstanceID: instanceID, NamespaceID: namespaceID, ControlVersion: buildinfo.Version, ContractVersion: "jobman.control/v1alpha1"},
		JobmanVersion: version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Metadata: diagnostic.MetadataTransactionalSnapshot,
		Runs: []diagnostic.SharedRun{}, Items: []diagnostic.Item{}, Logs: []diagnostic.SharedLogReference{}, Omissions: []diagnostic.Omission{}, RedactionNotices: []diagnostic.RedactionNotice{},
	}, omissions: map[string][]string{}}
}

func (builder *diagnosticBuilder) omit(code string, affects ...string) {
	builder.omissions[code] = append(builder.omissions[code], affects...)
}

func (builder *diagnosticBuilder) add(id, code, entity string, value any, observed *time.Time, quality diagnostic.Quality) {
	if builder.err != nil {
		return
	}
	encoded, err := diagnostic.JSONValue(value)
	if err != nil {
		builder.err = err
		return
	}
	source := diagnostic.ItemSource{Kind: "control", EntityID: entity}
	if entity == builder.snapshot.Job.ID {
		source.Revision = builder.snapshot.Job.Revision
	}
	if observed != nil {
		utc := observed.UTC()
		observed = &utc
	}
	builder.snapshot.Items = append(builder.snapshot.Items, diagnostic.Item{ID: id, Code: code, Value: encoded, ObservedAt: observed, Source: source, Quality: quality, Disclosure: diagnostic.DisclosureMetadata})
}

func (builder *diagnosticBuilder) timestamp(id, code, entity string, value *time.Time) {
	if value == nil {
		builder.omit("lifecycle_observation_unavailable", id)
		return
	}
	builder.add(id, code, entity, value.UTC(), value, diagnostic.QualityObserved)
}

func (builder *diagnosticBuilder) finish() (diagnostic.SharedSnapshot, error) {
	if builder.err != nil {
		return diagnostic.SharedSnapshot{}, builder.err
	}
	slices.SortFunc(builder.snapshot.Items, func(a, b diagnostic.Item) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(builder.snapshot.Runs, func(a, b diagnostic.SharedRun) int {
		if a.Number < b.Number {
			return -1
		}
		if a.Number > b.Number {
			return 1
		}
		return 0
	})
	slices.SortFunc(builder.snapshot.Logs, func(a, b diagnostic.SharedLogReference) int { return strings.Compare(a.ID, b.ID) })
	for code, affects := range builder.omissions {
		slices.Sort(affects)
		builder.snapshot.Omissions = append(builder.snapshot.Omissions, diagnostic.Omission{Code: code, Affects: slices.Compact(affects)})
	}
	slices.SortFunc(builder.snapshot.Omissions, func(a, b diagnostic.Omission) int { return strings.Compare(a.Code, b.Code) })
	if err := diagnostic.ValidateSharedSnapshot(builder.snapshot); err != nil {
		return diagnostic.SharedSnapshot{}, err
	}
	return builder.snapshot, nil
}

func (builder *diagnosticBuilder) readJob(ctx context.Context, tx pgx.Tx, selection diagnostic.SharedSelection) error {
	var revision int64
	var desired, confidence string
	var imported bool
	var created time.Time
	var started, completed *time.Time
	err := tx.QueryRow(ctx, `SELECT j.id::text,j.revision,j.phase,COALESCE(j.outcome,''),j.desired_state,j.imported,j.created_at,j.started_at,j.completed_at,COALESCE(current_execution.observation_confidence,'')
 FROM jobs j LEFT JOIN LATERAL (SELECT e.observation_confidence FROM runs r LEFT JOIN executions e ON e.run_id=r.id WHERE r.job_id=j.id ORDER BY r.run_number DESC LIMIT 1) current_execution ON true WHERE j.namespace_id=$1 AND j.id=$2`, selection.NamespaceID, selection.JobID).Scan(&builder.snapshot.Job.ID, &revision, &builder.snapshot.Job.Phase, &builder.snapshot.Job.Outcome, &desired, &imported, &created, &started, &completed, &confidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if revision < 1 {
		return domain.ErrConflict
	}
	builder.snapshot.Job.Revision = uint64(revision)
	if selection.ExpectedJobRevision != 0 && selection.ExpectedJobRevision != builder.snapshot.Job.Revision {
		return domain.ErrConflict
	}
	id := builder.snapshot.Job.ID
	prefix := "job." + id + "."
	for _, fact := range []struct{ name, code, value string }{{"phase", diagnostic.CodeJobPhase, builder.snapshot.Job.Phase}, {"desired", diagnostic.CodeSharedDesiredState, desired}, {"outcome", diagnostic.CodeJobOutcome, builder.snapshot.Job.Outcome}, {"confidence", diagnostic.CodeSharedObservationConfidence, confidence}} {
		if fact.value != "" {
			builder.add(prefix+fact.name, fact.code, id, fact.value, nil, diagnostic.QualityObserved)
		}
	}
	builder.add(prefix+"revision", diagnostic.CodeJobRevision, id, builder.snapshot.Job.Revision, nil, diagnostic.QualityObserved)
	if imported {
		builder.omit("imported_submission_time_unavailable", prefix+"submitted_at")
	} else {
		builder.timestamp(prefix+"submitted_at", diagnostic.CodeJobSubmittedAt, id, &created)
	}
	builder.timestamp(prefix+"started_at", diagnostic.CodeJobStartedAt, id, started)
	builder.timestamp(prefix+"completed_at", diagnostic.CodeJobCompletedAt, id, completed)
	builder.omit(diagnostic.OmissionCommandNotRequested, "command")
	builder.omit(diagnostic.OmissionPathsNotRequested, "paths")
	builder.omit(diagnostic.OmissionEnvironmentNamesNotRequested, "environment")
	builder.omit(diagnostic.OmissionSystemContextNotRequested, "system")
	return nil
}
