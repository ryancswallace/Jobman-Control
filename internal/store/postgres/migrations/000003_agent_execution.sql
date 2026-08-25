CREATE TABLE agent_certificates (
    serial text PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    public_key_digest text NOT NULL,
    not_after timestamptz NOT NULL,
    revoked_at timestamptz,
    replaced_by_serial text REFERENCES agent_certificates (serial) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT agent_certificates_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT agent_certificates_serial_length CHECK (length(serial) BETWEEN 1 AND 64),
    CONSTRAINT agent_certificates_public_key_digest_format CHECK (
        public_key_digest ~ '^sha256:[0-9a-f]{64}$'
    )
);

CREATE INDEX agent_certificates_active_index
    ON agent_certificates (agent_id, not_after)
    WHERE revoked_at IS NULL;

ALTER TABLE jobs DROP CONSTRAINT jobs_phase_known;
ALTER TABLE jobs
    ADD COLUMN outcome text,
    ADD CONSTRAINT jobs_phase_known CHECK (
        phase IN ('accepted', 'assigning', 'accepted_execution', 'running', 'terminal')
    ),
    ADD CONSTRAINT jobs_outcome_known CHECK (
        outcome IS NULL OR outcome IN (
            'success', 'failure', 'cancelled', 'timed_out', 'aborted', 'lost'
        )
    ),
    ADD CONSTRAINT jobs_terminal_consistent CHECK (
        (phase = 'terminal' AND outcome IS NOT NULL)
        OR (phase <> 'terminal' AND outcome IS NULL)
    );
ALTER TABLE jobs DROP CONSTRAINT jobs_desired_state_initial;
ALTER TABLE jobs
    ADD CONSTRAINT jobs_desired_state_known CHECK (desired_state IN ('run', 'cancel'));

ALTER TABLE runs DROP CONSTRAINT runs_phase_known;
ALTER TABLE runs
    ADD COLUMN outcome text,
    ADD CONSTRAINT runs_phase_known CHECK (
        phase IN ('ready', 'assigning', 'accepted', 'running', 'terminal')
    ),
    ADD CONSTRAINT runs_outcome_known CHECK (
        outcome IS NULL OR outcome IN (
            'success', 'failure', 'cancelled', 'timed_out', 'aborted', 'lost'
        )
    ),
    ADD CONSTRAINT runs_terminal_consistent CHECK (
        (phase = 'terminal' AND outcome IS NOT NULL)
        OR (phase <> 'terminal' AND outcome IS NULL)
    );
ALTER TABLE runs DROP CONSTRAINT runs_desired_state_known;
ALTER TABLE runs
    ADD CONSTRAINT runs_desired_state_known CHECK (desired_state IN ('run', 'cancel'));

ALTER TABLE executions DROP CONSTRAINT executions_phase_known;
ALTER TABLE executions
    ADD COLUMN revision bigint NOT NULL DEFAULT 1,
    ADD COLUMN accepted_at timestamptz,
    ADD COLUMN last_event_sequence bigint NOT NULL DEFAULT 0,
    ADD COLUMN native_id text,
    ADD COLUMN outcome text,
    ADD COLUMN process_result jsonb;
ALTER TABLE executions
    ADD CONSTRAINT executions_phase_known CHECK (
        phase IN ('planned', 'accepted', 'running', 'terminal')
    ),
    ADD CONSTRAINT executions_revision_positive CHECK (revision > 0),
    ADD CONSTRAINT executions_event_sequence_nonnegative CHECK (last_event_sequence >= 0),
    ADD CONSTRAINT executions_outcome_known CHECK (
        outcome IS NULL OR outcome IN (
            'success', 'failure', 'cancelled', 'timed_out', 'aborted', 'lost'
        )
    ),
    ADD CONSTRAINT executions_terminal_consistent CHECK (
        (phase = 'terminal' AND outcome IS NOT NULL AND process_result IS NOT NULL)
        OR (phase <> 'terminal' AND outcome IS NULL AND process_result IS NULL)
    );

