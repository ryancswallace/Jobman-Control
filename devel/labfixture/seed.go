package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	protocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/contracts"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func seed(ctx context.Context, store *postgres.Store, input fixtureInput, logRoot string) (fixtureInfo, error) {
	var info fixtureInfo
	principals := []domain.Principal{}
	for _, user := range input.Users {
		principal := domain.Principal{Issuer: input.Issuer, Subject: user.Subject}
		principals = append(principals, principal)
		for _, namespace := range []string{"dashboard-research", "dashboard-operations"} {
			if err := store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{Principal: principal, DisplayName: user.Name, Namespace: namespace, Mode: "synthetic-dashboard-lab"}); err != nil {
				return info, err
			}
		}
		access, err := store.CurrentPrincipal(ctx, principal, "", 100)
		if err != nil {
			return info, err
		}
		info.Identities = append(info.Identities, domain.DirectoryIdentity{DirectoryID: user.DirectoryID, PrincipalID: access.PrincipalID, Issuer: input.Issuer, Subject: user.Subject, DisplayName: user.Name})
	}
	for _, namespace := range []string{"dashboard-research", "dashboard-operations"} {
		target, err := store.CreateTarget(ctx, principals[0], namespace, "fixture-host-v1", fixtureDigest(namespace+"host"), domain.TargetSpec{Name: fixtureTarget, Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64", "arm64"}, Capabilities: []string{"process-groups"}, LogStoreName: fixtureStore, LogStoreVersion: 1})
		if err != nil {
			return info, err
		}
		ns := fixtureNamespace{Name: namespace, TargetGenerationID: target.Value.GenerationID}
		access, err := store.CurrentPrincipal(ctx, principals[0], "", 100)
		if err != nil {
			return info, err
		}
		for _, scope := range access.Namespaces {
			if scope.Name == namespace {
				ns.ID = scope.ID
			}
		}
		identity, err := seedAgent(ctx, store, principals[0], namespace, target.Value.GenerationID)
		if err != nil {
			return info, err
		}
		for _, kind := range []string{"completed-output", "completed-empty", "open-zero-prefix"} {
			id, seedErr := seedExecution(ctx, store, principals[0], identity, namespace, kind, logRoot)
			if seedErr != nil {
				return info, seedErr
			}
			ns.JobIDs = append(ns.JobIDs, id)
		}
		submission, err := seedSubmission(namespace, "bob-awaiting", fixtureTarget)
		if err != nil {
			return info, err
		}
		pending, err := store.SubmitJob(ctx, principals[1], "fixture-bob-awaiting-v1", submission)
		if err != nil {
			return info, err
		}
		ns.JobIDs = append(ns.JobIDs, pending.Job.ID)
		submission, err = seedSubmission(namespace, "imported-failure", fixtureTarget)
		if err != nil {
			return info, err
		}
		history := domain.CompletedHistoryImport{Job: submission, Outcome: "failure", CompletedAt: time.Now().UTC().Add(-time.Hour), SourceStore: "sqlite", SourceSchema: 1, SourceJobID: "synthetic-" + namespace + "-history", RequestDigest: fixtureDigest(namespace + "history-v1"), RequestDocument: json.RawMessage(`{"synthetic":true}`)}
		imported, err := store.ImportCompletedHistory(ctx, principals[0], "fixture-history-v1", false, history)
		if err != nil {
			return info, err
		}
		ns.JobIDs = append(ns.JobIDs, imported.Job.ID)
		if operationErr := seedGroups(ctx, store, principals[0], &ns); operationErr != nil {
			return info, operationErr
		}
		info.Namespaces = append(info.Namespaces, ns)
	}
	return info, nil
}

func seedAgent(ctx context.Context, store *postgres.Store, principal domain.Principal, namespace, generation string) (domain.AgentIdentity, error) {
	enrollment, err := store.CreateEnrollmentToken(ctx, principal, namespace, fixtureTarget, "fixture-agent-v1", fixtureDigest(namespace+"enrollment"), domain.EnrollmentRequest{Principal: principal, ExpectedUser: "synthetic", Lifetime: 10 * time.Minute})
	if err != nil {
		return domain.AgentIdentity{}, err
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{TargetGenerationID: generation, AgentVersion: "synthetic-dashboard-lab", ProtocolVersions: []string{"jobman/v1alpha1"}, OperatingSystem: "linux", Architecture: "amd64", Hostname: "synthetic-control-fixture", ExecutionUser: "synthetic", ExecutionBackends: []string{"subprocess"}, Runtimes: []string{"native"}, Capabilities: []string{"process-groups"}, RequestDigest: fixtureDigest(namespace + "agent")}, time.Hour)
	if err != nil {
		return domain.AgentIdentity{}, err
	}
	return store.AuthenticateAgent(ctx, session.Token)
}

func seedExecution(ctx context.Context, store *postgres.Store, principal domain.Principal, identity domain.AgentIdentity, namespace, kind, logRoot string) (string, error) {
	submission, err := seedSubmission(namespace, kind, fixtureTarget)
	if err != nil {
		return "", err
	}
	created, err := store.SubmitJob(ctx, principal, "fixture-"+kind+"-v1", submission)
	if err != nil {
		return "", err
	}
	if _, err = store.ReconcileAssignments(ctx, 100); err != nil {
		return "", err
	}
	assignments, err := store.ListAssignments(ctx, identity, 100)
	if err != nil {
		return "", err
	}
	var assignment domain.Assignment
	for _, candidate := range assignments {
		decodedCandidate, decodeErr := protocol.DecodeAgentAssignment(bytes.NewReader(candidate.Document), protocol.DecodeLimits{})
		if decodeErr != nil {
			return "", decodeErr
		}
		if decodedCandidate.Document.Spec.EffectiveExecution.Metadata.JobID == created.Job.ID {
			assignment = candidate
		}
	}
	if assignment.ExecutionID == "" {
		return "", errors.New("synthetic execution assignment absent")
	}
	decoded, err := protocol.DecodeAgentAssignment(bytes.NewReader(assignment.Document), protocol.DecodeLimits{})
	if err != nil {
		return "", err
	}
	accepted, err := protocol.SealAgentAcceptance(protocol.AgentAcceptance{APIVersion: protocol.V1Alpha1, Kind: protocol.AgentAcceptanceKind, Metadata: protocol.AgentAcceptanceMetadata{DeliveryID: assignment.DeliveryID, ExecutionID: assignment.ExecutionID, AgentID: identity.AgentID}, Spec: protocol.AgentAcceptanceSpec{TargetGenerationID: identity.TargetGenerationID, EffectiveExecutionDigest: decoded.EffectiveExecutionDigest}})
	if err != nil {
		return "", err
	}
	if _, err = store.AcceptAssignment(ctx, identity, domain.Acceptance{DeliveryID: assignment.DeliveryID, ExecutionID: assignment.ExecutionID, AgentID: identity.AgentID, TargetGenerationID: identity.TargetGenerationID, EffectiveExecutionDigest: decoded.EffectiveExecutionDigest, RequestDigest: accepted.Digest, RequestDocument: accepted.CanonicalJSON}); err != nil {
		return "", err
	}
	if operationErr := seedObservation(ctx, store, identity, assignment.ExecutionID, 1, "process.started", nil); operationErr != nil {
		return "", operationErr
	}
	if kind == "open-zero-prefix" {
		// Only an out-of-order terminal marker is known. The published contiguous
		// prefix is correctly empty/open until sequence one arrives.
		err = seedChunk(ctx, store, identity, namespace, created.Job.ID, assignment.ExecutionID, "stdout", 2, 3, nil, true, logRoot)
		return created.Job.ID, err
	}
	next, offset := int64(1), int64(0)
	if kind == "completed-output" {
		data := []byte("SYNTHETIC Dashboard Lab log: metadata and byte delivery acceptance only.\n")
		if operationErr := seedChunk(ctx, store, identity, namespace, created.Job.ID, assignment.ExecutionID, "stdout", next, offset, data, false, logRoot); operationErr != nil {
			return "", operationErr
		}
		next++
		offset = int64(len(data))
	}
	if operationErr := seedChunk(ctx, store, identity, namespace, created.Job.ID, assignment.ExecutionID, "stdout", next, offset, nil, true, logRoot); operationErr != nil {
		return "", operationErr
	}
	if operationErr := seedChunk(ctx, store, identity, namespace, created.Job.ID, assignment.ExecutionID, "stderr", 1, 0, nil, true, logRoot); operationErr != nil {
		return "", operationErr
	}
	exitCode := 0
	if operationErr := seedObservation(ctx, store, identity, assignment.ExecutionID, 2, "process.completed", &protocol.ProcessResult{Outcome: "success", ExitCode: &exitCode}); operationErr != nil {
		return "", operationErr
	}
	return created.Job.ID, nil
}

func seedObservation(ctx context.Context, store *postgres.Store, identity domain.AgentIdentity, execution string, sequence int64, kind string, result *protocol.ProcessResult) error {
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	native := ""
	if kind == "process.started" {
		native = "synthetic-1"
	}
	sealed, err := protocol.SealExecutionEvent(protocol.ExecutionEvent{APIVersion: protocol.V1Alpha1, Kind: protocol.ExecutionEventKind, Metadata: protocol.ExecutionEventMetadata{EventID: id, ExecutionID: execution, AgentID: identity.AgentID, Sequence: sequence, ObservedAt: time.Now().UTC()}, Spec: protocol.ExecutionEventSpec{Type: kind, NativeID: native, Result: result}})
	if err != nil {
		return err
	}
	outcome := ""
	if result != nil {
		outcome = result.Outcome
	}
	_, err = store.RecordExecutionEvent(ctx, identity, domain.ExecutionObservation{EventID: id, ExecutionID: execution, AgentID: identity.AgentID, Sequence: sequence, ObservedAt: sealed.Document.Metadata.ObservedAt, Type: kind, NativeID: native, Outcome: outcome, DocumentDigest: sealed.Digest, Document: sealed.CanonicalJSON})
	return err
}

func seedChunk(ctx context.Context, store *postgres.Store, identity domain.AgentIdentity, namespace, job, execution, stream string, sequence, offset int64, data []byte, complete bool, root string) error {
	key := fmt.Sprintf("namespaces/%s/jobs/%s/executions/%s/logs/%s/%08d.chunk", namespace, job, execution, stream, sequence)
	path := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	// #nosec G302 -- Synthetic nonsecret log bytes deliberately retain group-read ACL mask.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if operationErr := errors.Join(writeErr, file.Close()); operationErr != nil {
		return operationErr
	}
	chunk := domain.LogChunk{ExecutionID: execution, AgentID: identity.AgentID, Stream: stream, Sequence: sequence, StoreName: fixtureStore, StoreVersion: 1, ObjectKey: key, ByteOffset: offset, ByteLength: int64(len(data)), Checksum: fixtureDigest(string(data)), CapturedAt: time.Now().UTC(), Complete: complete}
	document, err := json.Marshal(map[string]any{"synthetic": true, "executionId": execution, "agentId": identity.AgentID, "stream": stream, "sequence": sequence, "storeName": fixtureStore, "storeVersion": 1, "objectKey": key, "byteOffset": offset, "byteLength": len(data), "checksum": chunk.Checksum, "capturedAt": chunk.CapturedAt, "complete": complete})
	if err != nil {
		return err
	}
	chunk.Document = document
	chunk.DocumentDigest = fixtureDigest(string(document))
	_, err = store.CommitLogChunk(ctx, identity, chunk)
	return err
}

