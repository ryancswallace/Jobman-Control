ALTER TABLE target_generations
    ADD COLUMN artifact_stores jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT target_generations_artifact_stores_array CHECK (
        jsonb_typeof(artifact_stores) = 'array'
    );

ALTER TABLE jobs
    ADD COLUMN resolved_artifact_stores jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT jobs_resolved_artifact_stores_array CHECK (
        jsonb_typeof(resolved_artifact_stores) = 'array'
    );

CREATE TABLE execution_artifacts (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    execution_id uuid NOT NULL REFERENCES executions (id) ON DELETE RESTRICT,
    name text NOT NULL,
    store_name text NOT NULL,
    store_version bigint NOT NULL,
    object_key text NOT NULL,
    byte_length bigint NOT NULL,
    checksum text NOT NULL,
    published_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (execution_id, name),
    CONSTRAINT execution_artifacts_namespace_execution_fk
        FOREIGN KEY (namespace_id, execution_id)
        REFERENCES executions (namespace_id, id) ON DELETE RESTRICT,
    CONSTRAINT execution_artifacts_name_format CHECK (
        length(name) BETWEEN 1 AND 128
        AND name ~ '^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$'
    ),
    CONSTRAINT execution_artifacts_store_format CHECK (
        length(store_name) BETWEEN 1 AND 64
        AND store_name ~ '^[a-z]([a-z0-9._-]{0,62}[a-z0-9])?$'
    ),
    CONSTRAINT execution_artifacts_store_version_positive CHECK (store_version > 0),
    CONSTRAINT execution_artifacts_object_key_bounded CHECK (
        length(object_key) BETWEEN 1 AND 65536
    ),
    CONSTRAINT execution_artifacts_byte_length_nonnegative CHECK (byte_length >= 0),
    CONSTRAINT execution_artifacts_checksum_format CHECK (
        checksum ~ '^sha256:[0-9a-f]{64}$'
    )
);

CREATE INDEX execution_artifacts_namespace_created_idx
    ON execution_artifacts (namespace_id, created_at DESC, execution_id, name);
