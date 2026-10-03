package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH": "true",
	}))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadDevelopmentDefaults(t *testing.T) {
	t.Parallel()
	configuration, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":     "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH": "true",
	}))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if configuration.ListenAddress != defaultListenAddress {
		t.Fatalf("ListenAddress = %q", configuration.ListenAddress)
	}
	if configuration.DevelopmentNamespace != defaultNamespace {
		t.Fatalf("DevelopmentNamespace = %q", configuration.DevelopmentNamespace)
	}
	if !configuration.MigrateOnStart || configuration.MaxRequestBytes != defaultMaxRequestBytes {
		t.Fatalf("development defaults = %#v", configuration)
	}
	if configuration.AgentStaleAfter != 2*time.Minute {
		t.Fatalf("AgentStaleAfter = %s", configuration.AgentStaleAfter)
	}
}

func TestLoadAgentStaleAfter(t *testing.T) {
	t.Parallel()
	configuration, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":      "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH":  "true",
		"JOBMAN_CONTROL_AGENT_STALE_AFTER": "15m",
	}))
	if err != nil || configuration.AgentStaleAfter != 15*time.Minute {
		t.Fatalf("load() = %#v, %v", configuration, err)
	}
	_, err = load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":      "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH":  "true",
		"JOBMAN_CONTROL_AGENT_STALE_AFTER": "10s",
	}))
	if err == nil || !strings.Contains(err.Error(), "AGENT_STALE_AFTER") {
		t.Fatalf("load(invalid stale duration) error = %v", err)
	}
}

func TestLoadRejectsUnsafeDevelopmentBinding(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":     "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH": "true",
		"JOBMAN_CONTROL_LISTEN":           "0.0.0.0:8080",
	}))
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadRejectsImplicitDevelopmentAuth(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL": "postgres://unused",
	}))
	if err == nil || !strings.Contains(err.Error(), "AUTH_MODE") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadRejectsMalformedBoolean(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":     "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH": "definitely",
	}))
	if err == nil || !strings.Contains(err.Error(), "DEVELOPMENT_AUTH") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadRejectsAgentCAWithoutServerTLS(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":       "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH":   "true",
		"JOBMAN_CONTROL_AGENT_CA_CERT_FILE": "agent-ca.pem",
		"JOBMAN_CONTROL_AGENT_CA_KEY_FILE":  "agent-ca-key.pem",
	}))
	if err == nil || !strings.Contains(err.Error(), "mTLS") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadRejectsInvalidDevelopmentNamespace(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":          "postgres://unused",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH":      "true",
		"JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE": "Not Portable",
	}))
	if err == nil || !strings.Contains(err.Error(), "NAMESPACE") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadOIDCMode(t *testing.T) {
	t.Parallel()
	configuration, err := load(mapLookup(map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":        "postgres://unused",
		"JOBMAN_CONTROL_AUTH_MODE":           "oidc",
		"JOBMAN_CONTROL_OIDC_ISSUER":         "https://identity.example.edu",
		"JOBMAN_CONTROL_OIDC_AUDIENCE":       "jobman-control",
		"JOBMAN_CONTROL_AGENT_TOKEN_KEY":     base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"JOBMAN_CONTROL_BOOTSTRAP_SUBJECT":   "stable-subject",
		"JOBMAN_CONTROL_BOOTSTRAP_NAME":      "Research User",
		"JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE": "research",
	}))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if configuration.AuthMode != AuthModeOIDC || configuration.OIDCAudience != "jobman-control" ||
		len(configuration.AgentTokenKey) != 32 {
		t.Fatalf("OIDC configuration = %#v", configuration)
	}
}

