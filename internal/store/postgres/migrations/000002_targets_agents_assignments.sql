CREATE TABLE targets (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    name text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL DEFAULT 'active',
    current_generation_id uuid,
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT targets_namespace_name_unique UNIQUE (namespace_id, name),
    CONSTRAINT targets_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT targets_id_pair_unique UNIQUE (id, current_generation_id),
    CONSTRAINT targets_name_format CHECK (
        length(name) BETWEEN 1 AND 128
        AND name ~ '^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$'
    ),
    CONSTRAINT targets_kind_known CHECK (kind IN ('host', 'slurm')),
    CONSTRAINT targets_state_known CHECK (
        state IN ('active', 'draining', 'disabled', 'retired')
    ),
    CONSTRAINT targets_revision_positive CHECK (revision > 0)
);

CREATE TABLE target_generations (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    target_id uuid NOT NULL REFERENCES targets (id) ON DELETE RESTRICT,
    generation bigint NOT NULL,
    execution_backend text NOT NULL,
    control_transport text NOT NULL,
    runtimes text[] NOT NULL,
    operating_systems text[] NOT NULL,
    architectures text[] NOT NULL,
    capabilities text[] NOT NULL,
    policy jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT target_generations_number_unique UNIQUE (target_id, generation),
    CONSTRAINT target_generations_target_id_unique UNIQUE (target_id, id),
    CONSTRAINT target_generations_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT target_generations_namespace_target_id_unique UNIQUE (namespace_id, target_id, id),
    CONSTRAINT target_generations_namespace_target_fk
        FOREIGN KEY (namespace_id, target_id)
        REFERENCES targets (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT target_generations_backend_known CHECK (
        execution_backend IN ('subprocess', 'slurm')
    ),
    CONSTRAINT target_generations_transport_known CHECK (
        control_transport = 'agent-api'
    ),
    CONSTRAINT target_generations_generation_positive CHECK (generation > 0),
    CONSTRAINT target_generations_runtimes_known CHECK (
        cardinality(runtimes) > 0
        AND runtimes <@ ARRAY['native', 'container']::text[]
    ),
    CONSTRAINT target_generations_policy_object CHECK (jsonb_typeof(policy) = 'object')
);

ALTER TABLE targets
    ADD CONSTRAINT targets_current_generation_fk
    FOREIGN KEY (id, current_generation_id)
    REFERENCES target_generations (target_id, id) ON DELETE RESTRICT;

CREATE TABLE partitions (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    target_generation_id uuid NOT NULL REFERENCES target_generations (id) ON DELETE RESTRICT,
    name text NOT NULL,
    is_default boolean NOT NULL DEFAULT false,
    state text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT partitions_generation_name_unique UNIQUE (target_generation_id, name),
    CONSTRAINT partitions_namespace_generation_fk
        FOREIGN KEY (namespace_id, target_generation_id)
        REFERENCES target_generations (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT partitions_name_format CHECK (
        length(name) BETWEEN 1 AND 128
        AND name ~ '^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$'
    ),
    CONSTRAINT partitions_state_known CHECK (
        state IN ('active', 'draining', 'disabled', 'retired')
    )
);

CREATE UNIQUE INDEX partitions_one_default_index
    ON partitions (target_generation_id)
    WHERE is_default;

ALTER TABLE jobs
    ADD COLUMN target_id uuid,
    ADD COLUMN target_generation_id uuid;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_namespace_id_unique UNIQUE (namespace_id, id);

ALTER TABLE jobs
    ADD CONSTRAINT jobs_target_generation_fk
    FOREIGN KEY (target_id, target_generation_id)
    REFERENCES target_generations (target_id, id) ON DELETE RESTRICT;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_namespace_target_generation_fk
    FOREIGN KEY (namespace_id, target_id, target_generation_id)
    REFERENCES target_generations (namespace_id, target_id, id) ON DELETE RESTRICT;

ALTER TABLE jobs DROP CONSTRAINT jobs_phase_initial;
ALTER TABLE jobs
    ADD CONSTRAINT jobs_phase_known CHECK (phase IN ('accepted', 'assigning'));

CREATE INDEX jobs_assignment_candidate_index
    ON jobs (created_at, id)
    WHERE phase = 'accepted' AND desired_state = 'run';

CREATE TABLE agents (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    target_id uuid NOT NULL REFERENCES targets (id) ON DELETE RESTRICT,
    target_generation_id uuid NOT NULL REFERENCES target_generations (id) ON DELETE RESTRICT,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    status text NOT NULL DEFAULT 'active',
    agent_version text NOT NULL,
    protocol_versions text[] NOT NULL,
    operating_system text NOT NULL,
    architecture text NOT NULL,
    hostname text NOT NULL,
    execution_user text NOT NULL,
    execution_backends text[] NOT NULL,
    runtimes text[] NOT NULL,
    capabilities text[] NOT NULL,
    registration_digest text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT agents_target_generation_fk
        FOREIGN KEY (target_id, target_generation_id)
        REFERENCES target_generations (target_id, id) ON DELETE RESTRICT,
    CONSTRAINT agents_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT agents_namespace_target_generation_fk
        FOREIGN KEY (namespace_id, target_id, target_generation_id)
        REFERENCES target_generations (namespace_id, target_id, id) ON DELETE RESTRICT,
    CONSTRAINT agents_status_known CHECK (status IN ('active', 'draining', 'disabled', 'retired')),
    CONSTRAINT agents_registration_digest_format CHECK (
        registration_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT agents_text_bounds CHECK (
        length(agent_version) BETWEEN 1 AND 128
        AND length(operating_system) BETWEEN 1 AND 128
        AND length(architecture) BETWEEN 1 AND 128
        AND length(hostname) BETWEEN 1 AND 255
        AND length(execution_user) BETWEEN 1 AND 512
    )
);

CREATE INDEX agents_assignment_index
    ON agents (namespace_id, target_generation_id, principal_id, created_at, id)
    WHERE status = 'active';

ALTER TABLE audit_events
    ALTER COLUMN actor_principal_id DROP NOT NULL,
    ADD COLUMN actor_kind text NOT NULL DEFAULT 'principal',
    ADD COLUMN actor_agent_id uuid REFERENCES agents (id) ON DELETE RESTRICT;

ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_actor_consistent CHECK (
        (actor_kind = 'principal' AND actor_principal_id IS NOT NULL AND actor_agent_id IS NULL)
        OR (actor_kind = 'agent' AND actor_principal_id IS NULL AND actor_agent_id IS NOT NULL)
        OR (actor_kind = 'system' AND actor_principal_id IS NULL AND actor_agent_id IS NULL)
    );

ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_namespace_agent_fk
    FOREIGN KEY (namespace_id, actor_agent_id)
    REFERENCES agents (namespace_id, id) ON DELETE RESTRICT;

CREATE TABLE agent_enrollment_tokens (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    target_id uuid NOT NULL REFERENCES targets (id) ON DELETE RESTRICT,
    target_generation_id uuid NOT NULL REFERENCES target_generations (id) ON DELETE RESTRICT,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    created_by_principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    expected_user text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    request_digest text NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    used_by_agent_id uuid REFERENCES agents (id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT agent_enrollment_target_generation_fk
        FOREIGN KEY (target_id, target_generation_id)
        REFERENCES target_generations (target_id, id) ON DELETE RESTRICT,
    CONSTRAINT agent_enrollment_namespace_target_generation_fk
        FOREIGN KEY (namespace_id, target_id, target_generation_id)
        REFERENCES target_generations (namespace_id, target_id, id) ON DELETE RESTRICT,
    CONSTRAINT agent_enrollment_namespace_used_agent_fk
        FOREIGN KEY (namespace_id, used_by_agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT agent_enrollment_request_digest_format CHECK (
        request_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT agent_enrollment_expected_user_length CHECK (
        length(expected_user) BETWEEN 1 AND 512
    ),
    CONSTRAINT agent_enrollment_use_consistent CHECK (
        (used_at IS NULL AND used_by_agent_id IS NULL)
        OR (used_at IS NOT NULL AND used_by_agent_id IS NOT NULL)
    )
);

CREATE TABLE agent_sessions (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz,
    revoked_at timestamptz,
    replaced_by_session_id uuid REFERENCES agent_sessions (id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);

ALTER TABLE agent_sessions
    ADD CONSTRAINT agent_sessions_namespace_agent_fk
    FOREIGN KEY (namespace_id, agent_id)
    REFERENCES agents (namespace_id, id) ON DELETE RESTRICT;

CREATE INDEX agent_sessions_active_index
    ON agent_sessions (agent_id, expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE runs (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE RESTRICT,
    run_number integer NOT NULL,
    phase text NOT NULL,
    desired_state text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT runs_job_number_unique UNIQUE (job_id, run_number),
    CONSTRAINT runs_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT runs_namespace_job_fk
        FOREIGN KEY (namespace_id, job_id)
        REFERENCES jobs (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT runs_number_positive CHECK (run_number > 0),
    CONSTRAINT runs_phase_known CHECK (phase IN ('ready', 'assigning')),
    CONSTRAINT runs_desired_state_known CHECK (desired_state = 'run')
);

CREATE TABLE executions (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    run_id uuid NOT NULL REFERENCES runs (id) ON DELETE RESTRICT,
    target_id uuid NOT NULL REFERENCES targets (id) ON DELETE RESTRICT,
    target_generation_id uuid NOT NULL REFERENCES target_generations (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    phase text NOT NULL,
    effective_spec_digest text NOT NULL,
    effective_spec jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT executions_one_per_run UNIQUE (run_id),
    CONSTRAINT executions_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT executions_namespace_run_fk
        FOREIGN KEY (namespace_id, run_id)
        REFERENCES runs (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT executions_target_generation_fk
        FOREIGN KEY (target_id, target_generation_id)
        REFERENCES target_generations (target_id, id) ON DELETE RESTRICT,
    CONSTRAINT executions_namespace_target_generation_fk
        FOREIGN KEY (namespace_id, target_id, target_generation_id)
        REFERENCES target_generations (namespace_id, target_id, id) ON DELETE RESTRICT,
    CONSTRAINT executions_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT executions_phase_known CHECK (phase = 'planned'),
    CONSTRAINT executions_effective_digest_format CHECK (
        effective_spec_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT executions_effective_spec_object CHECK (
        jsonb_typeof(effective_spec) = 'object'
    )
);

CREATE TABLE assignments (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL REFERENCES executions (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    state text NOT NULL,
    document jsonb NOT NULL,
    delivery_count integer NOT NULL DEFAULT 0,
    last_delivered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT assignments_execution_unique UNIQUE (execution_id),
    CONSTRAINT assignments_namespace_execution_fk
        FOREIGN KEY (namespace_id, execution_id)
        REFERENCES executions (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT assignments_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT assignments_state_inert CHECK (state = 'offered'),
    CONSTRAINT assignments_document_object CHECK (jsonb_typeof(document) = 'object'),
    CONSTRAINT assignments_delivery_count_nonnegative CHECK (delivery_count >= 0)
);

CREATE INDEX assignments_agent_delivery_index
    ON assignments (agent_id, created_at, id)
    WHERE state = 'offered';
