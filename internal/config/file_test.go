package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileValidationDoesNotResolveSecrets(t *testing.T) {
	t.Parallel()
	path := writeConfigFile(t, `{"schemaVersion":1,"databaseURLFile":"missing-password","authMode":"oidc","oidcIssuer":"https://identity.example.invalid","oidcAudience":"private-audience","agentTokenKeyFile":"missing-key","operational":{"writeTimeout":"45s"},"databasePool":{"maxConnections":12}}`)
	configuration, inspection, err := loadFile(path, mapLookup(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.WriteTimeout != 45*time.Second || configuration.DatabasePool.MaxConnections != 12 {
		t.Fatal("operational settings not applied")
	}
	if inspection.Origins["oidcAudience"] != "file" || inspection.Origins["operational.readTimeout"] != "default" {
		t.Fatal("provenance was lost")
	}
	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-audience", "missing-password", "identity.example.invalid", "offline-validation"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("inspection exposed private configuration")
		}
	}
	if _, _, err = loadFile(path, mapLookup(nil), true); err == nil {
		t.Fatal("runtime accepted missing credentials")
	}
}

func TestFileRuntimeCredentialsAndRelativePaths(t *testing.T) {
	t.Parallel()
	path := writeConfigFile(t, `{"schemaVersion":1,"databaseURLFile":"db-url","authMode":"oidc","oidcIssuer":"https://identity.example.invalid","oidcAudience":"control","agentTokenKeyFile":"key","directoryConfigFile":"directory.json","tlsCertificateFile":"cert.pem","tlsKeyFile":"cert.key","agentCACertificateFile":"ca.pem","agentCAKeyFile":"ca.key","migrateOnStart":false}`)
	root := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(root, "db-url"), []byte("postgres://synthetic-secret.invalid/example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if err := os.WriteFile(filepath.Join(root, "key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, _, err := loadFile(path, mapLookup(nil), true)
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("runtime accepted a credential file without verified POSIX permissions")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if configuration.DatabaseURL != "postgres://synthetic-secret.invalid/example" || len(configuration.AgentTokenKey) != 32 || configuration.MigrateOnStart || configuration.DirectoryConfigFile != filepath.Join(root, "directory.json") {
		t.Fatal("credential references or explicit false were not applied")
	}
}

func TestFileRejectsAmbiguityAndUnsafeTuning(t *testing.T) {
	t.Parallel()
	for _, document := range []string{
		`{"schemaVersion":1,"schemaVersion":1}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","Listen":"127.0.0.1:8080"}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","databaseURL":"secret"}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","migrateOnStart":null}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","operational":{"writeTimeout":"1s","writeTimeout":"2s"}}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","operational":{"agentCertificateLifetime":"2h"}}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","operational":{"readHeaderTimeout":"30s"}}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","databasePool":{"maxConnections":2,"minConnections":3}}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development","databasePool":{"unknown":1}}`,
		`{"schemaVersion":1,"databaseURLFile":"x","authMode":"development"} {}`,
	} {
		path := writeConfigFile(t, document)
		if _, _, err := loadFile(path, mapLookup(nil), false); err == nil {
			t.Fatalf("invalid document accepted: %s", document)
		}
	}
	path := writeConfigFile(t, `{"schemaVersion":1,"databaseURLFile":"x","authMode":"development"}`)
	for _, name := range []string{"JOBMAN_CONTROL_LISTEN", "JOBMAN_CONTROL_DATABASE_URL", "JOBMAN_CONTROL_AGENT_TOKEN_KEY_FILE"} {
		if _, _, err := loadFile(path, mapLookup(map[string]string{name: ""}), false); err == nil {
			t.Fatal("file/environment conflict accepted")
		}
	}
}

func TestEnvironmentCredentialFiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "db-url")
	if err := os.WriteFile(path, []byte("postgres://synthetic.invalid/test"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"JOBMAN_CONTROL_DATABASE_URL_FILE": path, "JOBMAN_CONTROL_AUTH_MODE": "development"}
	_, err := loadEnvironment(mapLookup(values))
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("environment accepted a credential file without verified POSIX permissions")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	values["JOBMAN_CONTROL_DATABASE_URL"] = ""
	if _, err := loadEnvironment(mapLookup(values)); err == nil {
		t.Fatal("ambiguous credential accepted")
	}
	delete(values, "JOBMAN_CONTROL_DATABASE_URL")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnvironment(mapLookup(values)); err == nil {
		t.Fatal("world-readable credential accepted")
	}
}
