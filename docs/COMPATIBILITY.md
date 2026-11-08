# Compatibility

Jobman Control v0.3.1 is the service baseline. The `v1alpha1` API and
pre-v1 release series do not establish a permanent compatibility commitment.
This table describes implemented and tested contracts.

| Component | Current contract |
| --- | --- |
| Go source build | Exact patch in `go.version`; language baseline in `go.mod` |
| PostgreSQL | PostgreSQL 17.6 in continuous integration |
| Compiled shared evidence dependency | Public core `diagnostic` from released Jobman v1.9.0; exact module checksums are in `go.sum` |
| Client API | `jobman.control/v1alpha1` |
| Portable workload and agent protocol | Checked-in `jobman/v1alpha1` snapshot from Jobman v1.7.0; verified unchanged in v1.8.0 |
| Runtime archives | Linux, macOS, and Windows; amd64, arm64, and supported 386 combinations |
| OCI image | Linux amd64 and arm64, non-root |
| Native packages | Linux amd64, arm64, and 386 in DEB, RPM, and APK formats |
| Artifact manifests | Regular files in one target-resolved local/NFS or S3 store per workload |
| Target providers | On-premises or AWS ParallelCluster metadata; infrastructure management is external |
| Collections | Up to 10,000 explicit child jobs; optional compatible Slurm-array binding |
| Dependency graphs | Up to 10,000 immutable nodes and 100,000 edges; Control-owned cross-target readiness |
| Completed-history import | One quiescent terminal SQLite metadata record per request; no bulk bytes or live state |
| Agent installation | Linux systemd user service; one-time OpenSSH bootstrap lives in Jobman |

The service and every replica must run the same binary version during this
pre-v1 phase. A database migration ledger must exactly match the binary.
Downgrading across a migration is unsupported; restore the matching database
backup and previous binary together.

The copied Jobman contract is pinned to Jobman v1.7.0 at commit
`f23993f51aa7eb4638d13cc4cd46c5d62aa20d27` and is byte-identical in Jobman
v1.8.0. `make contracts-check` verifies its checksum lock without another
checkout. `make contracts-source-check JOBMAN_DIR=../jobman` verifies
byte-for-byte agreement with a selected canonical checkout. Refreshes must
originate in Jobman and update the tagged source record in
`contracts/jobman/v1alpha1/SOURCE.md`.

The current service implements named-host subprocess assignment, bounded Slurm
scheduler events and projections, immutable ParallelCluster target-generation
facts, target draining, agent capability/liveness snapshots, stale-observation
confidence, local/NFS or S3 logical manifests, transactional collections,
fail-fast policy, Slurm-array task bindings, immutable cross-target dependency
graphs, namespace quotas/fair dispatch, bounded metrics, audit export,
operational retention, a persistent recovery hold, and completed-history
import. The Jobman repository owns
one-time SSH bootstrap, systemd user-service installation, container adapters,
S3 byte transfer, and native array submission. Standalone Slurm, AWS
infrastructure provisioning/acceptance, multiple-store/directory artifacts,
per-execution cloud grants, retries, collection cancellation, and dependency
optimization through native Slurm dependencies are not yet release-supported.

The public shared diagnostic module pin is fetched and checksummed through Go
modules; builds never depend on a sibling checkout or a local `replace` rule.
The released module pin describes code embedded in Control, separately from the
Core/Agent executables installed on targets. Agent credential recovery requires
Control v0.3.0 and Jobman Agent v1.10.1 or newer compatible releases. Changing a
consumer pin requires its own compatibility checks; version strings alone do not
establish deployment interoperability.

Migration 21 adds the durable monitoring feed and explicit service-only assertion
shape. It does not backfill old terminal jobs. Exact schema compatibility prevents
old binaries from running after migration; rollback requires the coordinated
restore procedure, recovery-epoch advance, and explicit consumer reconciliation.
Original retained event UUIDs survive restores; missing backup history cannot be
reconstructed. See [monitoring events](MONITORING_EVENTS.md).
