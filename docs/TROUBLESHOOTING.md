# Troubleshooting

## Startup rejects configuration

Configuration errors name the invalid setting without echoing its secret
value. Confirm that the database URL and authentication mode are present, TLS
and CA file pairs are complete, bootstrap values are all set or all absent,
and development mode uses a loopback listener.

## Readiness is unavailable

Check PostgreSQL reachability, TLS trust, role permissions, pool exhaustion,
and the migration ledger. The service rejects pending, changed, and unknown
migrations. Do not log or paste the database URL while diagnosing connectivity.

## Agent cannot poll or report

Confirm the server presents the expected TLS chain, the client certificate is
within its validity period, and its agent, target generation, serial, and key
digest remain active in PostgreSQL. Opaque compatibility sessions cannot use
execution endpoints, but an expired session prevents new assignment
eligibility. Confirm the agent is renewing its session/certificate and
reporting capabilities within `JOBMAN_CONTROL_AGENT_STALE_AFTER`.

## Execution remains nonterminal

For a target with `logStore`, both stdout and stderr need contiguous terminal
manifests. Check the agent's durable outbox, approved logical store/version,
NFS mount and permissions, immutable object key, checksum, and any missing
sequence. Repair delivery rather than forcing database state.

If `observationConfidence` is `stale`, agent silence is the only conclusion.
Inspect the host-local spool and the native process or Slurm accounting before
operator repair; never assume it is safe to launch a replacement. `uncertain`
means a scheduler identity could not be proven, while `lost` means the agent
had a launch claim without trustworthy completion evidence.

## Artifact publication failed

Confirm the workload uses one target-approved store name/version, the store is
visible at the configured agent and compute-node mount, the source is a regular
file rather than a symlink, and the immutable destination is absent or has
identical content. Required outputs must exist after the command; optional
outputs may be omitted. Control stores metadata only, so repair or restore the
external bytes separately.

## Safe issue reporting

Capture the service version, PostgreSQL version, safe job/execution IDs, HTTP
status and error classification, lifecycle phase, and timestamps. Remove
database URLs, bearer tokens, enrollment tokens, certificates, private keys,
workload documents, command contents, environments, and log bytes.
