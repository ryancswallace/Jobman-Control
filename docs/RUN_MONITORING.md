# Bounded run monitoring

`GET /v1/namespaces/{namespace}/jobs/{jobId}/runs` returns up to 100 factual run
references (default 50), ordered by descending run number. The source advertises
`bounded-run-catalog`. `GET .../runs/{runId}` reads one run in the same job.
Both require current `jobs.read` in the repository transaction, including the
registered delegated service's namespace/operation intersection.

The first page fixes the highest run number. Subsequent pages retain that ceiling
and report its exact total, while returning each run's current metadata. New runs
appear after restarting pagination. An opaque ten-minute page token binds the
principal, namespace, job, instance, recovery epoch and authorization version.
Expired or mismatched tokens return conflict; they never authorize a read.
Each response carries the existing current-authority snapshot fields.

Run phases are the run's own source values, separate from job and execution phase.
A run without an assigned execution omits execution/target/backend/confidence
fields. Imported history and jobs awaiting first assignment can return no runs.
Creation/update times are metadata times; neither claims when a process started
or completed. No command, environment, specification, event history, storage path
or machine credential is returned.

Choose the returned `number` for existing log-chunk and artifact-metadata
`runNumber` queries, or the immutable `id` for a diagnostic snapshot's `runId`.
Those reads independently authorize their own required capabilities. A run list
is not permission to read logs or evidence.

The regression suite uses only the explicitly configured disposable PostgreSQL
schema. It covers bounded traversal, insertion during pagination, exact execution
references, empty unassigned history, cross-job selectors, expired/recovery
cursors and current membership removal. No schema migration is required.

Source selectors are authenticated with the existing persistent token key and a
run-specific HMAC domain. Same-key replicas accept valid selectors; token-key
rotation invalidates existing traversals. A valid signature does not replace
current service/user authorization. Missing signing material fails closed.
