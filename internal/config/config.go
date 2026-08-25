// Package config loads and validates process configuration without exposing secrets.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var namespacePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$`)

const (
	defaultListenAddress            = "127.0.0.1:8080"
	defaultIssuer                   = "jobman-control-development"
	defaultSubject                  = "local-developer"
	defaultNamespace                = "default"
	defaultMaxRequestBytes          = int64(2 * 1024 * 1024)
	defaultAgentSessionLifetime     = 15 * time.Minute
	defaultAgentCertificateLifetime = time.Hour
	defaultEnrollmentLifetime       = 10 * time.Minute
	defaultCoordinatorInterval      = time.Second
	defaultAgentStaleAfter          = 2 * time.Minute
	// AuthModeDevelopment enables the loopback-only fixed development identity.
	AuthModeDevelopment = "development"
	// AuthModeOIDC validates bearer JWTs against an OIDC issuer and audience.
	AuthModeOIDC = "oidc"
)

// Config contains validated service settings. DatabaseURL is secret-bearing
// and must never be logged or returned by an endpoint.
type Config struct {
	DatabaseURL              string
	ListenAddress            string
	DevelopmentAuth          bool
	DevelopmentIssuer        string
	DevelopmentSubject       string
	DevelopmentName          string
	DevelopmentNamespace     string
	AuthMode                 string
	OIDCIssuer               string
	OIDCAudience             string
	BootstrapSubject         string
	BootstrapName            string
	BootstrapNamespace       string
	TLSCertificateFile       string
	TLSKeyFile               string
	AgentCACertificateFile   string
	AgentCAKeyFile           string
	AgentTokenKey            []byte
	AgentSessionLifetime     time.Duration
	AgentCertificateLifetime time.Duration
	EnrollmentLifetime       time.Duration
	CoordinatorInterval      time.Duration
	AgentStaleAfter          time.Duration
	MigrateOnStart           bool
	MaxRequestBytes          int64
	ReadHeaderTimeout        time.Duration
	ReadTimeout              time.Duration
	WriteTimeout             time.Duration
	IdleTimeout              time.Duration
	ShutdownTimeout          time.Duration
	ReadinessTimeout         time.Duration
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

type lookupEnvironment func(string) (string, bool)

func load(lookup lookupEnvironment) (Config, error) {
	databaseURL := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_DATABASE_URL", ""))
	if databaseURL == "" {
		return Config{}, errors.New("JOBMAN_CONTROL_DATABASE_URL is required")
	}

	developmentAuth, err := boolValue(lookup, "JOBMAN_CONTROL_DEVELOPMENT_AUTH", false)
	if err != nil {
		return Config{}, err
	}
	authMode := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_AUTH_MODE", ""))
	if authMode == "" && developmentAuth {
		authMode = AuthModeDevelopment
	}
	if authMode == "" {
		return Config{}, errors.New("JOBMAN_CONTROL_AUTH_MODE is required")
	}
	if developmentAuth && authMode != AuthModeDevelopment {
		return Config{}, errors.New("JOBMAN_CONTROL_DEVELOPMENT_AUTH conflicts with JOBMAN_CONTROL_AUTH_MODE")
	}

	listenAddress := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_LISTEN", defaultListenAddress))
	tlsCertificateFile := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_TLS_CERT_FILE", ""))
	tlsKeyFile := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_TLS_KEY_FILE", ""))
	if (tlsCertificateFile == "") != (tlsKeyFile == "") {
		return Config{}, errors.New("JOBMAN_CONTROL_TLS_CERT_FILE and JOBMAN_CONTROL_TLS_KEY_FILE must be set together")
	}
	agentCACertificateFile := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_AGENT_CA_CERT_FILE", ""))
	agentCAKeyFile := strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_AGENT_CA_KEY_FILE", ""))
	if (agentCACertificateFile == "") != (agentCAKeyFile == "") {
		return Config{}, errors.New("JOBMAN_CONTROL_AGENT_CA_CERT_FILE and JOBMAN_CONTROL_AGENT_CA_KEY_FILE must be set together")
	}
	if agentCACertificateFile != "" && tlsCertificateFile == "" {
		return Config{}, errors.New("agent mTLS requires JOBMAN_CONTROL_TLS_CERT_FILE and JOBMAN_CONTROL_TLS_KEY_FILE")
	}

	migrateOnStart, err := boolValue(lookup, "JOBMAN_CONTROL_MIGRATE_ON_START", true)
	if err != nil {
		return Config{}, err
	}
	agentStaleAfter, err := durationValue(
		lookup, "JOBMAN_CONTROL_AGENT_STALE_AFTER", defaultAgentStaleAfter,
		time.Minute, 24*time.Hour,
	)
	if err != nil {
		return Config{}, err
	}

	configuration := Config{
		DatabaseURL:              databaseURL,
		ListenAddress:            listenAddress,
		DevelopmentAuth:          developmentAuth,
		DevelopmentIssuer:        strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_DEVELOPMENT_ISSUER", defaultIssuer)),
		DevelopmentSubject:       strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_DEVELOPMENT_SUBJECT", defaultSubject)),
		DevelopmentName:          strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_DEVELOPMENT_NAME", defaultSubject)),
		DevelopmentNamespace:     strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE", defaultNamespace)),
		AuthMode:                 authMode,
		OIDCIssuer:               strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_OIDC_ISSUER", "")),
		OIDCAudience:             strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_OIDC_AUDIENCE", "")),
		BootstrapSubject:         strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_BOOTSTRAP_SUBJECT", "")),
		BootstrapName:            strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_BOOTSTRAP_NAME", "")),
		BootstrapNamespace:       strings.TrimSpace(valueOrDefault(lookup, "JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE", "")),
		TLSCertificateFile:       tlsCertificateFile,
		TLSKeyFile:               tlsKeyFile,
		AgentCACertificateFile:   agentCACertificateFile,
		AgentCAKeyFile:           agentCAKeyFile,
		AgentSessionLifetime:     defaultAgentSessionLifetime,
		AgentCertificateLifetime: defaultAgentCertificateLifetime,
		EnrollmentLifetime:       defaultEnrollmentLifetime,
		CoordinatorInterval:      defaultCoordinatorInterval,
		AgentStaleAfter:          agentStaleAfter,
		MigrateOnStart:           migrateOnStart,
		MaxRequestBytes:          defaultMaxRequestBytes,
		ReadHeaderTimeout:        5 * time.Second,
		ReadTimeout:              15 * time.Second,
		WriteTimeout:             30 * time.Second,
		IdleTimeout:              60 * time.Second,
		ShutdownTimeout:          10 * time.Second,
		ReadinessTimeout:         2 * time.Second,
	}
	if authModeErr := validateAuthentication(lookup, &configuration); authModeErr != nil {
		return Config{}, authModeErr
	}

	return configuration, nil
}

func validateAuthentication(lookup lookupEnvironment, configuration *Config) error {
	switch configuration.AuthMode {
	case AuthModeDevelopment:
		configuration.DevelopmentAuth = true
		if err := validateDevelopmentListenAddress(configuration.ListenAddress); err != nil {
			return err
		}
		if err := validateDevelopmentIdentity(*configuration); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte("jobman-control-development-agent-token-key"))
		configuration.AgentTokenKey = append([]byte(nil), sum[:]...)
	case AuthModeOIDC:
		if err := validateOIDC(configuration); err != nil {
			return err
		}
		key, err := secretKey(lookup, "JOBMAN_CONTROL_AGENT_TOKEN_KEY")
		if err != nil {
			return err
		}
		configuration.AgentTokenKey = key
	default:
		return fmt.Errorf("unsupported JOBMAN_CONTROL_AUTH_MODE %q", configuration.AuthMode)
	}

	return nil
}

