# Configuration

Jobman Control accepts one explicit versioned JSON service file with
`jobman-control --config /etc/jobman-control/control.json`. Organization values
belong in an external private configuration repository. The public
[example](../etc/jobman-control/config.example.json) and
[schema](../schemas/config-v1.schema.json) contain generic defaults and references
only. JSON keys are case sensitive; unknown keys, duplicate keys, nulls, trailing
documents, files larger than 1 MiB, and invalid bounds are rejected.

Without `--config`, the environment interface below remains supported. Control
does not load `.env` files itself. A service file rejects any known service
configuration environment variable, including empty variables, to prevent two
conflicting authorities. There is no implicit file search or environment overlay.

`databaseURLFile` and `agentTokenKeyFile` reference regular files containing a
single secret line, at most 64 KiB, with no group or other permission bits. The
environment equivalents are `JOBMAN_CONTROL_DATABASE_URL_FILE` and
`JOBMAN_CONTROL_AGENT_TOKEN_KEY_FILE`; each conflicts with its inline counterpart.
Relative service file references resolve against the file's directory. Credentials
must never enter checked-in files, container images, arguments, or ordinary logs.

Protected secret-file loading currently requires POSIX permission semantics.
Windows binaries support offline validation and the existing environment runtime
interface; native Windows secret-file startup fails closed until an ACL-aware
file adapter is available. Use the Linux service/container for file-based runtime
configuration.

```console
jobman-control --config /etc/jobman-control/control.json --check-config
jobman-control --config /etc/jobman-control/control.json --show-config
jobman-control config validate --kind directory /etc/jobman-control/directory.json
jobman-control config validate --kind delegation /etc/jobman-control/delegation.json
jobman-control config validate --kind resources /etc/jobman-control/resources.json
```

These checks are offline: they do not open referenced secret/certificate files,
contact an identity provider, open the database, or migrate state. Validate the
directory and delegation documents separately. `--show-config` prints effective
operational values and `default`/`file` origins; deployment strings, paths, and
credentials are redacted. Startup still checks referenced material and services.
Directory and delegation JSON also reject duplicate keys, nulls, and incorrect
key casing. Their existing document versions remain unchanged.

## Service fields and compatibility environment

The service schema documents every field. Service fields use camelCase names:
`listen`, `authMode`, `developmentAuth`, `developmentIssuer`,
`developmentSubject`, `developmentName`, `developmentNamespace`, `oidcIssuer`,
`oidcAudience`, `bootstrapSubject`, `bootstrapName`, `bootstrapNamespace`,
`tlsCertificateFile`, `tlsKeyFile`, `agentCACertificateFile`, `agentCAKeyFile`,
`agentStaleAfter`, `migrateOnStart`, `directoryConfigFile`, `directoryMode`,
`delegationRegistryFile`, `delegationClientCAFile`, `delegationAuditRetention`,
`monitoringFeedRetention`, and `diagnosticDeploymentId`. The file requires
`schemaVersion: 1` and `databaseURLFile`. Authentication must be selected explicitly.

| Variable | Default | Purpose |
| --- | --- | --- |
| `JOBMAN_CONTROL_DATABASE_URL` | required | PostgreSQL connection URL; secret-bearing |
| `JOBMAN_CONTROL_AUTH_MODE` | required | `development` or `oidc` |
| `JOBMAN_CONTROL_DEVELOPMENT_AUTH` | `false` | Compatibility switch selecting development mode |
| `JOBMAN_CONTROL_LISTEN` | `127.0.0.1:8080` | HTTP or HTTPS listener |
| `JOBMAN_CONTROL_DEVELOPMENT_ISSUER` | `jobman-control-development` | Development principal issuer |
| `JOBMAN_CONTROL_DEVELOPMENT_SUBJECT` | `local-developer` | Development principal subject |
| `JOBMAN_CONTROL_DEVELOPMENT_NAME` | `local-developer` | Development display name |
| `JOBMAN_CONTROL_DEVELOPMENT_NAMESPACE` | `default` | Development namespace |
| `JOBMAN_CONTROL_OIDC_ISSUER` | required in OIDC mode | Exact HTTPS discovery issuer |
| `JOBMAN_CONTROL_OIDC_AUDIENCE` | required in OIDC mode | Required token audience |
| `JOBMAN_CONTROL_AGENT_TOKEN_KEY` | required in OIDC mode | Unpadded base64url encoding of at least 32 secret bytes; persistent agent-token and purpose-separated monitoring-cursor key, shared by all replicas |
| `JOBMAN_CONTROL_BOOTSTRAP_SUBJECT` | unset | Optional first administrator subject |
| `JOBMAN_CONTROL_BOOTSTRAP_NAME` | unset | Bootstrap administrator display name |
| `JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE` | unset | Bootstrap administrator namespace |
| `JOBMAN_CONTROL_TLS_CERT_FILE` | unset | Server certificate chain |
| `JOBMAN_CONTROL_TLS_KEY_FILE` | unset | Server private key |
| `JOBMAN_CONTROL_AGENT_CA_CERT_FILE` | unset | Agent client-certificate CA |
| `JOBMAN_CONTROL_AGENT_CA_KEY_FILE` | unset | Agent certificate-authority private key |
| `JOBMAN_CONTROL_AGENT_STALE_AFTER` | `2m` | Silence interval before accepted/running observations become stale |
| `JOBMAN_CONTROL_MIGRATE_ON_START` | `true` | Apply embedded forward-only migrations at startup |
| `JOBMAN_CONTROL_DIRECTORY_CONFIG_FILE` | unset | External [directory mapping](DIRECTORY.md), including password and CA references |
| `JOBMAN_CONTROL_DIRECTORY_MODE` | `preview` | `preview` or `enforce` directory reconciliation when configured |
| `JOBMAN_CONTROL_DELEGATION_REGISTRY_FILE` | unset | External public [delegation registrations](DELEGATION.md) |
| `JOBMAN_CONTROL_DELEGATION_CLIENT_CA_FILE` | unset | Delegated service client-certificate CA |
| `JOBMAN_CONTROL_DELEGATION_AUDIT_RETENTION` | `2160h` | Delegation audit retention, between 90 and 3650 days |
| `JOBMAN_CONTROL_MONITORING_FEED_RETENTION` | `720h` | Independent monitoring feed retention |
| `JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID` | unset | Immutable diagnostic deployment UUID |

