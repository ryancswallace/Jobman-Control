CREATE TABLE principals (
    id uuid PRIMARY KEY,
    issuer text NOT NULL,
    subject text NOT NULL,
    display_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT principals_identity_unique UNIQUE (issuer, subject),
    CONSTRAINT principals_issuer_length CHECK (length(issuer) BETWEEN 1 AND 512),
    CONSTRAINT principals_subject_length CHECK (length(subject) BETWEEN 1 AND 512),
    CONSTRAINT principals_display_name_length CHECK (length(display_name) BETWEEN 1 AND 512)
);

CREATE TABLE namespaces (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT namespaces_name_unique UNIQUE (name),
    CONSTRAINT namespaces_name_format CHECK (
        length(name) BETWEEN 1 AND 128
        AND name ~ '^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$'
    )
);

CREATE TABLE memberships (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    role text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (namespace_id, principal_id),
    CONSTRAINT memberships_role_known CHECK (
        role IN ('viewer', 'submitter', 'operator', 'namespace_admin')
    )
);

CREATE INDEX memberships_principal_index
    ON memberships (principal_id, namespace_id);

CREATE TABLE workload_revisions (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    digest text NOT NULL,
    api_version text NOT NULL,
    document jsonb NOT NULL,
    created_by uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (namespace_id, digest),
    CONSTRAINT workload_revisions_digest_format CHECK (
        digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT workload_revisions_document_object CHECK (
        jsonb_typeof(document) = 'object'
    )
);

CREATE TABLE jobs (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    owner_principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    name text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    phase text NOT NULL,
    desired_state text NOT NULL,
    placement_target text NOT NULL,
    placement_partition text,
    workload_digest text NOT NULL,
    request_digest text NOT NULL,
    request_document jsonb NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT jobs_workload_revision_fk
        FOREIGN KEY (namespace_id, workload_digest)
        REFERENCES workload_revisions (namespace_id, digest) ON DELETE RESTRICT,
    CONSTRAINT jobs_name_length CHECK (length(name) BETWEEN 1 AND 128),
    CONSTRAINT jobs_phase_initial CHECK (phase IN ('accepted')),
    CONSTRAINT jobs_desired_state_initial CHECK (desired_state IN ('run')),
    CONSTRAINT jobs_target_length CHECK (length(placement_target) BETWEEN 1 AND 128),
    CONSTRAINT jobs_partition_length CHECK (
        placement_partition IS NULL OR length(placement_partition) BETWEEN 1 AND 128
    ),
    CONSTRAINT jobs_request_digest_format CHECK (
        request_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT jobs_labels_object CHECK (jsonb_typeof(labels) = 'object'),
    CONSTRAINT jobs_request_document_object CHECK (
        jsonb_typeof(request_document) = 'object'
    ),
    CONSTRAINT jobs_revision_positive CHECK (revision > 0)
);

CREATE INDEX jobs_namespace_created_index
    ON jobs (namespace_id, created_at DESC, id);

CREATE TABLE idempotency_records (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    request_digest text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid,
    response_status integer,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    completed_at timestamptz,
    PRIMARY KEY (namespace_id, principal_id, operation, idempotency_key),
    CONSTRAINT idempotency_records_operation_length CHECK (
        length(operation) BETWEEN 1 AND 128
    ),
    CONSTRAINT idempotency_records_key_length CHECK (
        length(idempotency_key) BETWEEN 1 AND 200
    ),
    CONSTRAINT idempotency_records_request_digest_format CHECK (
        request_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT idempotency_records_completion_consistent CHECK (
        (resource_id IS NULL AND response_status IS NULL AND completed_at IS NULL)
        OR
        (resource_id IS NOT NULL AND response_status IS NOT NULL AND completed_at IS NOT NULL)
    )
);

CREATE TABLE audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    actor_principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid NOT NULL,
    request_digest text,
    idempotency_key text,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT audit_events_details_object CHECK (jsonb_typeof(details) = 'object')
);

CREATE INDEX audit_events_namespace_occurred_index
    ON audit_events (namespace_id, occurred_at DESC, id DESC);

CREATE TABLE outbox (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    topic text NOT NULL,
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    payload jsonb NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    claimed_at timestamptz,
    published_at timestamptz,
    CONSTRAINT outbox_topic_length CHECK (length(topic) BETWEEN 1 AND 128),
    CONSTRAINT outbox_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_attempts_nonnegative CHECK (attempts >= 0)
);

CREATE INDEX outbox_available_index
    ON outbox (available_at, id)
    WHERE published_at IS NULL;
