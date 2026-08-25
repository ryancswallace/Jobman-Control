# End-to-end tests

This directory is reserved for assembled service, PostgreSQL, OIDC, mTLS,
agent, graceful-shutdown, and upgrade tests. Current PostgreSQL repository
integration tests live beside the store implementation and require the
explicit disposable `JOBMAN_CONTROL_TEST_DATABASE_URL` setting.

Future tests here must use isolated ports and databases, generate ephemeral
credentials, remain bounded, and never call a live identity provider or shared
cluster.
