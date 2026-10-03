package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

const (
	aliceDirectoryID = "71000000-0000-4000-8000-000000000001"
	bobDirectoryID   = "71000000-0000-4000-8000-000000000002"
	groupID          = "72000000-0000-4000-8000-000000000001"
)

func testInput() fixtureInput {
	return fixtureInput{Issuer: "https://issuer.example.test", Audience: "synthetic-audience", Host: "127.0.0.1", Users: []fixtureUser{{DirectoryID: aliceDirectoryID, Subject: "alice", Name: "Synthetic Alice"}, {DirectoryID: bobDirectoryID, Subject: "bob", Name: "Synthetic Bob"}}}
}

func TestSyntheticDirectoryTLS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires POSIX private-file permission enforcement")
	}
	root := filepath.Join(t.TempDir(), "private")
	keys, err := generateMaterial(root, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.public) != 32 || keys.thumbprint == keys.brokerThumbprint || bytes.Equal(keys.public, keys.brokerPublic) {
		t.Fatal("fixture services do not have distinct trust material")
	}
	state := fixtureState{Revision: 1, Users: []stateUser{{DirectoryID: aliceDirectoryID, Enabled: true}, {DirectoryID: bobDirectoryID, Enabled: false}}, Groups: []stateGroup{{ID: groupID, Members: []string{aliceDirectoryID}}}}
	if err = writeJSON(filepath.Join(root, "directory-state.json"), state); err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(root, "control-server.crt"), filepath.Join(root, "control-server.key"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
	served := make(chan error, 1)
	go func() { served <- serveDirectoryOn(ctx, server, root) }()
	namespace := "73000000-0000-4000-8000-000000000001"
	config := directory.Config{URL: "ldaps://" + listener.Addr().String(), BaseDN: fixtureBaseDN, BindDN: fixtureBindDN, PasswordFile: filepath.Join(root, "directory-password"), CAFile: filepath.Join(root, "fixture-ca.crt"), Mapping: domain.DirectoryMapping{SourceID: fixtureSource, Revision: 1, Namespaces: []string{namespace}, Bindings: []domain.DirectoryBinding{{GroupID: groupID, NamespaceID: namespace, Role: domain.RoleViewer}}, Identities: []domain.DirectoryIdentity{{DirectoryID: aliceDirectoryID, PrincipalID: "74000000-0000-4000-8000-000000000001", Issuer: "https://issuer.example.test", Subject: "alice"}, {DirectoryID: bobDirectoryID, PrincipalID: "74000000-0000-4000-8000-000000000002", Issuer: "https://issuer.example.test", Subject: "bob"}}}}
	snapshot, err := (directory.Reader{Config: config}).Read(t.Context(), 1)
	if err != nil || len(snapshot.Accounts) != 2 || !snapshot.Accounts[0].Enabled || snapshot.Accounts[1].Enabled || len(snapshot.Groups) != 1 || len(snapshot.Groups[0].DirectoryIDs) != 1 || snapshot.Groups[0].DirectoryIDs[0] != aliceDirectoryID {
		t.Fatalf("synthetic LDAPS snapshot=%#v,%v", snapshot, err)
	}
	cancel()
	if err = <-served; err != nil {
		t.Fatal(err)
	}
	if _, err = generateMaterial(root, "127.0.0.1"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing trust material was overwritten: %v", err)
	}
	for _, name := range []string{"fixture-ca.key", "dashboard-signing-key.pem", "broker-signing-key.pem", "directory-password"} {
		info, statErr := os.Stat(filepath.Join(root, name))
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private material permissions: %s,%v", name, statErr)
		}
	}
}

func TestFixtureLDAPBounds(t *testing.T) {
	for _, data := range [][]byte{{0x30, 0x83, 0x10, 0, 0}, {0x30, 0x80}, {0x30, 0x82, 0xff, 0xff}} {
		if _, err := readLDAPFrame(bytes.NewReader(data)); err == nil {
			t.Fatal("oversized or incomplete LDAP frame accepted")
		}
	}
	nested := []byte{4, 0}
	for range 18 {
		nested = append([]byte{0x30, byte(len(nested))}, nested...)
	}
	if validBERDepth(nested, 0) {
		t.Fatal("excessive BER nesting accepted")
	}
	if _, err := readLDAPFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fixtureInput){func(v *fixtureInput) { v.Issuer = "http://untrusted" }, func(v *fixtureInput) { v.Users[1].Subject = v.Users[0].Subject }, func(v *fixtureInput) { v.Users[0].DirectoryID = "not-a-guid" }, func(v *fixtureInput) { v.Host = "host/path" }} {
		input := testInput()
		mutate(&input)
		if validateInput(input) == nil {
			t.Fatal("unsafe fixture configuration accepted")
		}
	}
}

func TestFixtureSeedIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("open isolated test database")
	}
	defer admin.Close()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "fixture_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, dropErr := admin.Exec(context.WithoutCancel(ctx), "DROP SCHEMA "+quoted+" CASCADE"); dropErr != nil {
			t.Error(dropErr)
		}
	}()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse test database")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open fixture schema")
	}
	defer pool.Close()
	if err = postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := postgres.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	info, err := seed(ctx, store, testInput(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Namespaces) != 2 || len(info.Identities) != 2 {
		t.Fatalf("seed identities/scopes=%#v", info)
	}
	principal := domain.Principal{Issuer: testInput().Issuer, Subject: "alice"}
	for _, ns := range info.Namespaces {
		if len(ns.JobIDs) != 5 || ns.ArrayID == "" || ns.CollectionID == "" || ns.GraphID == "" {
			t.Fatalf("incomplete fixture namespace=%#v", ns)
		}
		for index, jobID := range ns.JobIDs[:3] {
			page, readErr := store.ListLogChunks(ctx, principal, ns.Name, jobID, domain.LogChunkOptions{Stream: "stdout", Limit: 100})
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch index {
			case 0:
				if page.State != "complete" || len(page.Chunks) != 2 || page.Chunks[1].ByteLength != 0 {
					t.Fatalf("nonempty stream=%#v", page)
				}
			case 1:
				if page.State != "complete" || len(page.Chunks) != 1 || page.ByteLength != 0 {
					t.Fatalf("empty stream=%#v", page)
				}
			case 2:
				if page.State != "open" || len(page.Chunks) != 0 || page.LastSequence != 0 {
					t.Fatalf("gapped open stream=%#v", page)
				}
			}
		}
	}
}
