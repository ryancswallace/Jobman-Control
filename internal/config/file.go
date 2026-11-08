package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maximumConfigBytes = 1 << 20

// File field names are deliberately case sensitive. Secret values are never
// accepted in a configuration document; only the two file references below.
var fileFields = map[string]string{ // #nosec G101 -- Configuration field names, not credential values.
	"databaseURLFile":          "DATABASE_URL_FILE",
	"agentTokenKeyFile":        "AGENT_TOKEN_KEY_FILE",
	"listen":                   "LISTEN",
	"authMode":                 "AUTH_MODE",
	"developmentAuth":          "DEVELOPMENT_AUTH",
	"developmentIssuer":        "DEVELOPMENT_ISSUER",
	"developmentSubject":       "DEVELOPMENT_SUBJECT",
	"developmentName":          "DEVELOPMENT_NAME",
	"developmentNamespace":     "DEVELOPMENT_NAMESPACE",
	"oidcIssuer":               "OIDC_ISSUER",
	"oidcAudience":             "OIDC_AUDIENCE",
	"bootstrapSubject":         "BOOTSTRAP_SUBJECT",
	"bootstrapName":            "BOOTSTRAP_NAME",
	"bootstrapNamespace":       "BOOTSTRAP_NAMESPACE",
	"tlsCertificateFile":       "TLS_CERT_FILE",
	"tlsKeyFile":               "TLS_KEY_FILE",
	"agentCACertificateFile":   "AGENT_CA_CERT_FILE",
	"agentCAKeyFile":           "AGENT_CA_KEY_FILE",
	"agentStaleAfter":          "AGENT_STALE_AFTER",
	"migrateOnStart":           "MIGRATE_ON_START",
	"directoryConfigFile":      "DIRECTORY_CONFIG_FILE",
	"directoryMode":            "DIRECTORY_MODE",
	"delegationRegistryFile":   "DELEGATION_REGISTRY_FILE",
	"delegationClientCAFile":   "DELEGATION_CLIENT_CA_FILE",
	"delegationAuditRetention": "DELEGATION_AUDIT_RETENTION",
	"monitoringFeedRetention":  "MONITORING_FEED_RETENTION",
	"diagnosticDeploymentId":   "DIAGNOSTIC_DEPLOYMENT_ID",
}

// Inspection contains only safe effective operational values and field
// provenance. Deployment strings, paths and all secret values are redacted.
type Inspection struct {
	SchemaVersion int               `json:"schemaVersion"`
	Values        map[string]any    `json:"values"`
	Origins       map[string]string `json:"origins"`
}

// LoadFile reads one explicit configuration and its private credential files.
// Known service environment variables are rejected instead of overriding it.
func LoadFile(path string) (Config, error) {
	configuration, _, err := loadFile(path, os.LookupEnv, true)
	return configuration, err
}

// InspectFile validates without opening referenced files or contacting any
// service. It is suitable for offline rendering; deployment must additionally
// check credentials, certificates, directory mappings and service connectivity.
func InspectFile(path string) (Inspection, error) {
	_, inspection, err := loadFile(path, os.LookupEnv, false)
	return inspection, err
}

