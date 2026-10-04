package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

//nolint:misspell // Frozen v1alpha1 terminal outcome.
const notificationCancelled = "cancelled"

var notificationReceiptPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type notificationJob struct {
	Case  string `json:"case"`
	JobID string `json:"jobId"`
}
type notificationFixture struct {
	Synthetic         bool              `json:"synthetic"`
	FixtureVersion    int               `json:"fixtureVersion"`
	ObservationMode   string            `json:"observationMode"`
	HelperCommit      string            `json:"helperCommit"`
	Receipt           string            `json:"receipt"`
	DeploymentID      string            `json:"deploymentId"`
	ControlInstanceID string            `json:"controlInstanceId"`
	RecoveryEpoch     string            `json:"recoveryEpoch"`
	NamespaceID       string            `json:"namespaceId"`
	Namespace         string            `json:"namespace"`
	Jobs              []notificationJob `json:"jobs"`
}
type notificationCompletion struct {
	Synthetic         bool      `json:"synthetic"`
	Receipt           string    `json:"receipt"`
	Case              string    `json:"case"`
	DeploymentID      string    `json:"deploymentId"`
	ControlInstanceID string    `json:"controlInstanceId"`
	RecoveryEpoch     string    `json:"recoveryEpoch"`
	NamespaceID       string    `json:"namespaceId"`
	JobID             string    `json:"jobId"`
	EventID           string    `json:"eventId"`
	Outcome           string    `json:"outcome"`
	JobRevision       string    `json:"jobRevision"`
	RecordedAt        time.Time `json:"recordedAt"`
	RunID             string    `json:"runId,omitempty"`
	ExecutionID       string    `json:"executionId,omitempty"`
}

