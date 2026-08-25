ALTER TABLE target_generations
    ADD COLUMN log_store_name text,
    ADD COLUMN log_store_version bigint,
    ADD CONSTRAINT target_generations_log_store_consistent CHECK (
        (log_store_name IS NULL AND log_store_version IS NULL)
        OR (log_store_name IS NOT NULL AND log_store_version IS NOT NULL
            AND log_store_name ~ '^[a-z]([a-z0-9._-]{0,62}[a-z0-9])?$'
            AND log_store_version > 0)
    );

CREATE TABLE log_streams (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL REFERENCES executions (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    stream text NOT NULL,
    store_name text NOT NULL,
    store_version bigint NOT NULL,
    state text NOT NULL DEFAULT 'capturing',
    byte_length bigint NOT NULL DEFAULT 0,
    last_sequence bigint NOT NULL DEFAULT 0,
    truncated boolean NOT NULL DEFAULT false,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (execution_id, stream),
    CONSTRAINT log_streams_namespace_execution_fk
        FOREIGN KEY (namespace_id, execution_id)
        REFERENCES executions (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT log_streams_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT log_streams_stream_known CHECK (stream IN ('stdout', 'stderr')),
    CONSTRAINT log_streams_store_name_format CHECK (
        store_name ~ '^[a-z]([a-z0-9._-]{0,62}[a-z0-9])?$'
    ),
    CONSTRAINT log_streams_store_version_positive CHECK (store_version > 0),
    CONSTRAINT log_streams_state_known CHECK (state IN ('capturing', 'complete')),
    CONSTRAINT log_streams_length_nonnegative CHECK (byte_length >= 0),
    CONSTRAINT log_streams_sequence_nonnegative CHECK (last_sequence >= 0),
    CONSTRAINT log_streams_completion_consistent CHECK (
        (state = 'capturing' AND completed_at IS NULL AND truncated = false)
        OR (state = 'complete' AND completed_at IS NOT NULL)
    )
);

CREATE TABLE log_chunks (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    stream text NOT NULL,
    sequence bigint NOT NULL,
    store_name text NOT NULL,
    store_version bigint NOT NULL,
    object_key text NOT NULL,
    byte_offset bigint NOT NULL,
    byte_length bigint NOT NULL,
    checksum text NOT NULL,
    captured_at timestamptz NOT NULL,
    complete boolean NOT NULL,
    truncated boolean NOT NULL,
    committed_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    document_digest text NOT NULL,
    document jsonb NOT NULL,
    PRIMARY KEY (execution_id, stream, sequence),
    CONSTRAINT log_chunks_stream_fk
        FOREIGN KEY (execution_id, stream)
        REFERENCES log_streams (execution_id, stream) ON DELETE RESTRICT,
    CONSTRAINT log_chunks_namespace_execution_fk
        FOREIGN KEY (namespace_id, execution_id)
        REFERENCES executions (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT log_chunks_namespace_agent_fk
        FOREIGN KEY (namespace_id, agent_id)
        REFERENCES agents (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT log_chunks_stream_known CHECK (stream IN ('stdout', 'stderr')),
    CONSTRAINT log_chunks_sequence_positive CHECK (sequence > 0),
    CONSTRAINT log_chunks_store_version_positive CHECK (store_version > 0),
    CONSTRAINT log_chunks_store_name_format CHECK (
        store_name ~ '^[a-z]([a-z0-9._-]{0,62}[a-z0-9])?$'
    ),
    CONSTRAINT log_chunks_object_key_length CHECK (length(object_key) BETWEEN 1 AND 1024),
    CONSTRAINT log_chunks_offset_nonnegative CHECK (byte_offset >= 0),
    CONSTRAINT log_chunks_length_nonnegative CHECK (byte_length >= 0),
    CONSTRAINT log_chunks_length_bounded CHECK (byte_length <= 262144),
    CONSTRAINT log_chunks_empty_only_terminal CHECK (byte_length > 0 OR complete = true),
    CONSTRAINT log_chunks_terminal_consistent CHECK (truncated = false OR complete = true),
    CONSTRAINT log_chunks_checksum_format CHECK (checksum ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT log_chunks_document_digest_format CHECK (
        document_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT log_chunks_document_object CHECK (jsonb_typeof(document) = 'object'),
    CONSTRAINT log_chunks_store_object_unique UNIQUE (store_name, store_version, object_key)
);

CREATE INDEX log_chunks_manifest_index
    ON log_chunks (execution_id, stream, sequence);
