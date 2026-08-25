# Deployment

Jobman Control is pre-release and not yet recommended for production. A
production-like deployment consists of PostgreSQL, one or more identical
Control replicas, an HTTPS endpoint, one OIDC issuer, shared agent CA material,
and separately deployed Jobman agents.

Linux agents can install a hardened per-user systemd service themselves after
enrollment. Jobman's OpenSSH bootstrap can transfer a matching Linux agent,
enroll it, install/start that user service, and verify local identity/version.
SSH ends at bootstrap; steady-state assignment, events, cancellation, and
capability reports use the mTLS agent API. Enabling user-manager lingering is a
site policy decision and is not performed automatically.

## Database

Use PostgreSQL 17.6, encrypted client connections, private networking, bounded
connections, durable storage, automated backups, and point-in-time recovery.
Create separate migration and runtime roles when possible. The URL is a secret
and must not appear in unit files, image layers, logs, support bundles, or shell
history.

Apply migrations with one candidate binary before shifting traffic, then run
all replicas with `JOBMAN_CONTROL_MIGRATE_ON_START=false`. Embedded migrations
use an advisory transaction lock and are safe against concurrent attempts, but
an explicit deployment step provides clearer privilege and failure boundaries.

## Identity and TLS

Configure an exact HTTPS OIDC issuer and audience. The service currently
terminates TLS itself; plaintext proxy-to-service deployments on a non-loopback
listener are rejected. Provide a server certificate/key and the agent CA
certificate/key. All replicas need identical trusted configuration and agent
token key material. Configure `JOBMAN_CONTROL_AGENT_STALE_AFTER` identically
as well; it changes when observations are labeled stale, not whether Control
retries or reassigns external work.

The optional bootstrap subject, name, and namespace create the first
administrator idempotently. Remove the bootstrap variables from later
deployments. Provision subsequent membership through the authorized API.

Protect the online agent CA key and database backups as high-value credentials.
Mount secrets read-only, restrict them to the service identity, and use an
external secret manager. Rotation and emergency administrative revocation APIs
remain future work, so document and test an operator procedure before relying
on the service.

## Container

The OCI image runs as numeric UID/GID 10001, uses Tini for signal handling,
contains CA roots and timezone data, and exposes port 8080. It contains no
default credentials or configuration and intentionally fails startup until
required environment values and mounted keys are present.

Mount TLS files read-only and inject secret values through the orchestrator.
Use `/healthz` for liveness and `/readyz` for readiness. Apply resource limits,
network policy, a read-only root filesystem, and a writable temporary directory
only if the platform requires one.

## Native package and systemd

DEB, RPM, and APK packages include the executable, OpenAPI document,
documentation, an example environment file, and a hardened systemd unit. Copy
the example to `/etc/jobman-control/jobman-control.env`, replace every
placeholder, set mode 0600 owned by root, provision TLS material beneath
`/etc/jobman-control/tls`, and keep private keys readable only by root. The unit
uses `LoadCredential=` to copy those files into a private, read-only credential
directory that its dynamic service identity can access. It overrides the four
certificate and key path variables from the environment file with those
credential paths. Review the sandbox and minimum systemd version against local
filesystem, identity-provider, and database requirements before enabling it.

The package does not enable or start the service automatically. After database
migration and configuration validation:

```console
sudo systemctl daemon-reload
sudo systemctl enable --now jobman-control
systemctl status jobman-control
```

## Scaling and shutdown

Replicas are database-coordinated and may share traffic. Use rolling deployment
only within a release whose schema is compatible with both old and new
binaries; no such cross-version guarantee exists before v1. Send SIGTERM,
remove the replica from load balancing, and allow at least 30 seconds for
graceful request shutdown.