func validateOIDC(configuration *Config) error {
	issuer, err := url.Parse(configuration.OIDCIssuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil ||
		issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("JOBMAN_CONTROL_OIDC_ISSUER must be an HTTPS issuer URL without credentials, query, or fragment")
	}
	if configuration.OIDCAudience == "" || len(configuration.OIDCAudience) > 512 {
		return errors.New("JOBMAN_CONTROL_OIDC_AUDIENCE must contain 1 to 512 bytes")
	}
	if bootstrapErr := validateOIDCBootstrap(*configuration); bootstrapErr != nil {
		return bootstrapErr
	}
	if configuration.TLSCertificateFile == "" && !isLoopbackAddress(configuration.ListenAddress) {
		return errors.New("OIDC on a non-loopback listener requires JOBMAN_CONTROL_TLS_CERT_FILE and JOBMAN_CONTROL_TLS_KEY_FILE")
	}
	if configuration.TLSCertificateFile != "" && configuration.AgentCACertificateFile == "" {
		return errors.New("OIDC TLS deployments require JOBMAN_CONTROL_AGENT_CA_CERT_FILE and JOBMAN_CONTROL_AGENT_CA_KEY_FILE")
	}

	return nil
}

func validateOIDCBootstrap(configuration Config) error {
	valuesSet := 0
	for _, value := range []string{
		configuration.BootstrapSubject, configuration.BootstrapName, configuration.BootstrapNamespace,
	} {
		if value != "" {
			valuesSet++
		}
	}
	if valuesSet != 0 && valuesSet != 3 {
		return errors.New("JOBMAN_CONTROL_BOOTSTRAP_SUBJECT, JOBMAN_CONTROL_BOOTSTRAP_NAME, and JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE must be set together")
	}
	if valuesSet == 0 {
		return nil
	}
	if len(configuration.BootstrapSubject) > 512 || len(configuration.BootstrapName) > 512 ||
		!utf8.ValidString(configuration.BootstrapSubject) || !utf8.ValidString(configuration.BootstrapName) {
		return errors.New("OIDC bootstrap identity values must contain at most 512 bytes of valid UTF-8")
	}
	if !namespacePattern.MatchString(configuration.BootstrapNamespace) {
		return errors.New("JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE has an invalid namespace name")
	}

	return nil
}

