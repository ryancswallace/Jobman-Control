package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PoolSettings bounds database resource use independently of connection secrets.
type PoolSettings struct {
	MaxConnections    int32
	MinConnections    int32
	MaxLifetime       time.Duration
	MaxIdleTime       time.Duration
	HealthCheckPeriod time.Duration
}

// DefaultPoolSettings preserves the established service pool defaults.
func DefaultPoolSettings() PoolSettings {
	return PoolSettings{MaxConnections: 20, MaxLifetime: 30 * time.Minute, MaxIdleTime: 5 * time.Minute, HealthCheckPeriod: 30 * time.Second}
}

type durationSetting struct {
	target           *time.Duration
	minimum, maximum time.Duration
}

func durationSettings(configuration *Config) map[string]durationSetting {
	return map[string]durationSetting{
		"agentSessionLifetime":     {&configuration.AgentSessionLifetime, time.Minute, 15 * time.Minute},
		"agentCertificateLifetime": {&configuration.AgentCertificateLifetime, time.Minute, time.Hour},
		"enrollmentLifetime":       {&configuration.EnrollmentLifetime, time.Minute, 10 * time.Minute},
		"coordinatorInterval":      {&configuration.CoordinatorInterval, 250 * time.Millisecond, time.Minute},
		"readHeaderTimeout":        {&configuration.ReadHeaderTimeout, time.Second, time.Minute},
		"readTimeout":              {&configuration.ReadTimeout, time.Second, 5 * time.Minute},
		"writeTimeout":             {&configuration.WriteTimeout, time.Second, 5 * time.Minute},
		"idleTimeout":              {&configuration.IdleTimeout, time.Second, 10 * time.Minute},
		"shutdownTimeout":          {&configuration.ShutdownTimeout, time.Second, 5 * time.Minute},
		"readinessTimeout":         {&configuration.ReadinessTimeout, 100 * time.Millisecond, 30 * time.Second},
	}
}

func poolDurations(pool *PoolSettings) map[string]durationSetting {
	return map[string]durationSetting{
		"maxLifetime":       {&pool.MaxLifetime, time.Minute, 24 * time.Hour},
		"maxIdleTime":       {&pool.MaxIdleTime, time.Second, time.Hour},
		"healthCheckPeriod": {&pool.HealthCheckPeriod, time.Second, time.Minute},
	}
}

func applyTuning(configuration *Config, fields map[string]json.RawMessage, origins map[string]string) error {
	for _, group := range []string{"operational", "databasePool"} {
		settings := durationSettings(configuration)
		if group == "databasePool" {
			settings = poolDurations(&configuration.DatabasePool)
		}
		for name := range settings {
			origins[group+"."+name] = "default"
		}
		if group == "operational" {
			origins[group+".maxRequestBytes"] = "default"
		} else {
			origins[group+".maxConnections"] = "default"
			origins[group+".minConnections"] = "default"
		}
		raw, exists := fields[group]
		if !exists {
			continue
		}
		values, err := decodeObject(raw)
		if err != nil {
			return err
		}
		for name, value := range values {
			if setting, found := settings[name]; found {
				var encoded string
				if err = json.Unmarshal(value, &encoded); err != nil {
					return fmt.Errorf("configuration %s.%s must be a duration", group, name)
				}
				parsed, parseErr := time.ParseDuration(encoded)
				if parseErr != nil || parsed < setting.minimum || parsed > setting.maximum {
					return fmt.Errorf("configuration %s.%s is outside its duration bounds", group, name)
				}
				*setting.target = parsed
			} else if err := applyInteger(configuration, group, name, value); err != nil {
				return err
			}
			origins[group+"."+name] = "file"
		}
	}
	if configuration.ReadHeaderTimeout > configuration.ReadTimeout || configuration.ReadinessTimeout > configuration.ShutdownTimeout {
		return errors.New("HTTP timeout relationships are invalid")
	}
	pool := configuration.DatabasePool
	if pool.MinConnections > pool.MaxConnections || pool.MaxIdleTime > pool.MaxLifetime {
		return errors.New("database pool relationships are invalid")
	}
	return nil
}

func applyInteger(configuration *Config, group, name string, raw json.RawMessage) error {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return errors.New("configuration limit must be an integer")
	}
	switch group + "." + name {
	case "operational.maxRequestBytes":
		if value < 4096 || value > defaultMaxRequestBytes {
			return errors.New("maxRequestBytes must be between 4096 and 2097152")
		}
		configuration.MaxRequestBytes = value
	case "databasePool.maxConnections":
		if value < 1 || value > 512 {
			return errors.New("maxConnections must be between 1 and 512")
		}
		configuration.DatabasePool.MaxConnections = int32(value)
	case "databasePool.minConnections":
		if value < 0 || value > 512 {
			return errors.New("minConnections must be between 0 and 512")
		}
		configuration.DatabasePool.MinConnections = int32(value)
	default:
		return errors.New("configuration contains an unknown operational setting")
	}
	return nil
}

func inspect(configuration Config, origins map[string]string) Inspection {
	values := make(map[string]any, len(origins))
	for name := range fileFields {
		values[name] = "[redacted]"
	}
	values["authMode"] = configuration.AuthMode
	values["developmentAuth"] = configuration.DevelopmentAuth
	values["migrateOnStart"] = configuration.MigrateOnStart
	values["directoryMode"] = configuration.DirectoryMode
	values["agentStaleAfter"] = configuration.AgentStaleAfter.String()
	values["monitoringFeedRetention"] = configuration.MonitoringFeedRetention.String()
	values["delegationAuditRetention"] = configuration.DelegationAuditRetention.String()
	for name, setting := range durationSettings(&configuration) {
		values["operational."+name] = setting.target.String()
	}
	for name, setting := range poolDurations(&configuration.DatabasePool) {
		values["databasePool."+name] = setting.target.String()
	}
	values["operational.maxRequestBytes"] = configuration.MaxRequestBytes
	values["databasePool.maxConnections"] = configuration.DatabasePool.MaxConnections
	values["databasePool.minConnections"] = configuration.DatabasePool.MinConnections
	return Inspection{SchemaVersion: 1, Values: values, Origins: origins}
}

func loadEnvironment(lookup lookupEnvironment) (Config, error) {
	values := map[string]string{}
	for _, name := range []string{"JOBMAN_CONTROL_DATABASE_URL", "JOBMAN_CONTROL_AGENT_TOKEN_KEY"} {
		path, hasFile := lookup(name + "_FILE")
		_, hasValue := lookup(name)
		if hasFile && hasValue {
			return Config{}, fmt.Errorf("%s and its file reference conflict", name)
		}
		if hasFile {
			secret, err := readSecret(path)
			if err != nil {
				return Config{}, fmt.Errorf("cannot load %s credential file", name)
			}
			values[name] = secret
		}
	}
	return load(func(name string) (string, bool) {
		if value, exists := values[name]; exists {
			return value, true
		}
		return lookup(name)
	})
}
