package main

// cspell:words relnamespace regnamespace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const secondaryReceipt = ".secondary-prepare.json"

type secondaryReceiptValue struct {
	Profile       string `json:"profile"`
	Database      string `json:"database"`
	DirectoryRoot string `json:"directoryRoot"`
}

func secondaryPreparePreflight(ctx context.Context, pool *pgxpool.Pool, root, directoryRoot, logRoot string) error {
	if runtime.GOOS == "windows" || !separateFixtureDirectories(root, directoryRoot) {
		return errors.New("secondary fixture requires separate POSIX private roots")
	}
	for _, path := range []string{root, directoryRoot, logRoot} {
		if err := emptyPrivateFixtureRoot(path); err != nil {
			return err
		}
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class WHERE relnamespace=current_schema()::regnamespace`).Scan(&tables); err != nil || tables != 0 {
		return errors.New("secondary preparation requires an empty dedicated database schema")
	}
	return writeDiagnosticJSON(root, secondaryReceipt, secondaryReceiptValue{Profile: "secondary-v1", Database: secondaryProfile().database, DirectoryRoot: directoryRoot})
}

func emptyPrivateFixtureRoot(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("secondary roots must already exist without aliases")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("secondary roots must be private real directories")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		return errors.New("secondary preparation refuses populated or partial roots")
	}
	return nil
}

func exportSecondaryDirectory(root, directoryRoot string, profile fixtureProfile, state fixtureState) error {
	if !profile.secondary() || profile.validate() != nil || !separateFixtureDirectories(root, directoryRoot) {
		return errors.New("invalid secondary directory export")
	}
	// Explicit allowlist: no database URL, source token key, CA private key,
	// delegation key, or source TLS server key enters the independent LDAP root.
	for _, name := range []string{"directory-server.crt", "directory-server.key", "directory-password"} {
		data, err := readDiagnosticPrivate(filepath.Join(root, name), 65536)
		if err != nil {
			return err
		}
		if operationErr := writePrivate(filepath.Join(directoryRoot, name), data); operationErr != nil {
			return operationErr
		}
	}
	if err := writeDiagnosticJSON(directoryRoot, "directory-state.json", state); err != nil {
		return err
	}
	if err := writeDiagnosticJSON(directoryRoot, "directory-profile.json", map[string]string{"profile": profile.name}); err != nil {
		return err
	}
	return writeDiagnosticJSON(root, "secondary-profile.json", secondaryReceiptValue{Profile: profile.name, Database: profile.database, DirectoryRoot: directoryRoot})
}

func verifyCompletedSecondary(root, directoryRoot string, profile fixtureProfile) error {
	if _, err := os.Lstat(filepath.Join(root, secondaryReceipt)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("secondary preparation receipt requires inspection")
	}
	var recorded secondaryReceiptValue
	if readJSON(filepath.Join(root, "secondary-profile.json"), &recorded) != nil || recorded.Profile != profile.name || recorded.Database != profile.database || recorded.DirectoryRoot != directoryRoot {
		return errors.New("completed secondary profile/export differs")
	}
	return verifyDirectoryProfile(directoryRoot, profile)
}

func finishSecondaryPreparation(root string) error {
	if err := os.Remove(filepath.Join(root, secondaryReceipt)); err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func verifyDirectoryProfile(root string, profile fixtureProfile) error {
	if profile.validate() != nil {
		return errors.New("invalid directory profile")
	}
	if !profile.secondary() {
		for _, marker := range []string{"directory-profile.json", "secondary-profile.json", secondaryReceipt} {
			if _, err := os.Lstat(filepath.Join(root, marker)); !errors.Is(err, os.ErrNotExist) {
				return errors.New("secondary material cannot serve the primary profile")
			}
		}
		return nil
	}
	data, err := readDiagnosticPrivate(filepath.Join(root, "directory-profile.json"), 4096)
	if err != nil {
		return err
	}
	var marker struct {
		Profile string `json:"profile"`
	}
	if decodeProfileJSON(data, &marker) != nil || marker.Profile != profile.name {
		return errors.New("directory profile differs")
	}
	return nil
}

func decodeProfileJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing profile data")
	}
	return nil
}

func verifySecondaryDatabaseFile(path string, profile fixtureProfile) error {
	raw, err := readDiagnosticPrivate(path, 16384)
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(string(raw))
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || u.User == nil || u.User.Username() != profile.database || u.Path != "/"+profile.database || u.Query().Get("sslmode") != "verify-full" || strings.ContainsAny(dsn, "\r\n") {
		return errors.New("secondary database input differs")
	}
	return nil
}

func secondaryOperationPreflight(root string, profile fixtureProfile) error {
	if !profile.secondary() {
		return nil
	}
	data, err := readDiagnosticPrivate(filepath.Join(root, "secondary-profile.json"), 4096)
	if err != nil {
		return err
	}
	var recorded secondaryReceiptValue
	if decodeProfileJSON(data, &recorded) != nil || recorded.Profile != profile.name || recorded.Database != profile.database || !separateFixtureDirectories(root, recorded.DirectoryRoot) {
		return errors.New("secondary fixture profile/export differs")
	}
	// The LDAP process owns a separate 0700 tree. Source-side helpers must not
	// acquire access to its TLS key/state; the privileged Lab wrapper checks that
	// tree's recovery receipts before invoking any secondary mutation.
	if _, statErr := os.Lstat(filepath.Join(root, secondaryReceipt)); !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("secondary preparation receipt requires inspection")
	}
	return nil
}