func fixtureWorkload() protocol.Workload {
	return protocol.Workload{APIVersion: protocol.V1Alpha1, Kind: protocol.WorkloadKind, Spec: protocol.WorkloadSpec{Command: protocol.Command{Executable: "true"}, Runtime: protocol.Runtime{Kind: "native"}, Policy: protocol.ExecutionPolicy{Retry: protocol.RetryPolicy{MaxRuns: 1}, DuplicateRisk: "reject"}}}
}

func seedSubmission(namespace, name, target string) (domain.JobSubmission, error) {
	sealed, err := protocol.SealJobRequest(protocol.JobRequest{APIVersion: protocol.V1Alpha1, Kind: protocol.JobRequestKind, Metadata: protocol.JobRequestMetadata{Namespace: namespace, Name: name, Labels: map[string]string{"fixture": "synthetic-dashboard-lab"}}, Spec: protocol.JobRequestSpec{Workload: protocol.WorkloadBinding{Document: fixtureWorkload()}, Placement: protocol.Placement{Target: target}}})
	if err != nil {
		return domain.JobSubmission{}, err
	}
	decoded, err := contracts.DecodeJobRequest(bytes.NewReader(sealed.CanonicalJSON))
	if err != nil {
		return domain.JobSubmission{}, err
	}
	return submissionFrom(decoded), nil
}

