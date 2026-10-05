package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

func profileInfo() fixtureInfo {
	return fixtureInfo{Namespaces: []fixtureNamespace{{ID: "74000000-0000-4000-8000-000000000001", Name: "dashboard-research"}, {ID: "74000000-0000-4000-8000-000000000002", Name: "dashboard-operations"}}, Identities: []domain.DirectoryIdentity{{DirectoryID: aliceDirectoryID, PrincipalID: "75000000-0000-4000-8000-000000000001", Issuer: testInput().Issuer, Subject: "alice", DisplayName: "Alice"}, {DirectoryID: bobDirectoryID, PrincipalID: "75000000-0000-4000-8000-000000000002", Issuer: testInput().Issuer, Subject: "bob", DisplayName: "Bob"}}}
}

func TestFixtureProfilesAreFixedAndDefaultPrimary(t *testing.T) {
	p, err := selectProfile("")
	if err != nil || p != primaryProfile() || p.database != fixtureDatabase || p.ldapPort != "18636" {
		t.Fatal("primary compatibility changed", err)
	}
	s, err := selectProfile("secondary-v1")
	if err != nil || s.database == p.database || s.ldapPort == p.ldapPort || s.apiPort == p.apiPort || s.audience == p.audience || s.source == p.source || s.notificationUser != 1 {
		t.Fatal("secondary identities collide", err)
	}
	for _, name := range []string{"secondary", "PRIMARY", "../primary"} {
		if _, err = selectProfile(name); err == nil {
			t.Fatal("unlisted profile accepted")
		}
	}
	for _, database := range []string{"jobman_control", "jobman_dashboard", fixtureDatabase, "jobman_dashboard_control_secondary_other"} {
		if s.matchesDatabase(database) {
			t.Fatal("secondary repurposes another database")
		}
	}
	changed := s
	changed.database = fixtureDatabase
	if changed.validate() == nil {
		t.Fatal("mutated fixed profile accepted")
	}
	raw, marshalErr := json.Marshal(fixtureInfo{})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(raw, []byte(`"profile"`)) {
		t.Fatal("primary JSON gained a profile field")
	}
	for _, profile := range []fixtureProfile{p, s} {
		c, state := directoryProfile("/private", profile, testInput(), profileInfo())
		if directory.Validate(c) != nil || len(c.Mapping.Bindings) != 8 || len(state.Groups) != 8 {
			t.Fatal("invalid directory profile")
		}
		if profile.secondary() {
			if !strings.HasPrefix(state.Groups[0].ID, "73000000-") || len(state.Groups[0].Members) != 2 || len(state.Groups[1].Members) != 1 || state.Groups[1].Members[0] != bobDirectoryID || len(state.Groups[7].Members) != 1 || state.Groups[7].Members[0] != bobDirectoryID {
				t.Fatal("secondary role union differs")
			}
		} else if len(state.Groups[0].Members) != 2 || state.Groups[1].Members[0] != aliceDirectoryID || state.Groups[7].Members[0] != aliceDirectoryID {
			t.Fatal("primary memberships changed")
		}
	}
}

