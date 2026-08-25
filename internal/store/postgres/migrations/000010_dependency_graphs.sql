CREATE TABLE graphs (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    owner_principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    name text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    request_digest text NOT NULL,
    request_document jsonb NOT NULL,
    max_active integer NOT NULL,
    unsatisfied_policy text NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT graphs_namespace_id_unique UNIQUE (namespace_id, id),
    CONSTRAINT graphs_name_length CHECK (length(name) BETWEEN 1 AND 128),
    CONSTRAINT graphs_labels_object CHECK (jsonb_typeof(labels) = 'object'),
    CONSTRAINT graphs_request_document_object CHECK (jsonb_typeof(request_document) = 'object'),
    CONSTRAINT graphs_request_digest_format CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT graphs_max_active_bounded CHECK (max_active BETWEEN 1 AND 10000),
    CONSTRAINT graphs_unsatisfied_policy_known CHECK (
        unsatisfied_policy IN ('skip', 'cancel', 'blocked')
    ),
    CONSTRAINT graphs_revision_positive CHECK (revision > 0)
);

ALTER TABLE jobs
    ADD COLUMN graph_id uuid,
    ADD COLUMN graph_index integer,
    ADD COLUMN graph_disposition text;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_graph_binding_complete CHECK (
        (graph_id IS NULL AND graph_index IS NULL AND graph_disposition IS NULL)
        OR
        (graph_id IS NOT NULL AND graph_index >= 0 AND graph_index < 10000)
    ),
    ADD CONSTRAINT jobs_graph_fk
        FOREIGN KEY (namespace_id, graph_id)
        REFERENCES graphs (namespace_id, id) ON DELETE RESTRICT,
    ADD CONSTRAINT jobs_graph_index_unique UNIQUE (graph_id, graph_index),
    ADD CONSTRAINT jobs_graph_disposition_known CHECK (
        graph_disposition IS NULL OR graph_disposition IN ('skipped', 'cancelled', 'blocked')
    ),
    ADD CONSTRAINT jobs_grouping_exclusive CHECK (
        collection_id IS NULL OR graph_id IS NULL
    );

CREATE TABLE graph_nodes (
    graph_id uuid NOT NULL REFERENCES graphs (id) ON DELETE RESTRICT,
    node_index integer NOT NULL,
    node_name text NOT NULL,
    job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE RESTRICT,
    PRIMARY KEY (graph_id, node_index),
    CONSTRAINT graph_nodes_name_unique UNIQUE (graph_id, node_name),
    CONSTRAINT graph_nodes_job_unique UNIQUE (job_id),
    CONSTRAINT graph_nodes_index_bounded CHECK (node_index >= 0 AND node_index < 10000),
    CONSTRAINT graph_nodes_name_length CHECK (length(node_name) BETWEEN 1 AND 128)
);

CREATE TABLE graph_edges (
    graph_id uuid NOT NULL REFERENCES graphs (id) ON DELETE RESTRICT,
    upstream_job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE RESTRICT,
    downstream_job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE RESTRICT,
    predicate text NOT NULL,
    outcomes text[] NOT NULL DEFAULT '{}'::text[],
    PRIMARY KEY (graph_id, upstream_job_id, downstream_job_id),
    CONSTRAINT graph_edges_no_self_reference CHECK (upstream_job_id <> downstream_job_id),
    CONSTRAINT graph_edges_predicate_known CHECK (
        predicate IN ('success', 'failure', 'any-terminal', 'outcomes')
    ),
    CONSTRAINT graph_edges_outcomes_known CHECK (
        outcomes <@ ARRAY['success', 'failure', 'cancelled', 'timed_out', 'aborted', 'lost']::text[]
        AND ((predicate = 'outcomes' AND cardinality(outcomes) > 0)
            OR (predicate <> 'outcomes' AND cardinality(outcomes) = 0))
    )
);

CREATE INDEX jobs_graph_phase_index
    ON jobs (graph_id, phase, created_at, id)
    WHERE graph_id IS NOT NULL;

CREATE INDEX graph_edges_downstream_index
    ON graph_edges (downstream_job_id, upstream_job_id);