func TestLoadOIDCRejectsUnsafeSettings(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":    "postgres://unused",
		"JOBMAN_CONTROL_AUTH_MODE":       "oidc",
		"JOBMAN_CONTROL_OIDC_ISSUER":     "https://identity.example.edu",
		"JOBMAN_CONTROL_OIDC_AUDIENCE":   "jobman-control",
		"JOBMAN_CONTROL_AGENT_TOKEN_KEY": base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	}
	tests := []struct {
		name   string
		change func(map[string]string)
		want   string
	}{
		{name: "HTTP issuer", change: func(values map[string]string) {
			values["JOBMAN_CONTROL_OIDC_ISSUER"] = "http://identity.example.edu"
		}, want: "HTTPS"},
		{name: "short key", change: func(values map[string]string) {
			values["JOBMAN_CONTROL_AGENT_TOKEN_KEY"] = base64.RawURLEncoding.EncodeToString([]byte("short"))
		}, want: "32 bytes"},
		{name: "non-loopback without TLS", change: func(values map[string]string) {
			values["JOBMAN_CONTROL_LISTEN"] = "0.0.0.0:8443"
		}, want: "non-loopback"},
		{name: "partial bootstrap", change: func(values map[string]string) {
			values["JOBMAN_CONTROL_BOOTSTRAP_SUBJECT"] = "subject"
		}, want: "must be set together"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			values := make(map[string]string, len(base))
			for key, value := range base {
				values[key] = value
			}
			test.change(values)
			_, err := load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func mapLookup(values map[string]string) lookupEnvironment {
	return func(name string) (string, bool) {
		value, exists := values[name]

		return value, exists
	}
}

func TestDelegationConfigurationRequiresVerifiedTransport(t *testing.T) {
	t.Parallel()
	baseline := map[string]string{
		"JOBMAN_CONTROL_AGENT_CA_CERT_FILE": "agent-ca.pem",
		"JOBMAN_CONTROL_AGENT_CA_KEY_FILE":  "agent-ca.key",
		"JOBMAN_CONTROL_DATABASE_URL":       "postgres://unused",
		"JOBMAN_CONTROL_AUTH_MODE":          "oidc",
		"JOBMAN_CONTROL_OIDC_ISSUER":        "https://identity.example.edu",
		"JOBMAN_CONTROL_OIDC_AUDIENCE":      "jobman-control",
		"JOBMAN_CONTROL_AGENT_TOKEN_KEY":    base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"JOBMAN_CONTROL_TLS_CERT_FILE":      "server.pem", "JOBMAN_CONTROL_TLS_KEY_FILE": "server.key",
		"JOBMAN_CONTROL_DELEGATION_REGISTRY_FILE": "services.json", "JOBMAN_CONTROL_DELEGATION_CLIENT_CA_FILE": "client-ca.pem",
	}
	configuration, err := load(mapLookup(baseline))
	if err != nil || configuration.DelegationRegistryFile != "services.json" || configuration.DelegationAuditRetention != 90*24*time.Hour {
		t.Fatalf("delegation configuration=%#v,%v", configuration, err)
	}
	for _, key := range []string{"JOBMAN_CONTROL_TLS_CERT_FILE", "JOBMAN_CONTROL_DELEGATION_CLIENT_CA_FILE", "JOBMAN_CONTROL_DELEGATION_REGISTRY_FILE"} {
		copyValues := make(map[string]string, len(baseline))
		for name, value := range baseline {
			copyValues[name] = value
		}
		delete(copyValues, key)
		if _, loadErr := load(mapLookup(copyValues)); loadErr == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	baseline["JOBMAN_CONTROL_DELEGATION_AUDIT_RETENTION"] = "24h"
	if _, err = load(mapLookup(baseline)); err == nil {
		t.Fatal("short audit retention accepted")
	}
	delete(baseline, "JOBMAN_CONTROL_DELEGATION_AUDIT_RETENTION")
	baseline["JOBMAN_CONTROL_DIRECTORY_CONFIG_FILE"] = "directory.json"
	configuration, err = load(mapLookup(baseline))
	if err != nil || configuration.DirectoryMode != "preview" {
		t.Fatalf("directory preview default=%#v,%v", configuration, err)
	}
	baseline["JOBMAN_CONTROL_DIRECTORY_MODE"] = "enforce"
	if _, err = load(mapLookup(baseline)); err != nil {
		t.Fatalf("directory enforcement=%v", err)
	}
	baseline["JOBMAN_CONTROL_DIRECTORY_MODE"] = "unsafe"
	if _, err = load(mapLookup(baseline)); err == nil {
		t.Fatal("unknown directory mode accepted")
	}
}

func TestDiagnosticDeploymentIdentity(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "79000000-0000-4000-8000-000000000001", "not-an-id"} {
		configuration, err := load(mapLookup(map[string]string{"JOBMAN_CONTROL_DATABASE_URL": "postgres://unused", "JOBMAN_CONTROL_DEVELOPMENT_AUTH": "true", "JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID": id}))
		if id == "not-an-id" {
			if err == nil {
				t.Fatal("invalid evidence deployment accepted")
			}
			continue
		}
		if err != nil || configuration.DiagnosticDeploymentID != id {
			t.Fatalf("diagnostic source configuration=%q,%v", configuration.DiagnosticDeploymentID, err)
		}
	}
}
