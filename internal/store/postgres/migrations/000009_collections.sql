CREATE TABLE collections (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    owner_principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    name text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    request_digest text NOT NULL,
    request_document jsonb NOT NULL,
    max_active integer NOT NULL,
    failure_policy text NOT NULL,
    array_policy text NOT NULL,
    array_mode text NOT NULL DEFAULT 'individual',
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT collections_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT collections_name_length CHECK (length(name) BETWEEN 1 AND 128),
    CONSTRAINT collections_labels_object CHECK (jsonb_typeof(labels) = 'object'),
    CONSTRAINT collections_request_document_object CHECK (jsonb_typeof(request_document) = 'object'),
    CONSTRAINT collections_request_digest_format CHECK (
        request_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT collections_max_active_bounded CHECK (max_active BETWEEN 1 AND 10000),
    CONSTRAINT collections_failure_policy_known CHECK (
        failure_policy IN ('continue', 'fail-fast')
    ),
    CONSTRAINT collections_array_policy_known CHECK (
        array_policy IN ('never', 'prefer', 'require')
    ),
    CONSTRAINT collections_array_mode_known CHECK (
        array_mode IN ('individual', 'slurm-array')
    ),
    CONSTRAINT collections_revision_positive CHECK (revision > 0)
);

ALTER TABLE jobs
    ADD COLUMN collection_id uuid,
    ADD COLUMN collection_index integer;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_collection_binding_complete CHECK (
        (collection_id IS NULL AND collection_index IS NULL)
        OR
        (collection_id IS NOT NULL AND collection_index >= 0 AND collection_index < 10000)
    ),
    ADD CONSTRAINT jobs_collection_fk
        FOREIGN KEY (namespace_id, collection_id)
        REFERENCES collections (namespace_id, id) ON DELETE RESTRICT,
    ADD CONSTRAINT jobs_collection_index_unique UNIQUE (collection_id, collection_index);

CREATE TABLE collection_items (
    collection_id uuid NOT NULL REFERENCES collections (id) ON DELETE RESTRICT,
    item_index integer NOT NULL,
    item_name text NOT NULL,
    job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE RESTRICT,
    PRIMARY KEY (collection_id, item_index),
    CONSTRAINT collection_items_name_unique UNIQUE (collection_id, item_name),
    CONSTRAINT collection_items_job_unique UNIQUE (job_id),
    CONSTRAINT collection_items_index_bounded CHECK (item_index >= 0 AND item_index < 10000),
    CONSTRAINT collection_items_name_length CHECK (length(item_name) BETWEEN 1 AND 128)
);

CREATE INDEX jobs_collection_phase_index
    ON jobs (collection_id, phase, created_at, id)
    WHERE collection_id IS NOT NULL;
