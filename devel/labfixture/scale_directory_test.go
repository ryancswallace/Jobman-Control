package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Exercise the actual TLS/BER reader and its final user-version recheck with
// the 27 accounts and 18 bindings used by the accepted-scale source proposal.
// No live Lab directory or database is read or changed by this test.
func TestSyntheticDirectoryScaleTLS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires POSIX private-file permission enforcement")
	}
	root := filepath.Join(t.TempDir(), "private")
	if _, err := generateMaterial(root, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	state := fixtureState{Revision: 1}
	config := directory.Config{
		BaseDN: fixtureBaseDN, BindDN: fixtureBindDN,
		PasswordFile: filepath.Join(root, "directory-password"), CAFile: filepath.Join(root, "fixture-ca.crt"),
		Mapping: domain.DirectoryMapping{SourceID: fixtureSource, Revision: 1},
	}
	members := []string{}
	for i := range 27 {
		id := fmt.Sprintf("74000000-0000-4000-8000-%012d", i+1)
		state.Users = append(state.Users, stateUser{DirectoryID: id, Enabled: true})
		config.Mapping.Identities = append(config.Mapping.Identities, domain.DirectoryIdentity{
			DirectoryID: id, PrincipalID: fmt.Sprintf("75000000-0000-4000-8000-%012d", i+1),
			Issuer: "https://issuer.example.test", Subject: fmt.Sprintf("scale-%02d", i+1),
		})
		if i >= 2 {
			members = append(members, id)
		}
	}
	for i := range 18 {
		namespace := fmt.Sprintf("76000000-0000-4000-8000-%012d", i+1)
		group := fmt.Sprintf("77000000-0000-4000-8000-%012d", i+1)
		config.Mapping.Namespaces = append(config.Mapping.Namespaces, namespace)
		config.Mapping.Bindings = append(config.Mapping.Bindings, domain.DirectoryBinding{GroupID: group, NamespaceID: namespace, Role: domain.RoleViewer})
		groupMembers := []string{}
		if i >= 8 {
			groupMembers = members
		}
		state.Groups = append(state.Groups, stateGroup{ID: group, Members: groupMembers})
	}
	if err := writeJSON(filepath.Join(root, "directory-state.json"), state); err != nil {
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
	config.URL = "ldaps://" + listener.Addr().String()
	snapshot, err := (directory.Reader{Config: config}).Read(t.Context(), 1)
	if err != nil || len(snapshot.Accounts) != 27 || len(snapshot.Groups) != 18 || snapshot.IgnoredDirectMembers != 0 {
		t.Fatalf("bounded scale LDAPS read failed: %v", err)
	}
	for _, account := range snapshot.Accounts {
		if !account.Exists || !account.Enabled {
			t.Fatal("actual TLS directory reader lost a synthetic account")
		}
	}
	for i, group := range snapshot.Groups {
		want := 0
		if i >= 8 {
			want = 25
		}
		if !group.Exists || len(group.DirectoryIDs) != want {
			t.Fatal("direct group read changed the bounded membership set")
		}
	}
	cancel()
	if err = <-served; err != nil {
		t.Fatal(err)
	}
}

func TestSyntheticDirectoryScaleBounds(t *testing.T) {
	state := fixtureState{Revision: 1}
	for i := range 32 {
		id := fmt.Sprintf("74000000-0000-4000-8000-%012d", i+1)
		state.Users = append(state.Users, stateUser{DirectoryID: id, Enabled: true})
		state.Groups = append(state.Groups, stateGroup{ID: fmt.Sprintf("75000000-0000-4000-8000-%012d", i+1), Members: []string{id}})
	}
	if validateState(state) != nil {
		t.Fatal("valid boundary rejected")
	}
	for _, mutate := range []func(*fixtureState){
		func(s *fixtureState) {
			s.Users = append(s.Users, stateUser{DirectoryID: "74000000-0000-4000-8000-000000000033"})
		},
		func(s *fixtureState) {
			s.Groups = append(s.Groups, stateGroup{ID: "75000000-0000-4000-8000-000000000033"})
		},
		func(s *fixtureState) { s.Groups[0].Members = make([]string, 33) },
	} {
		copy := fixtureState{Revision: state.Revision, Users: append([]stateUser{}, state.Users...), Groups: append([]stateGroup{}, state.Groups...)}
		mutate(&copy)
		if validateState(copy) == nil {
			t.Fatal("synthetic directory bound was not enforced")
		}
	}
}
