package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

const (
	scalePending  = ".scale-seed.pending.json"
	scaleComplete = ".scale-seed.completed.json"
)

// prepareScale is an operator-only offline seeding command. It makes additive
// DB writes and separate directory drafts; it cannot restart or reconfigure any
// service. The caller must retain the pending receipt if any operation fails.
func prepareScale(ctx context.Context, root, inputPath, databasePath, directoryRoot, output string, profile fixtureProfile) error {
	if runtime.GOOS == "windows" || profile.validate() != nil || !separateFixtureDirectories(root, output) {
		return errors.New("scale preparation requires isolated POSIX roots")
	}
	if err := emptyPrivateFixtureRoot(output); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(root)
	st, statErr := os.Lstat(root)
	if err != nil || resolved != root || statErr != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		return errors.New("private real source root required")
	}
	if operationErr := secondaryOperationPreflight(root, profile); operationErr != nil {
		return operationErr
	}
	if !profile.secondary() {
		if directoryRoot != "" && directoryRoot != root {
			return errors.New("primary directory root differs")
		}
		directoryRoot = root
	} else {
		var original secondaryReceiptValue
		if readJSON(filepath.Join(root, "secondary-profile.json"), &original) != nil || original.DirectoryRoot != directoryRoot || verifyCompletedSecondary(root, directoryRoot, profile) != nil {
			return errors.New("secondary directory root differs")
		}
	}
	for _, base := range []string{root, directoryRoot} {
		for _, pattern := range []string{scalePending, scaleComplete, ".directory-acceptance-*.json", diagnosticReceiptName} {
			matches, matchErr := filepath.Glob(filepath.Join(base, pattern))
			if matchErr != nil || len(matches) != 0 {
				return errors.New("existing fixture operation requires inspection")
			}
		}
	}
	inputRaw, err := readDiagnosticPrivate(inputPath, 1<<20)
	if err != nil {
		return err
	}
	var input scaleInput
	if decodeProfileJSON(inputRaw, &input) != nil || input.validate(time.Now().UTC()) != nil {
		return errors.New("invalid public scale input")
	}
	var original fixtureInfo
	if readJSON(filepath.Join(root, "fixture-info.json"), &original) != nil || !profile.matchesInfo(original) || !original.Synthetic || original.InstanceID != input.InstanceID || original.Issuer != input.Issuer {
		return errors.New("scale source receipt differs")
	}
	raw, err := readDiagnosticPrivate(filepath.Join(root, "control.env"), 65536)
	if err != nil {
		return err
	}
	environment, err := diagnosticEnvironment(raw)
	if err != nil {
		return err
	}
	db, err := readDiagnosticPrivate(databasePath, 16384)
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(string(db))
	endpoint, parseErr := url.Parse(dsn)
	if parseErr != nil || endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql" || endpoint.Path != "/"+profile.database || endpoint.Query().Get("sslmode") != "verify-full" || strings.ContainsAny(dsn, "\r\n") || environment["JOBMAN_CONTROL_DATABASE_URL"] != dsn || environment["JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID"] != profile.deployment || environment["JOBMAN_CONTROL_DIRECTORY_MODE"] != "enforce" || environment["JOBMAN_CONTROL_MIGRATE_ON_START"] != "false" || environment["JOBMAN_CONTROL_DIRECTORY_CONFIG_FILE"] != filepath.Join(root, "directory.json") {
		return errors.New("scale source private configuration differs")
	}
	key, err := base64.RawURLEncoding.DecodeString(environment["JOBMAN_CONTROL_AGENT_TOKEN_KEY"])
	if err != nil || len(key) != 32 {
		return errors.New("invalid source token key")
	}
	configRaw, err := readDiagnosticPrivate(filepath.Join(root, "directory.json"), 1<<20)
	if err != nil {
		return err
	}
	stateRaw, err := readDiagnosticPrivate(filepath.Join(directoryRoot, "directory-state.json"), 1<<20)
	if err != nil {
		return err
	}
	var config directory.Config
	var state fixtureState
	if decodeProfileJSON(configRaw, &config) != nil || directory.Validate(config) != nil || decodeProfileJSON(stateRaw, &state) != nil || validateState(state) != nil || config.Mapping.SourceID != profile.source {
		return errors.New("scale directory baseline differs")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid scale database configuration")
	}
	poolConfig.MaxConns = 4
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return err
	}
	defer pool.Close()
	var databaseName, schemaName, instance string
	if err = pool.QueryRow(ctx, `SELECT current_database(),current_schema(),id::text FROM control_instance`).Scan(&databaseName, &schemaName, &instance); err != nil || !profile.matchesDatabase(databaseName) || schemaName != "public" || instance != input.InstanceID {
		return errors.New("scale database identity differs")
	}
	if operationErr := postgres.CheckMigrations(ctx, pool); operationErr != nil {
		return operationErr
	}
	// Keep an independent session lock while normal Store transactions use their
	// own connections. A second seeder fails immediately rather than waiting.
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	var locked bool
	if err = connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(74000000,25)`).Scan(&locked); err != nil || !locked {
		return errors.New("another source scale preparation is active")
	}
	// Close the dedicated connection on return to release the session lock even
	// if the operation context has expired; never return a locked session to pool.
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = connection.Conn().Close(cleanup)
	}()
	receipt := map[string]any{"version": 1, "synthetic": true, "helperCommit": buildinfo.Commit, "instanceId": instance, "deploymentId": profile.deployment, "inputSHA256": scaleSHA(inputRaw), "directorySHA256": scaleSHA(configRaw), "directoryStateSHA256": scaleSHA(stateRaw), "output": output}
	if operationErr := writeDiagnosticJSON(root, scalePending, receipt); operationErr != nil {
		return operationErr
	}
	seeded, err := seedScale(ctx, pool, postgres.New(pool, key), input, profile)
	if err != nil {
		return err
	}
	next, members, err := scaleDirectoryDraft(config, state, seeded, profile)
	if err != nil {
		return err
	}
	// A concurrent directory operator change prevents publishing an applicable
	// draft, while the pending receipt retains any already-admitted new jobs.
	currentConfig, err := readDiagnosticPrivate(filepath.Join(root, "directory.json"), 1<<20)
	if err != nil {
		return err
	}
	currentState, err := readDiagnosticPrivate(filepath.Join(directoryRoot, "directory-state.json"), 1<<20)
	if err != nil || scaleSHA(currentConfig) != scaleSHA(configRaw) || scaleSHA(currentState) != scaleSHA(stateRaw) {
		return errors.New("directory baseline changed during scale preparation")
	}
	for _, item := range []struct {
		name  string
		value any
	}{{"seed.json", seeded}, {"directory.after.json", next}, {"directory-state.after.json", members}, {"receipt.json", receipt}} {
		if err := writeDiagnosticJSON(output, item.name, item.value); err != nil {
			return err
		}
	}
	// Preserve the immutable pending evidence as a start record. Both markers
	// block repeats; configuration installation is a separate reviewed action.
	return writeDiagnosticJSON(root, scaleComplete, receipt)
}

func scaleSHA(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
