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
	"slices"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func testScaleInput(now time.Time) scaleInput {
	input := scaleInput{InstanceID: "79000000-0000-4000-8000-000000000001", Issuer: "https://oidc.lab.test:8443/realms/jobman-lab", HistoryAt: now.Add(-time.Hour).Truncate(time.Second)}
	for i := range 25 {
		input.Users = append(input.Users, fixtureUser{DirectoryID: fmt.Sprintf("74000000-0000-4000-8000-%012d", i+1), Subject: fmt.Sprintf("75000000-0000-4000-8000-%012d", i+1), Name: fmt.Sprintf("Synthetic scale %02d", i+1)})
	}
	return input
}

func TestScaleSeedInputBounds(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	if testScaleInput(now).validate(now) != nil {
		t.Fatal("valid input rejected")
	}
	for _, mutate := range []func(*scaleInput){
		func(s *scaleInput) { s.InstanceID = "arbitrary" },
		func(s *scaleInput) { s.Issuer = "https://production.example" },
		func(s *scaleInput) { s.Users = s.Users[:24] },
		func(s *scaleInput) { s.Users[0].DirectoryID = s.Users[1].DirectoryID },
		func(s *scaleInput) { s.Users[0].Subject = s.Users[1].Subject },
		func(s *scaleInput) { s.Users[0].Name = "someone else" },
		func(s *scaleInput) { s.HistoryAt = now },
		func(s *scaleInput) { s.HistoryAt = now.Add(-8 * 24 * time.Hour) },
	} {
		input := testScaleInput(now)
		mutate(&input)
		if input.validate(now) == nil {
			t.Fatal("unsafe scale input accepted")
		}
	}
	if len(scaleNamespaceNames(primaryProfile())) != 10 || len(scaleNamespaceNames(secondaryProfile())) != 5 {
		t.Fatal("source envelope differs")
	}
}

func TestScaleDirectoryDraftIsAdditive(t *testing.T) {
	for _, profile := range []fixtureProfile{primaryProfile(), secondaryProfile()} {
		input := testScaleInput(time.Now().UTC())
		original := fixtureInfo{Identities: []domain.DirectoryIdentity{{DirectoryID: "71000000-0000-4000-8000-000000000001", PrincipalID: "71000000-0000-4000-8000-000000000002", Issuer: input.Issuer, Subject: "alice"}}, Namespaces: []fixtureNamespace{{ID: "71000000-0000-4000-8000-000000000003", Name: "dashboard-research"}}}
		base := testInput()
		config, state := directoryProfile("/private/synthetic", profile, base, original)
		seed := scaleSeedResult{DeploymentID: profile.deployment}
		for i, user := range input.Users {
			seed.Identities = append(seed.Identities, domain.DirectoryIdentity{DirectoryID: user.DirectoryID, PrincipalID: fmt.Sprintf("76000000-0000-4000-8000-%012d", i+1), Issuer: input.Issuer, Subject: user.Subject, DisplayName: user.Name})
		}
		for i, name := range scaleNamespaceNames(profile) {
			seed.Namespaces = append(seed.Namespaces, scaleNamespace{ID: fmt.Sprintf("77000000-0000-4000-8000-%012d", i+1), Name: name})
		}
		before, encodeErr := json.Marshal([]any{config, state})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		next, members, err := scaleDirectoryDraft(config, state, seed, profile)
		if err != nil || next.Mapping.Revision != 2 || members.Revision != 2 || len(next.Mapping.Identities) != 26 || len(members.Users) != 27 || len(next.Mapping.Bindings) != 4+len(seed.Namespaces) {
			t.Fatal("additive draft failed", err)
		}
		after, encodeErr := json.Marshal([]any{config, state})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("draft mutated original inputs")
		}
		for _, binding := range next.Mapping.Bindings[4:] {
			if binding.Role != domain.RoleViewer {
				t.Fatal("scale grant exceeds viewer")
			}
		}
		for _, group := range members.Groups[4:] {
			if len(group.Members) != 25 {
				t.Fatal("scale membership incomplete")
			}
		}
		if _, _, err = scaleDirectoryDraft(next, members, seed, profile); err == nil {
			t.Fatal("duplicate directory additions accepted")
		}
	}
}