func submissionFrom(decoded contracts.JobRequest) domain.JobSubmission {
	return domain.JobSubmission{Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels, Target: decoded.Target, Partition: decoded.Partition, RuntimeKind: decoded.RuntimeKind, OperatingSystems: decoded.OperatingSystems, Architectures: decoded.Architectures, Capabilities: decoded.Capabilities, ArtifactStores: decoded.ArtifactStores, WorkloadDigest: decoded.WorkloadDigest, WorkloadDocument: decoded.WorkloadDocument, RequestDigest: decoded.RequestDigest, RequestDocument: decoded.RequestDocument, ExecutionFeatures: domain.ExecutionFeatures{DirectCommand: decoded.ExecutionFeatures.DirectCommand, Resources: decoded.ExecutionFeatures.Resources, TemporaryStorage: decoded.ExecutionFeatures.TemporaryStorage, Artifacts: decoded.ExecutionFeatures.Artifacts, Extensions: decoded.ExecutionFeatures.Extensions, EnvironmentProfile: decoded.ExecutionFeatures.EnvironmentProfile, Secrets: decoded.ExecutionFeatures.Secrets, RetryMaxRuns: decoded.ExecutionFeatures.RetryMaxRuns, SchedulerEnvironmentOverride: decoded.ExecutionFeatures.SchedulerEnvironmentOverride}}
}
