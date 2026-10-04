package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	protocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

const (
	diagnosticDeployment   = "72000000-0000-4000-8000-000000000001"
	diagnosticTarget       = "synthetic-diagnostics"
	diagnosticNamespace    = "dashboard-operations"
	diagnosticLog          = "SYNTHETIC OBSERVATION ONLY: open synthetic-output.txt: permission denied; metadata and byte delivery\n"
	diagnosticManifestName = "diagnostic-fixture.json"
	diagnosticReceiptName  = ".diagnostic-prepare.json"
)

type diagnosticFixture struct {
	Synthetic          bool                  `json:"synthetic"`
	FixtureVersion     int                   `json:"fixtureVersion"`
	ObservationMode    string                `json:"observationMode"`
	HelperCommit       string                `json:"helperCommit"`
	DeploymentID       string                `json:"deploymentId"`
	ControlInstanceID  string                `json:"controlInstanceId"`
	RecoveryEpoch      string                `json:"recoveryEpoch"`
	NamespaceID        string                `json:"namespaceId"`
	Namespace          string                `json:"namespace"`
	TargetID           string                `json:"targetId"`
	TargetName         string                `json:"targetName"`
	TargetGenerationID string                `json:"targetGenerationId"`
	JobID              string                `json:"jobId"`
	JobRevision        string                `json:"jobRevision"`
	RunID              string                `json:"runId"`
	ExecutionID        string                `json:"executionId"`
	Streams            []domain.LogChunkPage `json:"streams"`
}

func readDiagnosticPrivate(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("diagnostic fixture requires bounded private regular files")
	}
	return readBounded(path, limit)
}

func diagnosticEnvironment(data []byte) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		name, encoded, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(name, "JOBMAN_CONTROL_") || values[name] != "" {
			return nil, errors.New("invalid private fixture environment")
		}
		var value string
		if json.Unmarshal([]byte(encoded), &value) != nil || value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("invalid private fixture environment value")
		}
		values[name] = value
	}
	return values, nil
}