func TestScaleSeedIntegration(t *testing.T) {
	if os.Getenv("JOBMAN_CONTROL_TEST_SCALE") != "1" {
		t.Skip("explicit bounded scale fixture opt-in required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	pool := secondaryIntegrationPool(ctx, t)
	store := postgres.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	// Preserve pre-existing source identities, job rows and terminal publications.
	base := testInput()
	original, err := seed(ctx, store, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var oldRows string
	if err = pool.QueryRow(ctx, `SELECT md5(string_agg(to_jsonb(j)::text,'' ORDER BY id)) FROM jobs j`).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	var oldEvents int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic='monitoring.job_terminal.v1'`).Scan(&oldEvents); err != nil {
		t.Fatal(err)
	}
	caps, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := testScaleInput(time.Now().UTC())
	input.InstanceID = caps.InstanceID
	seeded, err := seedScale(ctx, pool, store, input, secondaryProfile())
	if err != nil {
		t.Fatal(err)
	}
	if len(seeded.Namespaces) != 5 || len(seeded.Identities) != 25 {
		t.Fatal("incomplete fixed source envelope")
	}
	for _, ns := range seeded.Namespaces {
		principal := domain.Principal{Issuer: input.Issuer, Subject: input.Users[0].Subject}
		summary, summaryErr := store.NamespaceSummary(ctx, principal, ns.Name, nil, nil)
		if summaryErr != nil || summary.Total != "10050" || summary.Active != "50" || summary.ByOutcome["success"] != "10000" || summary.MissingCompletionTime != "0" {
			t.Fatal("source summary differs", summaryErr)
		}
		history, historyErr := store.GetJob(ctx, principal, ns.Name, ns.ImportedJobID)
		if historyErr != nil || !history.Imported || history.Phase != "terminal" {
			t.Fatal("history provenance absent", historyErr)
		}
	}
	preserved := []string{}
	for _, ns := range original.Namespaces {
		preserved = append(preserved, ns.ID)
	}
	var afterRows string
	var afterEvents int
	if err = pool.QueryRow(ctx, `SELECT md5(string_agg(to_jsonb(j)::text,'' ORDER BY id)) FROM jobs j WHERE namespace_id=ANY($1::uuid[])`, preserved).Scan(&afterRows); err != nil || afterRows != oldRows {
		t.Fatal("existing job rows changed", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic='monitoring.job_terminal.v1'`).Scan(&afterEvents); err != nil || afterEvents != oldEvents {
		t.Fatal("scale seed published terminal events", err)
	}
	// Exercise the real TLS reader and normal adoption path: no fabricated
	// verified membership rows. Every new account becomes a direct viewer only.
	root := t.TempDir()
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := secondaryProfile()
	if _, err = generateMaterialProfile(root, "127.0.0.1", profile); err != nil {
		t.Fatal(err)
	}
	cfg, state := directoryProfile(root, profile, base, original)
	cfg, state, err = scaleDirectoryDraft(cfg, state, seeded, profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(root, "directory-state.json"), state); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(root, "directory-profile.json"), map[string]string{"profile": profile.name}); err != nil {
		t.Fatal(err)
	}
	if err = store.ConfigureDirectory(ctx, cfg.Mapping); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(root, "directory-server.crt"), filepath.Join(root, "directory-server.key"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	directoryContext, stopDirectory := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- serveDirectoryOnProfile(directoryContext, tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}), root, profile)
	}()
	t.Cleanup(func() {
		stopDirectory()
		if serveErr := <-served; serveErr != nil {
			t.Error(serveErr)
		}
	})
	cfg.URL = "ldaps://" + listener.Addr().String()
	epoch, err := store.DirectoryRecoveryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (directory.Reader{Config: cfg}).Read(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, user := range input.Users {
		access, accessErr := store.CurrentPrincipal(ctx, domain.Principal{Issuer: input.Issuer, Subject: user.Subject}, "", 100)
		if accessErr != nil || len(access.Namespaces) != 5 {
			t.Fatal("direct scale scope differs", accessErr)
		}
		for _, ns := range access.Namespaces {
			if !slices.Equal(ns.Roles, []string{domain.RoleViewer}) || ns.AuthorizationStatus != "verified" {
				t.Fatal("temporary bootstrap grant survived normal directory adoption")
			}
		}
	}
	if _, err = seedScale(ctx, pool, store, input, secondaryProfile()); err == nil {
		t.Fatal("unsafe seed retry accepted")
	}
	var total int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs j JOIN namespaces n ON n.id=j.namespace_id WHERE n.name LIKE 'dashboard-scale-%'`).Scan(&total); err != nil || total != 50250 {
		t.Fatal("repeat changed bounded metadata", err)
	}
}
