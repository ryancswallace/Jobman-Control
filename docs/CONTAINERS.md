# Containers

The runtime image is a minimal, non-root packaging of the Control service. It
does not include PostgreSQL, Jobman agents, OIDC configuration, certificates,
or default credentials.

- UID and GID are 10001.
- Tini is PID 1 and forwards SIGTERM.
- The binary, license, and third-party notices are root-owned and immutable.
- Port 8080 is declared but callers choose the actual listener.
- The working directory is `/var/lib/jobman-control`.

Inject the database URL and token key through an orchestrator secret facility,
mount TLS and CA files read-only, and run with a read-only root filesystem.
Use `/healthz` and `/readyz` for probes. Development authentication cannot be
used through a published container port because it is intentionally restricted
to a loopback listener; run the binary directly for local development.

`make docker-smoke` verifies the image's version metadata, numeric non-root
identity, executable, license inventory, and OCI revision label.