func secretKey(lookup lookupEnvironment, name string) ([]byte, error) {
	encoded := strings.TrimSpace(valueOrDefault(lookup, name, ""))
	if encoded == "" {
		return nil, fmt.Errorf("%s is required in OIDC mode", name)
	}
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode %s as unpadded base64url: %w", name, err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("%s must decode to at least 32 bytes", name)
	}

	return key, nil
}

func validateDevelopmentIdentity(configuration Config) error {
	identityValues := []struct {
		name  string
		value string
	}{
		{name: "JOBMAN_CONTROL_DEVELOPMENT_ISSUER", value: configuration.DevelopmentIssuer},
		{name: "JOBMAN_CONTROL_DEVELOPMENT_SUBJECT", value: configuration.DevelopmentSubject},
		{name: "JOBMAN_CONTROL_DEVELOPMENT_NAME", value: configuration.DevelopmentName},
	}
	for _, item := range identityValues {
		if !utf8.ValidString(item.value) || len(item.value) < 1 || len(item.value) > 512 {
			return fmt.Errorf("%s must contain 1 to 512 bytes of valid UTF-8", item.name)
		}
	}
	if !namespacePattern.MatchString(configuration.DevelopmentNamespace) {
		return errors.New("JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE has an invalid namespace name")
	}

	return nil
}

func valueOrDefault(lookup lookupEnvironment, name, fallback string) string {
	if value, exists := lookup(name); exists {
		return value
	}

	return fallback
}

func boolValue(lookup lookupEnvironment, name string, fallback bool) (bool, error) {
	value, exists := lookup(name)
	if !exists {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}

	return parsed, nil
}

func durationValue(
	lookup lookupEnvironment,
	name string,
	fallback, minimum, maximum time.Duration,
) (time.Duration, error) {
	value, exists := lookup(name)
	if !exists {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, minimum, maximum)
	}

	return parsed, nil
}

func validateDevelopmentListenAddress(address string) error {
	if !isLoopbackAddress(address) {
		return errors.New("development authentication requires a loopback JOBMAN_CONTROL_LISTEN address")
	}

	return nil
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