func prepareDiagnostic(ctx context.Context, root, databasePath, logRoot, deployment string) error {
	if deployment != diagnosticDeployment || !filepath.IsAbs(logRoot) || filepath.Clean(logRoot) != logRoot || logRoot == "/" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("explicit isolated diagnostic paths and deployment required")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 {
		return errors.New("private real fixture directory required")
	}
	spoolInfo, err := os.Lstat(logRoot)
	if err != nil || !spoolInfo.IsDir() || spoolInfo.Mode().Perm() != 0o750 || strings.HasPrefix(logRoot+"/", root+"/") || strings.HasPrefix(root+"/", logRoot+"/") {
		return errors.New("separate real synthetic spool with mode 0750 required")
	}
	for _, pattern := range []string{".directory-acceptance-*.json", diagnosticReceiptName} {
		matches, globErr := filepath.Glob(filepath.Join(root, pattern))
		if globErr != nil || len(matches) != 0 {
			return errors.New("pending fixture operation requires inspection before diagnostic preparation")
		}
	}
	var input fixtureInput
	var original fixtureInfo
	if readJSON(filepath.Join(root, "fixture-input.json"), &input) != nil || validateInput(input) != nil || readJSON(filepath.Join(root, "fixture-info.json"), &original) != nil || !original.Synthetic || !domain.IsID(original.InstanceID) || original.Issuer != input.Issuer {
		return errors.New("completed original synthetic fixture required")
	}
	data, err := readDiagnosticPrivate(filepath.Join(root, "control.env"), 65536)
	if err != nil {
		return err
	}
	environment, err := diagnosticEnvironment(data)
	if err != nil {
		return err
	}
	database, err := readDiagnosticPrivate(databasePath, 16384)
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(string(database))
	endpoint, err := url.Parse(dsn)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || endpoint.Path != "/"+fixtureDatabase || endpoint.Query().Get("sslmode") != "verify-full" || environment["JOBMAN_CONTROL_DATABASE_URL"] != dsn || environment["JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID"] != deployment || environment["JOBMAN_CONTROL_DIRECTORY_MODE"] != "enforce" || environment["JOBMAN_CONTROL_MIGRATE_ON_START"] != "false" {
		return errors.New("diagnostic fixture requires matching isolated source configuration")
	}
	key, err := base64.RawURLEncoding.DecodeString(environment["JOBMAN_CONTROL_AGENT_TOKEN_KEY"])
	if err != nil || len(key) != 32 {
		return errors.New("invalid private fixture token key")
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	var databaseName, instance string
	if queryErr := pool.QueryRow(ctx, "SELECT current_database(),id::text FROM control_instance").Scan(&databaseName, &instance); queryErr != nil || databaseName != fixtureDatabase || instance != original.InstanceID {
		return errors.New("dedicated database and source instance preflight failed")
	}
	if operationErr := postgres.CheckMigrations(ctx, pool); operationErr != nil {
		return operationErr
	}
	store := postgres.New(pool, key)
	principal := domain.Principal{Issuer: input.Issuer, Subject: input.Users[0].Subject}
	manifestPath := filepath.Join(root, diagnosticManifestName)
	if _, err = os.Lstat(manifestPath); err == nil {
		var existing diagnosticFixture
		if readJSON(manifestPath, &existing) != nil {
			return errors.New("invalid completed diagnostic fixture")
		}
		return verifyDiagnostic(ctx, store, principal, original, existing, logRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(logRoot)
	if err != nil || len(entries) != 0 {
		return errors.New("new diagnostic preparation requires an empty dedicated spool")
	}
	// An exclusive durable receipt precedes the first database mutation. Failed
	// runs require operator inspection; no reset, overwrite or automatic replay.
	if operationErr := writeDiagnosticJSON(root, diagnosticReceiptName, map[string]any{"synthetic": true, "instanceId": instance, "deploymentId": deployment}); operationErr != nil {
		return operationErr
	}
	prepared, err := seedDiagnostic(ctx, store, principal, original, logRoot)
	if err != nil {
		return err
	}
	if operationErr := verifyDiagnostic(ctx, store, principal, original, prepared, logRoot); operationErr != nil {
		return operationErr
	}
	if operationErr := writeDiagnosticJSON(root, diagnosticManifestName, prepared); operationErr != nil {
		return operationErr
	}
	return os.Remove(filepath.Join(root, diagnosticReceiptName))
}

func writeDiagnosticJSON(root, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func seedDiagnostic(ctx context.Context, store *postgres.Store, principal domain.Principal, original fixtureInfo, logRoot string) (diagnosticFixture, error) {
	var result diagnosticFixture
	namespace := fixtureNamespace{}
	for _, candidate := range original.Namespaces {
		if candidate.Name == diagnosticNamespace {
			namespace = candidate
		}
	}
	if !domain.IsID(namespace.ID) {
		return result, errors.New("original operations namespace absent")
	}
	target, err := store.CreateTarget(ctx, principal, namespace.Name, "fixture-diagnostic-target-v1", fixtureDigest("diagnostic-target-v1"), domain.TargetSpec{Name: diagnosticTarget, Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64", "arm64"}, Capabilities: []string{"process-groups"}, LogStoreName: fixtureStore, LogStoreVersion: 1})
	if err != nil {
		return result, err
	}
	if target.Replayed {
		return result, errors.New("partial diagnostic target requires inspection")
	}
	enrollment, err := store.CreateEnrollmentToken(ctx, principal, namespace.Name, diagnosticTarget, "fixture-diagnostic-agent-v1", fixtureDigest("diagnostic-enrollment-v1"), domain.EnrollmentRequest{Principal: principal, ExpectedUser: "synthetic", Lifetime: 10 * time.Minute})
	if err != nil {
		return result, err
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{TargetGenerationID: target.Value.GenerationID, AgentVersion: "synthetic-dashboard-diagnostic-v1", ProtocolVersions: []string{"jobman/v1alpha1"}, OperatingSystem: "linux", Architecture: "arm64", Hostname: "synthetic-diagnostic-observations", ExecutionUser: "synthetic", ExecutionBackends: []string{"subprocess"}, Runtimes: []string{"native"}, Capabilities: []string{"process-groups"}, RequestDigest: fixtureDigest("diagnostic-agent-v1")}, 10*time.Minute)
	if err != nil {
		return result, err
	}
	identity, err := store.AuthenticateAgent(ctx, session.Token)
	if err != nil {
		return result, err
	}
	submission, err := seedSubmission(namespace.Name, "synthetic-permission-failure", diagnosticTarget)
	if err != nil {
		return result, err
	}
	created, err := store.SubmitJob(ctx, principal, "fixture-diagnostic-job-v1", submission)
	if err != nil {
		return result, err
	}
	assignment, err := awaitDiagnosticAssignment(ctx, store, identity, created.Job.ID)
	if err != nil {
		return result, err
	}
	decoded, err := protocol.DecodeAgentAssignment(bytes.NewReader(assignment.Document), protocol.DecodeLimits{})
	if err != nil {
		return result, err
	}
	accepted, err := protocol.SealAgentAcceptance(protocol.AgentAcceptance{APIVersion: protocol.V1Alpha1, Kind: protocol.AgentAcceptanceKind, Metadata: protocol.AgentAcceptanceMetadata{DeliveryID: assignment.DeliveryID, ExecutionID: assignment.ExecutionID, AgentID: identity.AgentID}, Spec: protocol.AgentAcceptanceSpec{TargetGenerationID: identity.TargetGenerationID, EffectiveExecutionDigest: decoded.EffectiveExecutionDigest}})
	if err != nil {
		return result, err
	}
	if _, err = store.AcceptAssignment(ctx, identity, domain.Acceptance{DeliveryID: assignment.DeliveryID, ExecutionID: assignment.ExecutionID, AgentID: identity.AgentID, TargetGenerationID: identity.TargetGenerationID, EffectiveExecutionDigest: decoded.EffectiveExecutionDigest, RequestDigest: accepted.Digest, RequestDocument: accepted.CanonicalJSON}); err != nil {
		return result, err
	}
	if operationErr := seedObservation(ctx, store, identity, assignment.ExecutionID, 1, "process.started", nil); operationErr != nil {
		return result, operationErr
	}
	for _, stream := range []string{"stdout", "stderr"} {
		var data []byte
		if stream == "stderr" {
			data = []byte(diagnosticLog)
		}
		if operationErr := seedChunk(ctx, store, identity, namespace.Name, created.Job.ID, assignment.ExecutionID, stream, 1, 0, data, true, logRoot); operationErr != nil {
			return result, operationErr
		}
	}
	exitCode := 1
	if operationErr := seedObservation(ctx, store, identity, assignment.ExecutionID, 2, "process.completed", &protocol.ProcessResult{Outcome: "failure", ExitCode: &exitCode}); operationErr != nil {
		return result, operationErr
	}
	job, err := store.GetJob(ctx, principal, namespace.Name, created.Job.ID)
	if err != nil {
		return result, err
	}
	if job.CurrentRun == nil {
		return result, errors.New("diagnostic run absent")
	}
	capabilities, err := store.Capabilities(ctx)
	if err != nil {
		return result, err
	}
	result = diagnosticFixture{Synthetic: true, FixtureVersion: 1, ObservationMode: "synthetic-store-observations-no-execution", HelperCommit: buildinfo.Commit, DeploymentID: diagnosticDeployment, ControlInstanceID: original.InstanceID, RecoveryEpoch: capabilities.RecoveryEpoch, NamespaceID: namespace.ID, Namespace: namespace.Name, TargetID: target.Value.ID, TargetName: diagnosticTarget, TargetGenerationID: target.Value.GenerationID, JobID: job.ID, JobRevision: strconv.FormatInt(job.Revision, 10), RunID: job.CurrentRun.ID, ExecutionID: assignment.ExecutionID}
	for _, stream := range []string{"stdout", "stderr"} {
		page, err := store.ListLogChunks(ctx, principal, namespace.Name, job.ID, domain.LogChunkOptions{Stream: stream, Limit: 10})
		if err != nil {
			return result, err
		}
		result.Streams = append(result.Streams, page)
	}
	return result, nil
}

func awaitDiagnosticAssignment(ctx context.Context, store *postgres.Store, identity domain.AgentIdentity, job string) (domain.Assignment, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		assignments, err := store.ListAssignments(ctx, identity, 10)
		if err != nil {
			return domain.Assignment{}, err
		}
		for _, assignment := range assignments {
			decoded, err := protocol.DecodeAgentAssignment(bytes.NewReader(assignment.Document), protocol.DecodeLimits{})
			if err != nil {
				return domain.Assignment{}, err
			}
			if decoded.Document.Spec.EffectiveExecution.Metadata.JobID == job {
				return assignment, nil
			}
			return domain.Assignment{}, errors.New("unexpected job assigned to dedicated synthetic agent")
		}
		select {
		case <-ctx.Done():
			return domain.Assignment{}, errors.New("normal coordinator did not assign synthetic diagnostic job within deadline")
		case <-tick.C:
		}
	}
}

func verifyDiagnostic(ctx context.Context, store *postgres.Store, principal domain.Principal, original fixtureInfo, fixture diagnosticFixture, logRoot string) error {
	if !fixture.Synthetic || fixture.FixtureVersion != 1 || fixture.ObservationMode != "synthetic-store-observations-no-execution" || fixture.DeploymentID != diagnosticDeployment || fixture.ControlInstanceID != original.InstanceID || fixture.Namespace != diagnosticNamespace || fixture.TargetName != diagnosticTarget || len(fixture.Streams) != 2 {
		return errors.New("diagnostic fixture manifest differs")
	}
	capabilities, err := store.Capabilities(ctx)
	if err != nil || capabilities.InstanceID != fixture.ControlInstanceID || capabilities.RecoveryEpoch != fixture.RecoveryEpoch {
		return errors.New("diagnostic fixture source identity changed")
	}
	job, err := store.GetJob(ctx, principal, fixture.Namespace, fixture.JobID)
	if err != nil {
		return err
	}
	if job.NamespaceID != fixture.NamespaceID || job.TargetID != fixture.TargetID || job.TargetGenerationID != fixture.TargetGenerationID || job.CurrentRun == nil || job.CurrentRun.ID != fixture.RunID || job.CurrentRun.ExecutionID != fixture.ExecutionID || job.Outcome != "failure" || strconv.FormatInt(job.Revision, 10) != fixture.JobRevision {
		return errors.New("diagnostic fixture job changed")
	}
	for index, stream := range []string{"stdout", "stderr"} {
		page, err := store.ListLogChunks(ctx, principal, fixture.Namespace, fixture.JobID, domain.LogChunkOptions{Stream: stream, Limit: 10})
		if err != nil {
			return err
		}
		var want []byte
		if stream == "stderr" {
			want = []byte(diagnosticLog)
		}
		stored := fixture.Streams[index]
		if page.State != "complete" || page.ByteLength != int64(len(want)) || page.RunID != fixture.RunID || page.ExecutionID != fixture.ExecutionID || page.TargetGenerationID != fixture.TargetGenerationID || page.NextAfterSequence != nil || len(page.Chunks) != 1 || stored.Stream != stream || stored.ManifestRevision != page.ManifestRevision || stored.ByteLength != page.ByteLength || stored.State != page.State || stored.RunID != page.RunID || stored.ExecutionID != page.ExecutionID || len(stored.Chunks) != 1 {
			return errors.New("diagnostic log manifest differs")
		}
		actualChunk, storedChunk := page.Chunks[0], stored.Chunks[0]
		if !actualChunk.CapturedAt.Equal(storedChunk.CapturedAt) {
			return errors.New("diagnostic chunk timestamp differs")
		}
		actualChunk.CapturedAt, storedChunk.CapturedAt = time.Time{}, time.Time{}
		if actualChunk != storedChunk {
			return errors.New("diagnostic chunk metadata differs")
		}
		chunk := page.Chunks[0]
		key := "namespaces/" + fixture.Namespace + "/jobs/" + fixture.JobID + "/executions/" + fixture.ExecutionID + "/logs/" + stream + "/00000001.chunk"
		if chunk.ObjectKey != key || chunk.ByteOffset != 0 || chunk.ByteLength != int64(len(want)) || chunk.Checksum != fixtureDigest(string(want)) || !chunk.Complete || chunk.StoreName != fixtureStore || chunk.StoreVersion != 1 {
			return errors.New("diagnostic chunk identity differs")
		}
		data, err := readBounded(filepath.Join(logRoot, filepath.FromSlash(key)), 4096)
		if err != nil || !bytes.Equal(data, want) {
			return errors.New("diagnostic immutable bytes differ")
		}
	}
	return nil
}
