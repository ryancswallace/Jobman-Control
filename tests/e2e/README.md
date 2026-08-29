# End-to-end tests

The assembled identity test starts the real service composition against an
isolated PostgreSQL schema and an ephemeral HTTPS OIDC provider. It proves one
signed OIDC client path, one-time agent enrollment, certificate issuance, an
actual mTLS handshake, database-backed agent authorization, readiness, and
graceful shutdown. It requires the explicit disposable
`JOBMAN_CONTROL_TEST_DATABASE_URL` setting and otherwise skips.

The broader PostgreSQL repository integration tests live beside the store
implementation. `make integration-test` requires the disposable database URL
and runs both suites under the race detector.

Future tests here must use isolated ports and databases, generate ephemeral
credentials, remain bounded, and never call a live identity provider or shared
cluster.