ALTER TABLE assignments DROP CONSTRAINT assignments_state_inert;
ALTER TABLE assignments
    ADD COLUMN accepted_at timestamptz,
    ADD COLUMN rejected_at timestamptz,
    ADD COLUMN rejection_code text,
    ADD COLUMN acceptance_digest text,
    ADD COLUMN acceptance_document jsonb;
ALTER TABLE assignments
    ADD CONSTRAINT assignments_state_known CHECK (
        state IN ('offered', 'accepted', 'rejected', 'withdrawn')
    ),
    ADD CONSTRAINT assignments_acceptance_digest_format CHECK (
        acceptance_digest IS NULL OR acceptance_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    ADD CONSTRAINT assignments_acceptance_document_object CHECK (
        acceptance_document IS NULL OR jsonb_typeof(acceptance_document) = 'object'
    ),
    ADD CONSTRAINT assignments_decision_consistent CHECK (
        (state = 'offered' AND accepted_at IS NULL AND rejected_at IS NULL
            AND acceptance_digest IS NULL AND acceptance_document IS NULL)
        OR (state = 'accepted' AND accepted_at IS NOT NULL AND rejected_at IS NULL
            AND acceptance_digest IS NOT NULL AND acceptance_document IS NOT NULL)
        OR (state = 'rejected' AND accepted_at IS NULL AND rejected_at IS NOT NULL
            AND rejection_code IS NOT NULL)
        OR (state = 'withdrawn' AND accepted_at IS NULL)
    );

DROP INDEX assignments_agent_delivery_index;
CREATE INDEX assignments_agent_delivery_index
    ON assignments (agent_id, created_at, id)
    WHERE state = 'offered';

CREATE TABLE execution_events (
    event_id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL REFERENCES executions (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    source_sequence bigint NOT NULL,
    event_type text NOT NULL,
    observed_at timestamptz NOT NULL,
    ingested_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    document_digest text NOT NULL,
    document jsonb NOT NULL,
    CONSTRAINT execution_events_namespace_execution_fk
        FOREIGN KEY (namespace_id, execution_id)
        REFERENCES executions (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT execution_events_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT execution_events_source_sequence_unique UNIQUE (
        execution_id, agent_id, source_sequence
    ),
    CONSTRAINT execution_events_sequence_positive CHECK (source_sequence > 0),
    CONSTRAINT execution_events_type_known CHECK (
        event_type IN ('process.started', 'process.completed')
    ),
    CONSTRAINT execution_events_digest_format CHECK (
        document_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT execution_events_document_object CHECK (jsonb_typeof(document) = 'object')
);

CREATE TABLE desired_actions (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL REFERENCES executions (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    revision bigint NOT NULL DEFAULT 1,
    action_type text NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    document jsonb NOT NULL,
    delivery_count integer NOT NULL DEFAULT 0,
    last_delivered_at timestamptz,
    acknowledged_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT desired_actions_namespace_execution_fk
        FOREIGN KEY (namespace_id, execution_id)
        REFERENCES executions (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT desired_actions_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT desired_actions_execution_type_unique UNIQUE (execution_id, action_type),
    CONSTRAINT desired_actions_revision_positive CHECK (revision > 0),
    CONSTRAINT desired_actions_type_known CHECK (action_type = 'cancel'),
    CONSTRAINT desired_actions_state_known CHECK (state IN ('pending', 'acknowledged')),
    CONSTRAINT desired_actions_document_object CHECK (jsonb_typeof(document) = 'object'),
    CONSTRAINT desired_actions_delivery_count_nonnegative CHECK (delivery_count >= 0),
    CONSTRAINT desired_actions_acknowledgement_consistent CHECK (
        (state = 'pending' AND acknowledged_at IS NULL)
        OR (state = 'acknowledged' AND acknowledged_at IS NOT NULL)
    )
);

CREATE INDEX desired_actions_agent_delivery_index
    ON desired_actions (agent_id, created_at, id)
    WHERE state = 'pending';
