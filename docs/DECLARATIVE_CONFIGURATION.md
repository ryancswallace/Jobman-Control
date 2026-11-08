# Declarative administrative configuration

Keep desired resource documents outside the public repository, separately from
process settings and credentials. Use the generic
[resource example](../etc/jobman-control/resources.example.json) only as a format
example: its UUIDs and policy values are synthetic. The local validator is the
authoritative semantic validator for this versioned format.

```console
jobman-control config validate --kind resources /etc/jobman-control/resources.json
jobman-control resources plan --file /etc/jobman-control/resources.json
jobman-control resources plan --file /etc/jobman-control/resources.json --token-file /run/credentials/control-admin
jobman-control resources apply --file /etc/jobman-control/resources.json --token-file /run/credentials/control-admin --approve-digest sha256:REVIEWED_DIGEST
```

The first two commands are offline. The offline plan reports `unverified` actions
and the SHA-256 digest of the typed document, including ordering and identity
fences. A token file enables authenticated reads and a live plan. Apply requires
the exact reviewed digest, performs a fresh complete preflight, then submits
changes through existing authorized HTTP APIs. It never writes database tables
directly. Service configuration rendering never invokes apply.

The endpoint must be an exact HTTPS origin. Tokens use protected regular files
with no group/other permissions, at most 64 KiB; `--ca-file` adds a bounded PEM
trust bundle to system trust. Requests have deadlines, no redirects, and no
ambient HTTP proxy. A command has a five-minute deadline. Plan output contains
only digest, live-verification status, resource index/kind and action. It excludes
names, endpoint, principal identity and desired policy values. Errors never echo
server response bodies or credentials.

Live plan/apply currently requires POSIX secret-file permissions; native Windows
can validate and produce offline plans but has no ACL-aware credential loader.

The document requires `schemaVersion: 1`, `endpoint`, `expectedInstanceId`,
`expectedRecoveryEpoch` (canonical positive decimal string), and 1–320 `resources`.
Every resource requires `namespace` and the immutable `namespaceId`. Obtain those
identities using authenticated capabilities/current-principal discovery. A changed
instance, restore epoch, namespace identity, or missing capability fails preflight.
The instance/recovery/namespace identities are checked again before each mutation.

| Kind | Desired fields | Required existing-state fences |
| --- | --- | --- |
| `NamespacePolicy` | `policy`: `maxActiveJobs`, `maxQueuedJobs`, `maxCollectionItems`, `maxGraphNodes`, `idempotencyRetention`, `publishedOutboxRetention` | `expectedRevision` |
| `Target` | `name`, `target` using the existing target creation spec | Must be absent or already have identical active configuration |
| `TargetGeneration` | `name`, `target` using the existing generation spec | Immutable target `id`, `expectedGenerationId`, `expectedRevision` |
| `MembershipGrant` | Immutable grant `id`, `grant`: `principal` (`issuer`, `subject`, `displayName`), `role` | Server preserves immutable grant identity and rejects revoked grant reuse |

Target specs contain `kind`, `executionBackend`, `runtimes`, optional
`operatingSystems`, `architectures`, `capabilities`, `partitions`, `logStore`,
`artifactStores`, and `provider`. Existing target admission constraints apply,
including sorted distinct lists. Store references contain logical `name` and
integer `version`; physical store paths and credentials remain agent configuration.
See the [API guide](API.md) for target and membership semantics.

Policy counts must be positive; active/queued limits cannot exceed 2,147,483,647,
and collection and graph limits cannot exceed 10,000. Retention durations must
use whole seconds between one hour and 365 days, matching the API's returned
precision so repeat applies remain unchanged. JSON is bounded to
2 MiB and rejects duplicate keys, unknown fields, wrong casing, nulls and trailing
documents. Duplicate operations for a managed resource are rejected. A matching
policy or target is unchanged, including after a successful earlier apply.
Mutation requests retain stable digest/index idempotency keys and revision checks.

A live plan verifies source identity and authorization; membership grant conflicts
can only be determined during apply because this API has no grant lookup endpoint.
Identity checks and writes are separate requests. Revision checks and authorization
are enforced transactionally by the service, but the API does not expose an atomic
instance/recovery precondition. Quiesce administrative apply during restore or
endpoint replacement; the client stops when it observes an identity change.

Apply is not atomic across resources. Successful earlier operations remain if a
later operation fails; inspect live state and repeat or revise the reviewed
document. Omission never deletes resources, revokes memberships, alters target
state, or changes recovery state. Namespace creation has no general authorized
API and is explicitly unsupported here; perform existing controlled bootstrap
first, then pin its resulting identity. An unsupported resource kind is an error.
Directory grants stay under the directory reconciler's existing preview/apply,
revision, freshness and revocation rules. Delegation registrations stay under
their existing service-start registration and revocation rules.