## Bounded operational settings

These service file fields expose deployment tuning without widening protocol or
security ceilings. They have no environment aliases. All replicas should use the
same identity, policy, and retention configuration.

| `operational` field | Default | Accepted range |
| --- | --- | --- |
| `agentSessionLifetime` | `15m` | `1m`–`15m` |
| `agentCertificateLifetime` | `1h` | `1m`–`1h` |
| `enrollmentLifetime` | `10m` | `1m`–`10m` |
| `coordinatorInterval` | `1s` | `250ms`–`1m` |
| `readHeaderTimeout` | `5s` | `1s`–`1m` |
| `readTimeout` | `15s` | `1s`–`5m` |
| `writeTimeout` | `30s` | `1s`–`5m` |
| `idleTimeout` | `60s` | `1s`–`10m` |
| `shutdownTimeout` | `10s` | `1s`–`5m` |
| `readinessTimeout` | `2s` | `100ms`–`30s` |
| `maxRequestBytes` | `2097152` | `4096`–`2097152` bytes |

Header timeout cannot exceed read timeout; readiness timeout cannot exceed
shutdown timeout. `databasePool` fields are `maxConnections` (20, range 1–512),
`minConnections` (0, range 0–512), `maxLifetime` (`30m`, range `1m`–`24h`),
`maxIdleTime` (`5m`, range `1s`–`1h`), and `healthCheckPeriod` (`30s`, range
`1s`–`1m`). Minimum connections cannot exceed maximum, and idle time cannot exceed
lifetime. Account for replicas when setting the database connection budget.

## Authentication and startup

Development mode is permitted only on a loopback listener. It derives a fixed
development-only agent token key and creates the selected local principal and
namespace. Do not expose it through port forwarding or a reverse proxy.

OIDC mode requires an HTTPS issuer and exact audience. The three bootstrap
settings are all-or-none and should be removed after the initial identity is
created. Non-loopback OIDC requires service TLS. A TLS-enabled OIDC deployment
currently also requires the agent CA pair because execution endpoints are part
of the same listener.

Certificate and key paths are read at startup. Make private keys readable only
by the service identity. The agent CA key is online in the current slice and
therefore requires stronger access control, backup, audit, and rotation
planning than an ordinary server certificate.

`JOBMAN_CONTROL_AGENT_STALE_AFTER` must be between one minute and 24 hours.
It controls evidence confidence, not execution failure: crossing the interval
marks accepted or running observations `stale` and never authorizes a duplicate
launch or automatic reassignment.

`JOBMAN_CONTROL_MIGRATE_ON_START=false` is recommended when a dedicated
deployment step uses elevated schema credentials. The runtime database role
must then have data access but may omit schema-changing authority. Startup
fails if migrations are pending, changed, or unknown to the binary.

See [`etc/jobman-control/jobman-control.env.example`](../etc/jobman-control/jobman-control.env.example)
for placeholders. That file is not usable until every value is replaced and
the resulting file is stored privately outside version control.

The optional `JOBMAN_CONTROL_DIAGNOSTIC_DEPLOYMENT_ID` is the immutable Dashboard
registry source UUID. Configuring it enables `shared-diagnostic-snapshots`; all
replicas must preserve this identity. See [Shared diagnostic snapshots](DIAGNOSTIC_SNAPSHOTS.md).

`JOBMAN_CONTROL_MONITORING_FEED_RETENTION` defaults to `720h`, independently of
namespace outbox and Dashboard inbox retention. Valid values are whole-second
durations from `24h` through `8760h`; configure the same value on all replicas.
See [monitoring events](MONITORING_EVENTS.md) before reducing the retained window.
