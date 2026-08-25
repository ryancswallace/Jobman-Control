# Maintenance and support policy

Jobman Control is pre-release. Interfaces under `v1alpha1`, migrations,
deployment procedures, and compatibility claims may still change. Support for
unreleased `main` snapshots is best effort.

Issues and pull requests should include:

- the Jobman Control and Jobman versions or commits;
- operating system, architecture, and installation method;
- PostgreSQL major version and deployment topology;
- authentication mode and target kind without credentials or identity tokens;
- expected and actual behavior; and
- safe error classifications or minimized request metadata.

Do not post database URLs, tokens, private keys, certificates, workload
environments, command contents, log chunks, or raw support archives. Report
security concerns through the private process in [SECURITY.md](SECURITY.md).

Current limitations and the implemented surface are tracked in [README.md](README.md)
and [the compatibility guide](docs/COMPATIBILITY.md).