func notificationScenario(ctx context.Context, root, databasePath, deployment, receipt, action, selected string) (any, error) {
	if runtime.GOOS == "windows" || deployment != diagnosticDeployment || !notificationReceiptPattern.MatchString(receipt) || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || action != "prepare" && action != "complete" || action == "complete" && selected != "first" && selected != "stopped" || action == "prepare" && selected != "" {
		return nil, errors.New("explicit POSIX isolated notification scenario required")
	}
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		return nil, errors.New("private real fixture directory required")
	}
	for _, pattern := range []string{".directory-acceptance-*.json", diagnosticReceiptName} {
		matches, globErr := filepath.Glob(filepath.Join(root, pattern))
		if globErr != nil || len(matches) != 0 {
			return nil, errors.New("another fixture operation requires inspection")
		}
	}
	var input fixtureInput
	var original fixtureInfo
	if readJSON(filepath.Join(root, "fixture-input.json"), &input) != nil || validateInput(input) != nil || readJSON(filepath.Join(root, "fixture-info.json"), &original) != nil || !original.Synthetic || !domain.IsID(original.InstanceID) || original.Issuer != input.Issuer {
		return nil, errors.New("completed original synthetic fixture required")
	}
	data, err := readDiagnosticPrivate(filepath.Join(root, "control.env"), 65536)
	if err != nil {
		return nil, err
	}
	environment, err := diagnosticEnvironment(data)
	if err != nil {
		return nil, err
	}
	database, err := readDiagnosticPrivate(databasePath, 16384)
	if err != nil {
		return nil, err
	}
	dsn := strings.TrimSpace(string(database))
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql" || endpoint.Path != "/"+fixtureDatabase || endpoint.Query().Get("sslmode") != "verify-full" || environment["JOBMAN_CONTROL_DATABASE_URL"] != dsn || environment["JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID"] != deployment || environment["JOBMAN_CONTROL_DIRECTORY_MODE"] != "enforce" || environment["JOBMAN_CONTROL_MIGRATE_ON_START"] != "false" {
		return nil, errors.New("notification scenario requires matching isolated source configuration")
	}
	key, err := base64.RawURLEncoding.DecodeString(environment["JOBMAN_CONTROL_AGENT_TOKEN_KEY"])
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid private fixture token key")
	}
	pool, err := postgres.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	var databaseName, instance string
	if queryErr := pool.QueryRow(ctx, "SELECT current_database(),id::text FROM control_instance").Scan(&databaseName, &instance); queryErr != nil || databaseName != fixtureDatabase || instance != original.InstanceID {
		return nil, errors.New("dedicated database and source instance preflight failed")
	}
	if operationErr := postgres.CheckMigrations(ctx, pool); operationErr != nil {
		return nil, operationErr
	}
	store := postgres.New(pool, key)
	principal := domain.Principal{Issuer: input.Issuer, Subject: input.Users[0].Subject}
	name := ".notification-" + receipt + ".json"
	if action == "prepare" {
		if _, err = os.Lstat(filepath.Join(root, name)); err == nil {
			var prior notificationFixture
			if readJSON(filepath.Join(root, name), &prior) != nil || verifyNotificationFixture(ctx, store, principal, original, prior, receipt) != nil {
				return nil, errors.New("existing notification receipt differs")
			}
			return prior, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// Bound accumulation in the long-lived synthetic source. Partial receipt
		// files are retained for inspection; preparation never blindly retries.
		files, err := filepath.Glob(filepath.Join(root, ".notification-*.pending"))
		completed, completeErr := filepath.Glob(filepath.Join(root, ".notification-*.json"))
		if err != nil || completeErr != nil || len(files) != 0 || len(completed) >= 100 {
			return nil, errors.New("notification scenario capacity or unfinished preparation")
		}
		pending := ".notification-" + receipt + ".pending"
		if operationErr := writeDiagnosticJSON(root, pending, map[string]string{"receipt": receipt, "controlInstanceId": instance}); operationErr != nil {
			return nil, operationErr
		}
		created, err := seedNotificationScenario(ctx, store, principal, original, receipt)
		if err != nil {
			return nil, err
		}
		if operationErr := writeDiagnosticJSON(root, name, created); operationErr != nil {
			return nil, operationErr
		}
		if operationErr := os.Remove(filepath.Join(root, pending)); operationErr != nil {
			return nil, operationErr
		}
		return created, nil
	}
	var fixture notificationFixture
	if readJSON(filepath.Join(root, name), &fixture) != nil || verifyNotificationFixture(ctx, store, principal, original, fixture, receipt) != nil {
		return nil, errors.New("notification scenario receipt is invalid")
	}
	return completeNotificationScenario(ctx, pool, store, principal, fixture, selected)
}

func seedNotificationScenario(ctx context.Context, store *postgres.Store, principal domain.Principal, original fixtureInfo, receipt string) (notificationFixture, error) {
	caps, err := store.Capabilities(ctx)
	if err != nil {
		return notificationFixture{}, err
	}
	f := notificationFixture{Synthetic: true, FixtureVersion: 1, ObservationMode: "normal-cancel-no-execution", HelperCommit: buildinfo.Commit, Receipt: receipt, DeploymentID: diagnosticDeployment, ControlInstanceID: original.InstanceID, RecoveryEpoch: caps.RecoveryEpoch, Namespace: "dashboard-research", Jobs: []notificationJob{}}
	for _, ns := range original.Namespaces {
		if ns.Name == f.Namespace {
			f.NamespaceID = ns.ID
		}
	}
	if !domain.IsID(f.NamespaceID) || caps.InstanceID != original.InstanceID || !notificationReceiptPattern.MatchString(receipt) {
		return f, errors.New("notification source identity differs")
	}
	for _, selected := range []string{"first", "stopped"} {
		request, err := seedSubmission(f.Namespace, "synthetic-alert-"+receipt+"-"+selected, fixtureTarget)
		if err != nil {
			return f, err
		}
		created, err := store.SubmitJob(ctx, principal, "notification-"+receipt+"-"+selected, request)
		if err != nil {
			return f, err
		}
		f.Jobs = append(f.Jobs, notificationJob{Case: selected, JobID: created.Job.ID})
	}
	return f, verifyNotificationFixture(ctx, store, principal, original, f, receipt)
}

