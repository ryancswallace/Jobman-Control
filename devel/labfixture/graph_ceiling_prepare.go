package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

const graphCeilingPrimaryInstance = "e633cf92-258d-48ff-965a-fda88d68ef3a"

type graphCeilingPolicy struct {
	Namespace                string        `json:"namespace"`
	MaxActiveJobs            int           `json:"maxActiveJobs"`
	MaxQueuedJobs            int           `json:"maxQueuedJobs"`
	MaxCollectionItems       int           `json:"maxCollectionItems"`
	MaxGraphNodes            int           `json:"maxGraphNodes"`
	IdempotencyRetention     time.Duration `json:"idempotencyRetention"`
	PublishedOutboxRetention time.Duration `json:"publishedOutboxRetention"`
	Revision                 int64         `json:"revision"`
	CreatedAt                time.Time     `json:"createdAt"`
	UpdatedAt                time.Time     `json:"updatedAt"`
}

type graphCeilingPreflight struct {
	Version           int                `json:"version"`
	Synthetic         bool               `json:"synthetic"`
	HelperCommit      string             `json:"helperCommit"`
	InstanceID        string             `json:"instanceId"`
	RecoveryEpoch     string             `json:"recoveryEpoch"`
	NamespaceID       string             `json:"namespaceId"`
	Policy            graphCeilingPolicy `json:"policy"`
	Nonterminal       int                `json:"nonterminal"`
	ProposedMaxQueued int                `json:"proposedMaxQueued"`
	SourceFiles       map[string]string  `json:"sourceFiles"`
}
type graphCeilingQuotaReceipt struct {
	Before graphCeilingPreflight `json:"before"`
	After  graphCeilingPolicy    `json:"after"`
}
type graphCeilingCompletion struct {
	HelperCommit string `json:"helperCommit"`
	QuotaSHA256  string `json:"quotaSHA256"`
	GraphSHA256  string `json:"graphSHA256"`
	GraphID      string `json:"graphId"`
}

type graphCeilingSession struct {
	pool      *pgxpool.Pool
	store     *postgres.Store
	principal domain.Principal
	bob       domain.Principal
	original  fixtureInfo
	files     map[string]string
}

func graphCeilingPrivateRoot(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("absolute private ceiling root required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	st, statErr := os.Lstat(path)
	if err != nil || statErr != nil || resolved != path || !st.IsDir() || st.Mode().Perm() != 0o700 {
		return errors.New("private real ceiling root required")
	}
	return nil
}

func readGraphCeilingPrivate(path string, maximum int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 {
		return nil, errors.New("private regular ceiling file required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(before, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm() != 0o600 || actual.Size() < 1 || actual.Size() > maximum {
		return nil, errors.New("ceiling file changed or exceeds bound")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		return nil, errors.New("ceiling file exceeds bound")
	}
	return raw, nil
}

func writeGraphCeiling(root, name string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > 2<<20 {
		return errors.New("ceiling receipt exceeds bound")
	}
	f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func openGraphCeiling(ctx context.Context, root, databasePath string) (*graphCeilingSession, error) {
	if runtime.GOOS == "windows" || graphCeilingPrivateRoot(root) != nil {
		return nil, errors.New("ceiling fixture requires a private POSIX source root")
	}
	var input fixtureInput
	var original fixtureInfo
	files := map[string]string{}
	documents := map[string][]byte{}
	for _, name := range []string{"control.env", "fixture-input.json", "fixture-info.json", "directory.json", "directory-state.json", "delegation.json"} {
		raw, err := readGraphCeilingPrivate(filepath.Join(root, name), 2<<20)
		if err != nil {
			return nil, err
		}
		files[name] = scaleSHA(raw)
		documents[name] = raw
	}
	if decodeProfileJSON(documents["fixture-input.json"], &input) != nil || validateInput(input) != nil || decodeProfileJSON(documents["fixture-info.json"], &original) != nil || !primaryProfile().matchesInfo(original) || original.InstanceID != graphCeilingPrimaryInstance || original.Issuer != input.Issuer {
		return nil, errors.New("exact primary fixture identity required")
	}
	env, err := diagnosticEnvironment(documents["control.env"])
	if err != nil {
		return nil, err
	}
	dbRaw, err := readGraphCeilingPrivate(databasePath, 16384)
	if err != nil {
		return nil, err
	}
	dsn := strings.TrimSpace(string(dbRaw))
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql" || endpoint.Path != "/"+fixtureDatabase || endpoint.Query().Get("sslmode") != "verify-full" || env["JOBMAN_CONTROL_DATABASE_URL"] != dsn || env["JOBMAN_CONTROL_DIRECTORY_MODE"] != "enforce" || env["JOBMAN_CONTROL_MIGRATE_ON_START"] != "false" || env["JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID"] != diagnosticDeployment {
		return nil, errors.New("exact isolated verified TLS database configuration required")
	}
	key, err := base64.RawURLEncoding.DecodeString(env["JOBMAN_CONTROL_AGENT_TOKEN_KEY"])
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid private source key")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid ceiling database configuration")
	}
	config.MaxConns = 4
	config.MinConns = 0
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			pool.Close()
		}
	}()
	var database, schema, instance string
	if err = pool.QueryRow(ctx, `SELECT current_database(),current_schema(),id::text FROM control_instance`).Scan(&database, &schema, &instance); err != nil || database != fixtureDatabase || schema != "public" || instance != original.InstanceID {
		return nil, errors.New("ceiling database identity differs")
	}
	if operationErr := postgres.CheckMigrations(ctx, pool); operationErr != nil {
		return nil, operationErr
	}
	result := &graphCeilingSession{pool: pool, store: postgres.New(pool, key), principal: domain.Principal{Issuer: input.Issuer, Subject: input.Users[0].Subject}, bob: domain.Principal{Issuer: input.Issuer, Subject: input.Users[1].Subject}, original: original, files: files}
	if _, err = result.store.GetNamespacePolicy(ctx, result.bob, diagnosticNamespace); !errors.Is(err, domain.ErrForbidden) {
		return nil, errors.New("operations must remain inaccessible to synthetic bob")
	}
	access, err := result.store.CurrentPrincipal(ctx, result.principal, "", 100)
	if err != nil {
		return nil, err
	}
	admin := false
	for _, ns := range access.Namespaces {
		if ns.Name == diagnosticNamespace && slices.Contains(ns.Roles, domain.RoleNamespaceAdmin) {
			admin = true
		}
	}
	if !admin {
		return nil, errors.New("current synthetic alice namespace admin required")
	}
	valid = true
	return result, nil
}

