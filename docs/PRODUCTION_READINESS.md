# Production-readiness review

Initial review date: 2026-08-23.

## Current released baseline

Control v0.4.2 includes dedicated migrations and audited restore holds,
bounded lifecycle export, agent CA overlap and generation-specific agent
retirement. These operations build on the existing external configuration,
resource plan/apply, terminal monitoring feed and identity-preserving agent
recovery. The public API remains `v1alpha1`. See the
[compatibility reference](COMPATIBILITY.md) and [operations guide](OPERATIONS.md).

The initial review covered the service, agent and PostgreSQL architecture for
controlled development and integration testing. The exercises below are site
acceptance criteria. Record owners and deployment evidence separately from the
released service capabilities.

## Scope reviewed

The review covers Jobman Control's namespace and OIDC boundary, agent mTLS and
assignment protocol, PostgreSQL consistency model, subprocess and Slurm
evidence, local/NFS/S3 manifest handling, containers, collections/arrays,
immutable dependency graphs, completed-history import, namespace policy,
metrics, audit export, retention, and restore behavior. It does not certify the
department's OIDC provider, PostgreSQL topology, NFS ACLs, Slurm configuration,
AWS account, ParallelCluster images, S3/IAM policy, network controls, or backup
system.

## Implemented controls and evidence

- Shared metadata is authoritative only in PostgreSQL. Clients and agents have
  no database credentials, and SQLite remains local standalone state.
- Namespace authorization is repeated in repository operations. Mutating
  creation/cancellation APIs are digest-bound and idempotent; revision-checked
  policy changes use compare-and-swap.
- Assignment is inert until a generation-pinned mTLS agent durably accepts it
  and receives launch authorization. Silence, stale evidence, and ambiguous
  Slurm submission never authorize an automatic duplicate launch.
- Collection and graph admission is transactional and bounded. Graphs reject
  cycles and invalid predicates; cross-target readiness remains under Control,
  not a compromised worker or scheduler.
- Namespace policy serializes queued/group quota checks, caps active dispatch,
  and rotates eligible namespaces before applying FIFO order. It does not
  bypass Slurm fair share, account, QOS, or partition controls.
- Commands preserve argument boundaries. Artifact and log metadata is bounded,
  digest-checked, target-mapping-pinned, and kept separate from bulk bytes.
- Metrics use fixed label sets. Audit export is role-protected and resumable.
  Ordinary cleanup deletes only completed idempotency and published outbox
  records after policy retention.
- A persistent restore epoch and recovery hold block every new assignment until
  an operator reconciles target-side work. Dedicated logins separate migration,
  runtime, and audited hold control. Completed-history import cannot create
  runnable state or migrate live work.
- Bounded lifecycle export keeps acknowledged delivery receipts separate from
  the terminal monitoring feed. Backlog metrics and example alerts cover
  unpublished records.
- CA overlap allows staged trust rollover across replicas. Operators can
  retire an exact agent and generation while preserving execution evidence.
- Unit, race, contract, lint, documentation, and real PostgreSQL integration
  gates cover the initial code paths. Deterministic fake Slurm tests cover
  command and reconciliation behavior without claiming site acceptance.

## Required deployment decisions

Before site deployment, the department must record owners and approved
values for:

- supported PostgreSQL, Slurm, ParallelCluster, container-runtime, and agent
  versions;
- availability, assignment latency, queue age, reconciliation time, recovery
  point, recovery time, database growth, log throughput, and artifact-size
  objectives;
- namespace quotas, audit and lifecycle retention, NFS ownership/ACLs, S3
  encryption/versioning/lifecycle, and cross-user artifact access;
- OIDC group-to-namespace administration, Unix/Windows/Slurm identities,
  account/QOS bindings, and AWS role/prefix policy;
- access restriction for the unauthenticated metrics route and privileged
  recovery-hold database procedure; and
- server TLS, agent CA, OIDC signing-key, database credential, backup-key, and
  AWS credential rotation and revocation ownership.

## Blocking acceptance exercises

Production approval requires recorded, repeatable evidence for all of the
following:

1. Restore PostgreSQL to an earlier point, enter a new recovery epoch before
   service exposure, reconnect every affected agent/target generation,
   reconcile a process and Slurm job newer than the database, verify no new
   assignment appears during the hold, and release it deliberately.
2. Run concurrent multi-host clients against one Control deployment and verify
   namespace isolation, idempotency, quota serialization, graph readiness, and
   fair progress under burst single/collection/graph submissions.
3. Fail Control replicas, database connections, agents, submit hosts, artifact
   stores, and the network at each launch/publication boundary. Verify durable
   replay, bounded spools/retries, no duplicate automatic launch, and useful
   metrics/audit evidence.
4. Exercise on-premises Slurm submission, accounting lag, cancellation,
   requeue, arrays, cross-target graphs, and crash-after-`sbatch` ambiguity from
   the department's Linux and Windows client paths over the real NFS mappings.
5. Exercise ParallelCluster recreation/generation rollover, elastic compute,
   private S3/EFS routes, IAM scoping, containers/GPUs, cost controls, and
   scheduler accounting.
6. Load and soak at the approved maximum job, event, collection, graph, log,
   artifact, and retention-scale database sizes; establish alert thresholds
   from those measurements.
7. Perform emergency agent/server credential revocation and rotation, OIDC
   outage, target drain, user offboarding, audit export recovery, and artifact
   restore drills.

## Remaining operational capabilities

Operators reconcile target-side work after restore, coordinate CA changes
across replicas, provision cross-user filesystem ACLs, and select a durable
lifecycle export destination and alert route. The service leaves per-execution
secret or cloud credential brokering, priority classes, and target-weighted
fairness outside the released scope.

Department security and operations owners must review the resulting deployment
evidence and explicitly approve the residual risks. Passing repository tests
alone is not production approval.
