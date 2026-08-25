ALTER TABLE agents
    ADD COLUMN last_seen_at timestamptz,
    ADD COLUMN last_capability_at timestamptz,
    ADD COLUMN capability_revision bigint NOT NULL DEFAULT 0,
    ADD COLUMN accepting_assignments boolean NOT NULL DEFAULT true;

ALTER TABLE agents
    ADD CONSTRAINT agents_capability_revision_nonnegative CHECK (capability_revision >= 0);

UPDATE agents
SET last_seen_at = updated_at,
    last_capability_at = updated_at;

CREATE TABLE agent_capability_snapshots (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    revision bigint NOT NULL,
    observed_at timestamptz NOT NULL,
    accepting_assignments boolean NOT NULL,
    agent_version text NOT NULL,
    operating_system text NOT NULL,
    architecture text NOT NULL,
    hostname text NOT NULL,
    execution_user text NOT NULL,
    execution_backends text[] NOT NULL,
    runtimes text[] NOT NULL,
    capabilities text[] NOT NULL,
    document_digest text NOT NULL,
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (agent_id, revision),
    CONSTRAINT agent_capability_snapshots_digest_unique UNIQUE (agent_id, document_digest),
    CONSTRAINT agent_capability_snapshots_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT agent_capability_snapshots_revision_positive CHECK (revision > 0),
    CONSTRAINT agent_capability_snapshots_digest_format CHECK (
        document_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT agent_capability_snapshots_document_object CHECK (
        jsonb_typeof(document) = 'object'
    )
);

CREATE INDEX agent_capability_snapshots_observed_index
    ON agent_capability_snapshots (agent_id, observed_at DESC, revision DESC);

ALTER TABLE executions
    ADD COLUMN observation_confidence text NOT NULL DEFAULT 'current',
    ADD COLUMN confidence_updated_at timestamptz NOT NULL DEFAULT transaction_timestamp();

ALTER TABLE executions
    ADD CONSTRAINT executions_observation_confidence_known CHECK (
        observation_confidence IN ('current', 'stale', 'uncertain', 'lost')
    );

CREATE INDEX executions_stale_observation_index
    ON executions (updated_at, id)
    WHERE phase IN ('accepted', 'running') AND observation_confidence = 'current';