func graphCeilingQuota(policy domain.NamespacePolicy, nonterminal int) (int, error) {
	if policy.Namespace != diagnosticNamespace || policy.Revision < 1 || policy.MaxGraphNodes < graphCeilingNodes || policy.MaxQueuedJobs < 1 || policy.MaxQueuedJobs > 1000000 || nonterminal < 0 || nonterminal > policy.MaxQueuedJobs {
		return 0, errors.New("ceiling quota baseline is outside fixed bounds")
	}
	if nonterminal+graphCeilingNodes <= policy.MaxQueuedJobs {
		return policy.MaxQueuedJobs, nil
	}
	if policy.MaxQueuedJobs+graphCeilingNodes > 1000000 {
		return 0, errors.New("ceiling quota increase exceeds the fixture bound")
	}
	return policy.MaxQueuedJobs + graphCeilingNodes, nil
}

func (s *graphCeilingSession) preflight(ctx context.Context) (graphCeilingPreflight, error) {
	result := graphCeilingPreflight{Version: 1, Synthetic: true, HelperCommit: buildinfo.Commit, InstanceID: s.original.InstanceID, RecoveryEpoch: "1", SourceFiles: s.files}
	caps, err := s.store.Capabilities(ctx)
	if err != nil || caps.InstanceID != result.InstanceID || caps.RecoveryEpoch != "1" {
		return result, errors.New("ceiling source identity changed")
	}
	policy, err := s.store.GetNamespacePolicy(ctx, s.principal, diagnosticNamespace)
	if err != nil {
		return result, err
	}
	result.Policy = graphCeilingPolicy(policy)
	var collision bool
	err = s.pool.QueryRow(ctx, `SELECT n.id::text,(SELECT count(*) FROM jobs j WHERE j.namespace_id=n.id AND j.phase<>'terminal'),EXISTS(SELECT 1 FROM targets t WHERE t.namespace_id=n.id AND t.name=$2) OR EXISTS(SELECT 1 FROM graphs g WHERE g.namespace_id=n.id AND g.name=$3) FROM namespaces n WHERE n.name=$1`, diagnosticNamespace, graphCeilingTarget, graphCeilingName).Scan(&result.NamespaceID, &result.Nonterminal, &collision)
	if err != nil || collision {
		return result, errors.New("ceiling graph or target already exists")
	}
	matched := 0
	for _, ns := range s.original.Namespaces {
		if ns.Name == diagnosticNamespace {
			if ns.ID != result.NamespaceID {
				return result, errors.New("operations namespace identity changed")
			}
			matched++
		}
	}
	if matched != 1 {
		return result, errors.New("original operations namespace absent or repeated")
	}
	result.ProposedMaxQueued, err = graphCeilingQuota(policy, result.Nonterminal)
	return result, err
}