func TestSecondaryPreparationRejectsWrongDatabaseAndOverlappingRootsBeforeWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		if prepareProfile(t.Context(), "/unread/control", "/unread/input", "/unread/database", "/unread/logs", "/unread/directory", secondaryProfile()) == nil {
			t.Fatal("Windows secondary preparation accepted")
		}
		return
	}
	base, pathErr := filepath.EvalSymlinks(t.TempDir())
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	root := filepath.Join(base, "control")
	directoryRoot := filepath.Join(base, "directory")
	logs := filepath.Join(base, "logs")
	input := filepath.Join(base, "input.json")
	if err := writeJSON(input, testInput()); err != nil {
		t.Fatal(err)
	}
	for i, database := range []string{fixtureDatabase, "jobman_control", "jobman_dashboard"} {
		file := filepath.Join(base, fmt.Sprintf("db-%d", i))
		if err := os.WriteFile(file, []byte("postgres://synthetic@127.0.0.1:1/"+database+"?sslmode=verify-full"), 0o600); err != nil {
			t.Fatal(err)
		}
		if prepareProfile(t.Context(), root, input, file, logs, directoryRoot, secondaryProfile()) == nil {
			t.Fatal("wrong database reached preparation")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("wrong database mutated roots")
		}
	}
	for _, pair := range [][2]string{{root, root}, {root, filepath.Join(root, "nested")}, {filepath.Join(root, "nested"), root}} {
		if separateFixtureDirectories(pair[0], pair[1]) {
			t.Fatal("private tree overlap accepted")
		}
	}
	if _, err := notificationScenarioProfile(t.Context(), root, "unread", diagnosticDeployment, strings.Repeat("a", 32), "prepare", "", secondaryProfile()); err == nil {
		t.Fatal("primary deployment accepted by secondary scenario")
	}
	f := notificationFixture{DeploymentID: diagnosticDeployment}
	if verifyNotificationFixtureProfile(t.Context(), nil, domain.Principal{}, fixtureInfo{}, f, "", secondaryProfile()) == nil {
		t.Fatal("cross-profile receipt reached database")
	}
}

func TestSecondaryDirectoryExportAndTLSAreIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private POSIX fixture roots")
	}
	base, pathErr := filepath.EvalSymlinks(t.TempDir())
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	root := filepath.Join(base, "control")
	export := filepath.Join(base, "directory")
	if err := os.Mkdir(export, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := secondaryProfile()
	keys, err := generateMaterialProfile(root, "10.77.0.21", profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.workerPublic) != 32 || bytes.Equal(keys.public, keys.workerPublic) || bytes.Equal(keys.brokerPublic, keys.workerPublic) || keys.workerThumbprint == keys.thumbprint || keys.workerThumbprint == keys.brokerThumbprint {
		t.Fatal("service identities reused")
	}
	primaryRoot := filepath.Join(base, "primary")
	if _, err = generateMaterial(primaryRoot, "10.77.0.21"); err != nil {
		t.Fatal(err)
	}
	primaryCA, readErr := os.ReadFile(filepath.Join(primaryRoot, "fixture-ca.crt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	secondaryCA, readErr := os.ReadFile(filepath.Join(root, "fixture-ca.crt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Equal(primaryCA, secondaryCA) {
		t.Fatal("source trust reused")
	}
	cfg, state := directoryProfile(root, profile, testInput(), profileInfo())
	if err = exportSecondaryDirectory(root, export, profile, state); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(export)
	if err != nil || len(entries) != 5 {
		t.Fatal("directory export contains unexpected material", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "directory-server.crt", "directory-server.key", "directory-password", "directory-state.json", "directory-profile.json":
		default:
			t.Fatal("source secret exported")
		}
		info, statErr := entry.Info()
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("directory file not private")
		}
	}
	if exportSecondaryDirectory(root, export, profile, state) == nil {
		t.Fatal("directory material overwritten")
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(export, "directory-server.crt"), filepath.Join(export, "directory-server.key"))
	if err != nil {
		t.Fatal(err)
	}
	if cert.Leaf.VerifyHostname("127.0.0.1") != nil || cert.Leaf.VerifyHostname("10.77.0.21") == nil {
		t.Fatal("directory certificate impersonates source endpoint")
	}
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- serveDirectoryOnProfile(ctx, tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}), export, profile)
	}()
	cfg.URL = "ldaps://" + listener.Addr().String()
	snapshot, err := (directory.Reader{Config: cfg}).Read(t.Context(), 1)
	if err != nil || len(snapshot.Accounts) != 2 || len(snapshot.Groups) != 8 || len(snapshot.Groups[7].DirectoryIDs) != 1 || snapshot.Groups[7].DirectoryIDs[0] != bobDirectoryID {
		t.Fatal("secondary LDAP did not produce exact direct groups", err)
	}
	cfg.BaseDN, cfg.BindDN = fixtureBaseDN, fixtureBindDN
	if _, err = (directory.Reader{Config: cfg}).Read(t.Context(), 1); err == nil {
		t.Fatal("primary directory identity accepted at secondary LDAP")
	}
	cancel()
	if err = <-served; err != nil {
		t.Fatal(err)
	}
}