func loadFile(path string, lookup lookupEnvironment, resolveSecrets bool) (Config, Inspection, error) {
	if err := rejectFileEnvironment(lookup); err != nil {
		return Config{}, Inspection{}, err
	}
	data, err := readBoundedFile(path, maximumConfigBytes, false)
	if err != nil {
		return Config{}, Inspection{}, errors.New("cannot read configuration file")
	}
	fields, err := decodeObject(data)
	if err != nil {
		return Config{}, Inspection{}, err
	}
	var version int
	if err = json.Unmarshal(fields["schemaVersion"], &version); err != nil || version != 1 {
		return Config{}, Inspection{}, errors.New("configuration schemaVersion must be 1")
	}
	values := make(map[string]string, len(fields))
	origins := make(map[string]string, len(fileFields))
	for name := range fileFields {
		origins[name] = "default"
	}
	for name, raw := range fields {
		if name == "schemaVersion" || name == "operational" || name == "databasePool" {
			continue
		}
		environment, exists := fileFields[name]
		if !exists {
			return Config{}, Inspection{}, errors.New("configuration contains an unknown field")
		}
		var value string
		if name == "migrateOnStart" || name == "developmentAuth" {
			var boolean bool
			if err = json.Unmarshal(raw, &boolean); err != nil {
				return Config{}, Inspection{}, fmt.Errorf("configuration %s must be a boolean", name)
			}
			value = strconv.FormatBool(boolean)
		} else if err = json.Unmarshal(raw, &value); err != nil || value == "" || strings.ContainsRune(value, 0) {
			return Config{}, Inspection{}, fmt.Errorf("configuration %s must be a nonempty string", name)
		}
		if strings.HasSuffix(name, "File") && !filepath.IsAbs(value) {
			value = filepath.Join(filepath.Dir(path), value)
		}
		values["JOBMAN_CONTROL_"+environment] = value
		origins[name] = "file"
	}
	if values["JOBMAN_CONTROL_AUTH_MODE"] == "" {
		return Config{}, Inspection{}, errors.New("authMode is required")
	}
	if values["JOBMAN_CONTROL_DATABASE_URL_FILE"] == "" {
		return Config{}, Inspection{}, errors.New("databaseURLFile is required")
	}
	if resolveSecrets {
		if secretErr := resolveFileSecrets(values); secretErr != nil {
			return Config{}, Inspection{}, secretErr
		}
	} else {
		// The ordinary validator still checks every authentication/TLS relation.
		// These stand-ins never escape the offline inspection result.
		values["JOBMAN_CONTROL_DATABASE_URL"] = "postgres://offline-validation.invalid/unused"
		if values["JOBMAN_CONTROL_AGENT_TOKEN_KEY_FILE"] != "" {
			values["JOBMAN_CONTROL_AGENT_TOKEN_KEY"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		}
	}
	configuration, err := load(func(name string) (string, bool) { value, exists := values[name]; return value, exists })
	if err != nil {
		return Config{}, Inspection{}, err
	}
	if err := applyTuning(&configuration, fields, origins); err != nil {
		return Config{}, Inspection{}, err
	}
	inspection := inspect(configuration, origins)
	return configuration, inspection, nil
}

func rejectFileEnvironment(lookup lookupEnvironment) error {
	for _, suffix := range fileFields {
		if _, exists := lookup("JOBMAN_CONTROL_" + suffix); exists {
			return errors.New("explicit configuration conflicts with a service environment variable")
		}
	}
	for _, suffix := range []string{"DATABASE_URL", "AGENT_TOKEN_KEY"} {
		if _, exists := lookup("JOBMAN_CONTROL_" + suffix); exists {
			return errors.New("explicit configuration conflicts with a secret environment variable")
		}
	}
	return nil
}

func resolveFileSecrets(values map[string]string) error {
	for _, name := range []string{"JOBMAN_CONTROL_DATABASE_URL", "JOBMAN_CONTROL_AGENT_TOKEN_KEY"} {
		path := values[name+"_FILE"]
		if path == "" {
			continue
		}
		value, err := readSecret(path)
		if err != nil {
			return fmt.Errorf("cannot load %s credential file", name)
		}
		values[name] = value
	}
	return nil
}

func readSecret(path string) (string, error) {
	data, err := readBoundedFile(path, 65536, true)
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("credential must be one nonempty line")
	}
	return value, nil
}

func readBoundedFile(path string, limit int64, private bool) ([]byte, error) {
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("file must be bounded and regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || (private && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("file is not bounded, regular and appropriately protected")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read bounded file")
	}
	return data, nil
}

// decodeObject rejects duplicate keys, nulls and ambiguous casing before any
// ordinary JSON decoding can discard those distinctions.
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(decoder, 0); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("configuration contains trailing data")
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil || result == nil {
		return nil, errors.New("configuration must be a JSON object")
	}
	return result, nil
}

func checkJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("configuration nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return errors.New("configuration contains invalid JSON or null")
	}
	if delimiter, ok := token.(json.Delim); ok {
		if delimiter != '{' {
			return errors.New("configuration only supports objects and scalar values")
		}
		seen := map[string]bool{}
		for decoder.More() {
			key, keyErr := decoder.Token()
			name, valid := key.(string)
			if keyErr != nil || !valid || seen[name] {
				return errors.New("configuration contains duplicate or invalid keys")
			}
			seen[name] = true
			if childErr := checkJSONValue(decoder, depth+1); childErr != nil {
				return childErr
			}
		}
		if _, err = decoder.Token(); err != nil {
			return errors.New("configuration object is incomplete")
		}
	}
	return nil
}