func ceilingPolicyChange(before domain.NamespacePolicy, queued int) domain.NamespacePolicyChange {
	return domain.NamespacePolicyChange{MaxActiveJobs: before.MaxActiveJobs, MaxQueuedJobs: queued, MaxCollectionItems: before.MaxCollectionItems, MaxGraphNodes: before.MaxGraphNodes, IdempotencyRetention: before.IdempotencyRetention, PublishedOutboxRetention: before.PublishedOutboxRetention, ExpectedRevision: before.Revision}
}

func validateGraphCeilingQuota(receipt graphCeilingQuotaReceipt) error {
	before := receipt.Before
	proposed, err := graphCeilingQuota(domain.NamespacePolicy(before.Policy), before.Nonterminal)
	if err != nil || before.Version != 1 || !before.Synthetic || !domain.IsID(before.NamespaceID) || before.InstanceID != graphCeilingPrimaryInstance || before.RecoveryEpoch != "1" || proposed != before.ProposedMaxQueued {
		return errors.New("invalid ceiling quota transition")
	}
	expected := before.Policy
	if proposed != expected.MaxQueuedJobs {
		expected.MaxQueuedJobs = proposed
		expected.Revision++
		if receipt.After.UpdatedAt.Before(expected.UpdatedAt) {
			return errors.New("ceiling policy time regressed")
		}
		expected.UpdatedAt = receipt.After.UpdatedAt
	}
	if !reflect.DeepEqual(expected, receipt.After) {
		return errors.New("ceiling quota receipt changed unrelated policy")
	}
	return nil
}