func TestSecondaryRootsReceiptsAndDirectoryProfileAreImmutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private POSIX fixture roots")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, export, logs := filepath.Join(base, "control"), filepath.Join(base, "directory"), filepath.Join(base, "logs")
	for _, path := range []string{root, export, logs} {
		if err = os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err = emptyPrivateFixtureRoot(path); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err = os.Symlink(logs, alias); err != nil {
		t.Fatal(err)
	}
	if emptyPrivateFixtureRoot(alias) == nil {
		t.Fatal("aliased root accepted")
	}
	if err = os.Chmod(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	if emptyPrivateFixtureRoot(logs) == nil {
		t.Fatal("group-readable preparation root accepted")
	}
	if err = os.Chmod(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = writePrivate(filepath.Join(root, secondaryReceipt), []byte("unfinished")); err != nil {
		t.Fatal(err)
	}
	if secondaryPreparePreflight(t.Context(), nil, root, export, logs) == nil {
		t.Fatal("unfinished root reached database")
	}
	if verifyCompletedSecondary(root, export, secondaryProfile()) == nil {
		t.Fatal("unfinished receipt accepted")
	}
	if verifyDirectoryProfile(root, primaryProfile()) == nil {
		t.Fatal("secondary root accepted as primary LDAP")
	}
	if err = finishSecondaryPreparation(root); err != nil {
		t.Fatal(err)
	}
	if _, err = generateMaterialProfile(root, "10.77.0.21", secondaryProfile()); err != nil {
		t.Fatal(err)
	}
	_, state := directoryProfile(root, secondaryProfile(), testInput(), profileInfo())
	if err = exportSecondaryDirectory(root, export, secondaryProfile(), state); err != nil {
		t.Fatal(err)
	}
	if err = verifyCompletedSecondary(root, export, secondaryProfile()); err != nil {
		t.Fatal(err)
	}
	if err = secondaryOperationPreflight(root, secondaryProfile()); err != nil {
		t.Fatal(err)
	}
	if verifyCompletedSecondary(root, logs, secondaryProfile()) == nil {
		t.Fatal("completed export path changed")
	}
	if verifyDirectoryProfile(export, primaryProfile()) == nil || verifyDirectoryProfile(root, primaryProfile()) == nil {
		t.Fatal("secondary material served under primary profile")
	}
	if err = writePrivate(filepath.Join(root, secondaryReceipt), []byte("unfinished")); err != nil {
		t.Fatal(err)
	}
	if secondaryOperationPreflight(root, secondaryProfile()) == nil {
		t.Fatal("unfinished source preparation receipt ignored")
	}
	if err = os.WriteFile(filepath.Join(export, "directory-profile.json"), []byte(`{"profile":"primary"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if verifyCompletedSecondary(root, export, secondaryProfile()) == nil {
		t.Fatal("changed directory profile accepted")
	}
}

func TestPrimaryPreparationRejectsOverlappingRoots(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "control")
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, logs := range []string{root, filepath.Join(root, "logs"), base} {
		if err = prepare(t.Context(), root, "unread-input", "unread-database", logs); err == nil || !strings.Contains(err.Error(), "separate directory") {
			t.Fatalf("overlapping roots did not fail preflight: %v", err)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	alias := filepath.Join(base, "alias")
	if err = os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if separateFixtureDirectories(root, filepath.Join(alias, "new-logs")) {
		t.Fatal("symlink ancestor bypassed root separation")
	}
	if !separateFixtureDirectories(root, filepath.Join(base, "independent-logs")) {
		t.Fatal("independent new log directory rejected")
	}
}
