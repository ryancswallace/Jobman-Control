package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func prepare(ctx context.Context, root, inputPath, databasePath, logRoot string) error {
	return prepareProfile(ctx, root, inputPath, databasePath, logRoot, "", primaryProfile())
}

func prepareProfile(ctx context.Context, root, inputPath, databasePath, logRoot, directoryRoot string, profile fixtureProfile) error {
	if profile.validate() != nil || !separateFixtureDirectories(root, logRoot) || profile.secondary() && (runtime.GOOS == "windows" || !separateFixtureDirectories(root, directoryRoot) || !separateFixtureDirectories(root, logRoot) || !separateFixtureDirectories(directoryRoot, logRoot)) {
		return errors.New("invalid fixed profile or separate directory export")
	}
	var input fixtureInput
	if err := readJSON(inputPath, &input); err != nil {
		return err
	}
	if err := validateInput(input); err != nil {
		return err
	}
	if profile.secondary() {
		if err := verifySecondaryDatabaseFile(databasePath, profile); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(root, "fixture-info.json")); err == nil {
		var completed fixtureInfo
		if readJSON(filepath.Join(root, "fixture-info.json"), &completed) != nil || !profile.matchesInfo(completed) {
			return errors.New("completed fixture profile differs")
		}
		// Completed preparations are immutable. Repeated invocation never rotates
		// trust material, resets revocations, changes grants or seeds duplicate work.
		var original fixtureInput
		if operationErr := readJSON(filepath.Join(root, "fixture-input.json"), &original); operationErr != nil {
			return operationErr
		}
		if original.Issuer != input.Issuer || original.Audience != input.Audience || original.Host != input.Host || !slices.Equal(original.Users, input.Users) {
			return errors.New("completed fixture input differs")
		}
		if profile.secondary() {
			return verifyCompletedSecondary(root, directoryRoot, profile)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	databaseBytes, err := readBounded(databasePath, 16384)
	if err != nil {
		return err
	}
	databaseURL := strings.TrimSpace(string(databaseBytes))
	endpoint, err := url.Parse(databaseURL)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || endpoint.Query().Get("sslmode") != "verify-full" || strings.ContainsAny(databaseURL, "\r\n") {
		return errors.New("fixture requires a private verify-full PostgreSQL URL")
	}
	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	var databaseName, schemaName string
	if operationErr := pool.QueryRow(ctx, "SELECT current_database(),current_schema()").Scan(&databaseName, &schemaName); operationErr != nil {
		return operationErr
	}
	if !profile.matchesDatabase(databaseName) || profile.secondary() && schemaName != "public" {
		return errors.New("fixture refuses any other database")
	}
	if profile.secondary() {
		if operationErr := secondaryPreparePreflight(ctx, pool, root, directoryRoot, logRoot); operationErr != nil {
			return operationErr
		}
	}
	if operationErr := postgres.Migrate(ctx, pool); operationErr != nil {
		return operationErr
	}
	keys, err := generateMaterialProfile(root, input.Host, profile)
	if err != nil {
		return err
	}
	store := postgres.New(pool, keys.tokenKey)
	info, err := seed(ctx, store, input, logRoot)
	if err != nil {
		return err
	}
	info.Profile = profile.name
	info.Synthetic = true
	info.Version = buildinfo.Version
	info.Issuer = input.Issuer
	info.Endpoint = "https://" + net.JoinHostPort(input.Host, profile.apiPort)
	info.DelegationAudience = profile.audience
	capabilities, err := store.Capabilities(ctx)
	if err != nil {
		return err
	}
	info.InstanceID = capabilities.InstanceID
	config, state := directoryProfile(root, profile, input, info)
	if operationErr := directory.Validate(config); operationErr != nil {
		return operationErr
	}
	registry := struct {
		Services []domain.DelegationKey `json:"services"`
	}{Services: []domain.DelegationKey{{ServiceID: profile.apiService, KeyID: profile.apiKey, Audience: profile.audience, PublicKey: keys.public, CertificateThumbprints: []string{keys.thumbprint}, NamespaceIDs: config.Mapping.Namespaces, Operations: []string{domain.CapabilityNamespaceRead, domain.CapabilityJobsRead, domain.CapabilityGroupsRead, domain.CapabilityTargetsRead, domain.CapabilityLogsRead, domain.CapabilityArtifactsRead, domain.CapabilityEvidenceRead}, Enabled: true}}}
	registry.Services = append(registry.Services, domain.DelegationKey{ServiceID: profile.brokerService, KeyID: profile.brokerKey, Audience: profile.audience, PublicKey: keys.brokerPublic, CertificateThumbprints: []string{keys.brokerThumbprint}, NamespaceIDs: config.Mapping.Namespaces, Operations: []string{domain.CapabilityNamespaceRead, domain.CapabilityLogsRead}, Enabled: true})
	if profile.secondary() {
		registry.Services[0].Operations = append(registry.Services[0].Operations, "events.read")
		registry.Services = append(registry.Services, domain.DelegationKey{ServiceID: profile.workerService, KeyID: profile.workerKey, Audience: profile.audience, PublicKey: keys.workerPublic, CertificateThumbprints: []string{keys.workerThumbprint}, NamespaceIDs: config.Mapping.Namespaces, Operations: []string{domain.CapabilityNamespaceRead, domain.CapabilityJobsRead, domain.CapabilityLogsRead, domain.CapabilityEvidenceRead, "events.read"}, Enabled: true})
	}
	if operationErr := writeJSON(filepath.Join(root, "directory.json"), config); operationErr != nil {
		return operationErr
	}
	if operationErr := writeJSON(filepath.Join(root, "directory-state.json"), state); operationErr != nil {
		return operationErr
	}
	if operationErr := writeJSON(filepath.Join(root, "delegation.json"), registry); operationErr != nil {
		return operationErr
	}
	environment := map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL": databaseURL, "JOBMAN_CONTROL_AUTH_MODE": "oidc", "JOBMAN_CONTROL_OIDC_ISSUER": input.Issuer, "JOBMAN_CONTROL_OIDC_AUDIENCE": input.Audience,
		"JOBMAN_CONTROL_LISTEN": "0.0.0.0:" + profile.apiPort, "JOBMAN_CONTROL_TLS_CERT_FILE": filepath.Join(root, "control-server.crt"), "JOBMAN_CONTROL_TLS_KEY_FILE": filepath.Join(root, "control-server.key"),
		"JOBMAN_CONTROL_AGENT_CA_CERT_FILE": filepath.Join(root, "fixture-ca.crt"), "JOBMAN_CONTROL_AGENT_CA_KEY_FILE": filepath.Join(root, "fixture-ca.key"), "JOBMAN_CONTROL_AGENT_TOKEN_KEY": base64.RawURLEncoding.EncodeToString(keys.tokenKey),
		"JOBMAN_CONTROL_DELEGATION_REGISTRY_FILE": filepath.Join(root, "delegation.json"), "JOBMAN_CONTROL_DELEGATION_CLIENT_CA_FILE": filepath.Join(root, "fixture-ca.crt"),
		"JOBMAN_CONTROL_DIRECTORY_CONFIG_FILE": filepath.Join(root, "directory.json"), "JOBMAN_CONTROL_DIRECTORY_MODE": "enforce", "JOBMAN_CONTROL_MIGRATE_ON_START": "false",
	}
	if profile.secondary() {
		environment["JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID"] = profile.deployment
		if operationErr := exportSecondaryDirectory(root, directoryRoot, profile, state); operationErr != nil {
			return operationErr
		}
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	slices.Sort(names)
	var env strings.Builder
	for _, name := range names {
		env.WriteString(name + "=" + strconv.Quote(environment[name]) + "\n")
	}
	if operationErr := writePrivate(filepath.Join(root, "control.env"), []byte(env.String())); operationErr != nil {
		return operationErr
	}
	if operationErr := writeJSON(filepath.Join(root, "fixture-input.json"), input); operationErr != nil {
		return operationErr
	}
	if operationErr := writeJSON(filepath.Join(root, "fixture-info.json"), info); operationErr != nil {
		return operationErr
	}
	if profile.secondary() {
		return finishSecondaryPreparation(root)
	}
	return nil
}

func validateInput(input fixtureInput) error {
	issuer, err := url.Parse(input.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("invalid synthetic OIDC issuer")
	}
	if len(input.Audience) < 1 || len(input.Audience) > 512 || len(input.Users) != 2 || input.Host == "" || strings.ContainsAny(input.Host, " /\\:@\r\n") {
		return errors.New("invalid fixture configuration")
	}
	for _, user := range input.Users {
		if !domain.IsID(user.DirectoryID) || user.Subject == "" || len(user.Subject) > 512 || user.Name == "" || len(user.Name) > 128 {
			return errors.New("invalid explicit fixture identity")
		}
	}
	if input.Users[0].DirectoryID == input.Users[1].DirectoryID || input.Users[0].Subject == input.Users[1].Subject {
		return errors.New("fixture identities must differ")
	}
	return nil
}

func fixtureDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
