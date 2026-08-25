# Configuration

Jobman Control reads configuration only from environment variables. It does
not load `.env` files itself. Service managers, orchestrators, and secret stores
may inject the environment, but credentials must never enter checked-in files,
container images, command-line arguments, or ordinary logs.

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
| `JOBMAN_CONTROL_AGENT_TOKEN_KEY` | required in OIDC mode | Unpadded base64url encoding of at least 32 secret bytes |
| `JOBMAN_CONTROL_BOOTSTRAP_SUBJECT` | unset | Optional first administrator subject |
| `JOBMAN_CONTROL_BOOTSTRAP_NAME` | unset | Bootstrap administrator display name |
| `JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE` | unset | Bootstrap administrator namespace |
| `JOBMAN_CONTROL_TLS_CERT_FILE` | unset | Server certificate chain |
| `JOBMAN_CONTROL_TLS_KEY_FILE` | unset | Server private key |
| `JOBMAN_CONTROL_AGENT_CA_CERT_FILE` | unset | Agent client-certificate CA |
| `JOBMAN_CONTROL_AGENT_CA_KEY_FILE` | unset | Agent certificate-authority private key |
| `JOBMAN_CONTROL_AGENT_STALE_AFTER` | `2m` | Silence interval before accepted/running observations become stale |
| `JOBMAN_CONTROL_MIGRATE_ON_START` | `true` | Apply embedded forward-only migrations at startup |

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