func graphCeilingAction(ctx context.Context, root, databasePath, output, approved, action string) (any, error) {
	if !slices.Contains([]string{"preflight", "quota", "seed", "verify"}, action) {
		return nil, errors.New("unknown ceiling action")
	}
	session, err := openGraphCeiling(ctx, root, databasePath)
	if err != nil {
		return nil, err
	}
	defer session.pool.Close()
	if action == "preflight" {
		return session.preflight(ctx)
	}
	if !separateFixtureDirectories(root, output) || graphCeilingPrivateRoot(output) != nil {
		return nil, errors.New("separate private ceiling staging required")
	}
	connection, err := session.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Release()
	var locked bool
	if err = connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(76000000,10000)`).Scan(&locked); err != nil || !locked {
		return nil, errors.New("ceiling operation already active")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = connection.Conn().Close(cleanup)
	}()
	if action == "quota" {
		raw, readErr := readGraphCeilingPrivate(approved, 2<<20)
		if readErr != nil {
			return nil, readErr
		}
		var expected graphCeilingPreflight
		if decodeProfileJSON(raw, &expected) != nil {
			return nil, errors.New("invalid approved ceiling preflight")
		}
		current, currentErr := session.preflight(ctx)
		if currentErr != nil || !reflect.DeepEqual(current, expected) {
			return nil, errors.New("approved ceiling preflight changed")
		}
		if operationErr := emptyPrivateFixtureRoot(output); operationErr != nil {
			return nil, operationErr
		}
		if operationErr := writeGraphCeiling(output, "quota.intent.json", expected); operationErr != nil {
			return nil, operationErr
		}
		after := expected.Policy
		if expected.ProposedMaxQueued != expected.Policy.MaxQueuedJobs {
			changed, changeErr := session.store.UpdateNamespacePolicy(ctx, session.principal, diagnosticNamespace, ceilingPolicyChange(domain.NamespacePolicy(expected.Policy), expected.ProposedMaxQueued))
			if changeErr != nil {
				return nil, changeErr
			}
			after = graphCeilingPolicy(changed)
		}
		receipt := graphCeilingQuotaReceipt{Before: expected, After: after}
		if operationErr := writeGraphCeiling(output, "quota.complete.json", receipt); operationErr != nil {
			return nil, operationErr
		}
		return receipt, nil
	}
	raw, err := readGraphCeilingPrivate(filepath.Join(output, "quota.complete.json"), 2<<20)
	if err != nil {
		return nil, err
	}
	var quota graphCeilingQuotaReceipt
	if decodeProfileJSON(raw, &quota) != nil || validateGraphCeilingQuota(quota) != nil || quota.Before.Version != 1 || !quota.Before.Synthetic || quota.Before.HelperCommit != buildinfo.Commit || quota.Before.InstanceID != session.original.InstanceID || !reflect.DeepEqual(quota.Before.SourceFiles, session.files) {
		return nil, errors.New("ceiling quota receipt or source changed")
	}
	intentRaw, intentErr := readGraphCeilingPrivate(filepath.Join(output, "quota.intent.json"), 2<<20)
	var intent graphCeilingPreflight
	if intentErr != nil || decodeProfileJSON(intentRaw, &intent) != nil || !reflect.DeepEqual(intent, quota.Before) {
		return nil, errors.New("ceiling quota intent differs")
	}
	policy, err := session.store.GetNamespacePolicy(ctx, session.principal, diagnosticNamespace)
	if err != nil || !reflect.DeepEqual(graphCeilingPolicy(policy), quota.After) {
		return nil, errors.New("ceiling policy changed")
	}
	if action == "verify" {
		manifest, readErr := readCompletedGraphCeiling(output, quota, raw)
		if readErr != nil {
			return nil, readErr
		}
		if operationErr := verifyGraphCeiling(ctx, session.pool, session.store, session.principal, manifest); operationErr != nil {
			return nil, operationErr
		}
		if operationErr := verifyGraphCeilingFiles(root, session.files); operationErr != nil {
			return nil, operationErr
		}
		return manifest, nil
	}
	current, err := session.preflight(ctx)
	if err != nil || current.NamespaceID != quota.Before.NamespaceID || current.ProposedMaxQueued != quota.After.MaxQueuedJobs {
		return nil, errors.New("ceiling seed preflight changed")
	}
	if operationErr := writeGraphCeiling(output, "seed.intent.json", quota); operationErr != nil {
		return nil, operationErr
	}
	manifest, err := seedGraphCeiling(ctx, session.store, session.principal, quota.Before)
	if err != nil {
		return nil, err
	}
	if operationErr := verifyGraphCeiling(ctx, session.pool, session.store, session.principal, manifest); operationErr != nil {
		return nil, operationErr
	}
	if operationErr := writeGraphCeiling(output, "graph.json", manifest); operationErr != nil {
		return nil, operationErr
	}
	data, err := readGraphCeilingPrivate(filepath.Join(output, "graph.json"), 2<<20)
	if err != nil {
		return nil, err
	}
	if operationErr := verifyGraphCeilingFiles(root, session.files); operationErr != nil {
		return nil, operationErr
	}
	if operationErr := writeGraphCeiling(output, "seed.complete.json", graphCeilingCompletion{HelperCommit: buildinfo.Commit, QuotaSHA256: scaleSHA(raw), GraphSHA256: scaleSHA(data), GraphID: manifest.GraphID}); operationErr != nil {
		return nil, operationErr
	}
	return manifest, nil
}

func verifyGraphCeilingFiles(root string, expected map[string]string) error {
	for name, digest := range expected {
		data, err := readGraphCeilingPrivate(filepath.Join(root, name), 2<<20)
		if err != nil || scaleSHA(data) != digest {
			return errors.New("ceiling source files changed")
		}
	}
	return nil
}

func readCompletedGraphCeiling(output string, quota graphCeilingQuotaReceipt, quotaRaw []byte) (graphCeilingManifest, error) {
	var manifest graphCeilingManifest
	intentRaw, intentErr := readGraphCeilingPrivate(filepath.Join(output, "seed.intent.json"), 2<<20)
	var intent graphCeilingQuotaReceipt
	if intentErr != nil || decodeProfileJSON(intentRaw, &intent) != nil || !reflect.DeepEqual(intent, quota) {
		return manifest, errors.New("ceiling seed intent differs")
	}
	completeRaw, completeErr := readGraphCeilingPrivate(filepath.Join(output, "seed.complete.json"), 2<<20)
	var complete graphCeilingCompletion
	if completeErr != nil || decodeProfileJSON(completeRaw, &complete) != nil || complete.HelperCommit != buildinfo.Commit || complete.QuotaSHA256 != scaleSHA(quotaRaw) {
		return manifest, errors.New("ceiling completion receipt differs")
	}
	graphRaw, graphErr := readGraphCeilingPrivate(filepath.Join(output, "graph.json"), 2<<20)
	if graphErr != nil || scaleSHA(graphRaw) != complete.GraphSHA256 || decodeProfileJSON(graphRaw, &manifest) != nil || manifest.GraphID != complete.GraphID || manifest.InstanceID != quota.Before.InstanceID || manifest.RecoveryEpoch != quota.Before.RecoveryEpoch || manifest.NamespaceID != quota.Before.NamespaceID {
		return manifest, errors.New("completed ceiling manifest differs")
	}
	return manifest, nil
}