func verifyNotificationFixture(ctx context.Context, store *postgres.Store, principal domain.Principal, original fixtureInfo, f notificationFixture, receipt string) error {
	if !f.Synthetic || f.FixtureVersion != 1 || f.ObservationMode != "normal-cancel-no-execution" || f.Receipt != receipt || f.DeploymentID != diagnosticDeployment || f.ControlInstanceID != original.InstanceID || f.Namespace != "dashboard-research" || !domain.IsID(f.NamespaceID) || len(f.Jobs) != 2 || f.Jobs[0].Case != "first" || f.Jobs[1].Case != "stopped" || f.Jobs[0].JobID == f.Jobs[1].JobID {
		return errors.New("invalid notification fixture")
	}
	caps, err := store.Capabilities(ctx)
	if err != nil || caps.InstanceID != f.ControlInstanceID || caps.RecoveryEpoch != f.RecoveryEpoch {
		return errors.New("notification fixture source recovered or changed")
	}
	for _, selected := range f.Jobs {
		job, err := store.GetJob(ctx, principal, f.Namespace, selected.JobID)
		if err != nil || job.NamespaceID != f.NamespaceID || job.Name != "synthetic-alert-"+receipt+"-"+selected.Case || job.Target != fixtureTarget || job.Imported {
			return errors.New("notification fixture job differs")
		}
	}
	return nil
}

func completeNotificationScenario(ctx context.Context, pool *pgxpool.Pool, store *postgres.Store, principal domain.Principal, f notificationFixture, selected string) (notificationCompletion, error) {
	var result notificationCompletion
	jobID := ""
	for _, job := range f.Jobs {
		if job.Case == selected {
			jobID = job.JobID
		}
	}
	if jobID == "" {
		return result, errors.New("unknown notification scenario case")
	}
	job, err := store.CancelJob(ctx, principal, f.Namespace, jobID, "notification-"+f.Receipt+"-cancel-"+selected, fixtureDigest("notification:"+f.Receipt+":"+selected))
	if err != nil {
		return result, err
	}
	if job.Phase != "terminal" || job.Outcome != notificationCancelled {
		return result, errors.New("synthetic job unexpectedly launched; inspect exact job before retry")
	}
	var eventID string
	var raw []byte
	if queryErr := pool.QueryRow(ctx, `SELECT id::text,payload FROM outbox WHERE namespace_id=$1::uuid AND aggregate_id=$2::uuid AND topic='monitoring.job_terminal.v1'`, f.NamespaceID, jobID).Scan(&eventID, &raw); queryErr != nil {
		return result, queryErr
	}
	var event struct {
		EventID    string    `json:"eventId"`
		JobID      string    `json:"jobId"`
		Namespace  string    `json:"namespaceId"`
		Outcome    string    `json:"outcome"`
		Revision   string    `json:"jobRevision"`
		RecordedAt time.Time `json:"recordedAt"`
	}
	if len(raw) > 8192 || json.Unmarshal(raw, &event) != nil || event.EventID != eventID || event.JobID != jobID || event.Namespace != f.NamespaceID || event.Outcome != notificationCancelled || event.Revision != strconv.FormatInt(job.Revision, 10) || event.RecordedAt.IsZero() {
		return result, errors.New("original terminal outbox event differs")
	}
	result = notificationCompletion{Synthetic: true, Receipt: f.Receipt, Case: selected, DeploymentID: f.DeploymentID, ControlInstanceID: f.ControlInstanceID, RecoveryEpoch: f.RecoveryEpoch, NamespaceID: f.NamespaceID, JobID: jobID, EventID: eventID, Outcome: event.Outcome, JobRevision: event.Revision, RecordedAt: event.RecordedAt}
	if job.CurrentRun != nil {
		result.RunID, result.ExecutionID = job.CurrentRun.ID, job.CurrentRun.ExecutionID
	}
	return result, nil
}
