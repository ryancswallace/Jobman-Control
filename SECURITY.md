# Security policy

## Supported versions

Jobman Control v0.1.x and `main` are evaluation releases. Security fixes land
on `main` and may be backported when practical; this best-effort policy is not
a production support commitment.

| Version | Supported |
| --- | --- |
| `0.1.x` | Best effort |
| Pre-release `main` | Best effort |

## Reporting a vulnerability

Report suspected vulnerabilities privately through a
[GitHub security advisory]. Do not include credentials, private keys, tokens,
raw workload environments, log contents, or unrelated personal data.

Include the affected commit, deployment model, impact, and the smallest safe
reproduction. You should receive an acknowledgement within seven days. Do not
open a public issue until coordinated disclosure is complete.

## Security model

Jobman Control is a network-facing authorization and coordination service.
Compromise can permit job submission, cancellation, target enrollment, or
forged execution metadata within affected namespaces. Production deployments
must use OIDC for users, TLS for the server, mTLS for agents, restricted
PostgreSQL credentials, and a separately protected agent certificate authority.

PostgreSQL contains namespace membership, workload specifications, placement,
process results, audit history, credential derivation material, and object
manifests. It intentionally does not contain stdout, stderr, or artifact bytes.
Database backups are sensitive. Client and agent processes must never receive
database credentials.

Development authentication is restricted to loopback listeners and must never
be exposed through a proxy or container port. Environment variables may carry
database credentials and cryptographic keys; process environments, service
manager configuration, crash reports, and diagnostic output must be protected.
Logs deliberately omit request bodies, credentials, database URLs, workload
commands, and artifact content.

The complete trust and authority model is in
[`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md).

[GitHub security advisory]: https://github.com/ryancswallace/jobman-control/security/advisories/new
