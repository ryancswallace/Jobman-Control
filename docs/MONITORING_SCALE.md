# Monitoring repository scale checks

<!-- cspell:ignore GOWORK -->

Two explicitly enabled PostgreSQL tests exercise the Dashboard design's source
database and graph navigation profiles. They create and remove random schemas
using `JOBMAN_CONTROL_TEST_DATABASE_URL`; use only a disposable synthetic test
database with schema-creation permission. They never attach an agent or run a
coordinator, modify a live source schema, or submit scheduler workloads.

Configure the test URL privately with TLS verification and `pool_max_conns=8`.
Keep credentials out of command arguments, logs and committed files. Then run:

```sh
JOBMAN_CONTROL_TEST_SCALE=1 GOWORK=off GOTOOLCHAIN=go1.26.6 \
  go test -race -count=1 -v ./internal/store/postgres \
  -run '^(TestMonitoringAcceptedScaleIntegration|TestGraphNavigationCeilingIntegration)$' \
  -timeout 10m
```

Both tests skip unless the database URL and scale opt-in are present. A failure
still invokes the ordinary disposable-schema cleanup. The tests use bounded
contexts and page sizes; they do not tune the database or increase connection
limits. The test pool uses eight connections when configured above, with a
separate administration connection for schema setup/cleanup.

## Accepted job-count profile

`TestMonitoringAcceptedScaleIntegration` creates 10 namespaces and 25 distinct
viewer principals. The dataset has 100,000 explicitly imported historical jobs
(10,000 per namespace) plus 500 accepted jobs (50 per namespace). Historical rows
have no invented executions. Direct SQL expands fixed synthetic metadata after
ordinary store setup; this is a read benchmark, not submission throughput or
execution acceptance.

It traverses all 10,050 rows in one namespace in 200-row pages, including equal
creation timestamps. Together, 25 concurrent viewers issue 200 authorized calls
to each of job listing, job detail and complete namespace summaries: 600 calls.
Counts, identities, page bounds and continuation behavior are checked alongside
latency. A separately analyzed 200-row projection records index names and server
execution time; that plan is not presented as the complete authorization query.

## Graph navigation ceiling

`TestGraphNavigationCeilingIntegration` creates a synthetic DAG with exactly
10,000 nodes and 100,000 edges. A root star makes its one-hop neighborhood the
entire graph; additional forward edges cannot form a cycle. This separate
profile has 10,000 accepted nodes, not the 500-active-job baseline.

All nodes and edges are visited using 50 pages of 200 nodes and 200 pages of 500
edges. The test checks stable ordering, no duplicates, complete totals, exact
pending predicate states and valid endpoints. Twenty neighborhood reads return
only 200 nodes and 500 edges, with 9,800 omitted nodes and 99,500 omitted edges.
The center remains visible and every returned edge has both endpoints. An
unauthorized principal is denied. Serialized repository response sizes are
bounded; these figures do not include HTTP wrapping or client layout memory.

## Recorded synthetic Lab result

On 2026-10-04, PostgreSQL 17.6 in Jobman-Lab passed both tests with the race
detector, migrations 1–21 and the eight-connection test pool. The accepted-count
test passed in 9.30 seconds; the graph test passed in 5.97 seconds.

| Operation | Samples | p95 | Largest serialized repository response |
| --- | ---: | ---: | ---: |
| Job list | 200 | 354 ms | Not measured |
| Job detail | 200 | 222 ms | Not measured |
| Namespace summary | 200 | 317 ms | Not measured |
| Graph nodes | 50 | 15 ms | 249,497 bytes |
| Graph edges | 200 | 12 ms | 109,665 bytes |
| Graph neighborhood | 20 | 58 ms | 355,970 bytes |

A combined final rerun after a lint-only control-flow change also passed
(race package 16.076 seconds): job-list/detail/summary p95 was 352/224/301 ms;
graph-node/edge/neighborhood p95 was 17/10/54 ms. Full source review and the
repository's non-Docker validation passed. GitHub CI records the remaining
platform gates on the delivered commit.

The tests fail if repository p95 exceeds two seconds, because source reads alone
would exhaust the proposed end-to-end budget. Passing does **not** establish the
two-second authenticated client rendering target. HTTP, directory revalidation,
multi-Control aggregation, simultaneous log following, network latency, browser
and iPhone rendering, real submissions and actual execution remain separate
acceptance gates. The test data is synthetic and is removed after the run.
