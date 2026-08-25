CREATE TABLE namespace_policies (
    namespace_id uuid PRIMARY KEY REFERENCES namespaces (id) ON DELETE RESTRICT,
    max_active_jobs integer NOT NULL DEFAULT 100,
    max_queued_jobs integer NOT NULL DEFAULT 10000,
    max_collection_items integer NOT NULL DEFAULT 10000,
    max_graph_nodes integer NOT NULL DEFAULT 10000,
    idempotency_retention interval NOT NULL DEFAULT interval '7 days',
    published_outbox_retention interval NOT NULL DEFAULT interval '7 days',
    revision bigint NOT NULL DEFAULT 1,
    last_dispatched_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT namespace_policies_limits_positive CHECK (
        max_active_jobs > 0 AND max_queued_jobs > 0
        AND max_collection_items > 0 AND max_graph_nodes > 0
    ),
    CONSTRAINT namespace_policies_group_limits_bounded CHECK (
        max_collection_items <= 10000 AND max_graph_nodes <= 10000
    ),
    CONSTRAINT namespace_policies_retention_bounded CHECK (
        idempotency_retention BETWEEN interval '1 hour' AND interval '365 days'
        AND published_outbox_retention BETWEEN interval '1 hour' AND interval '365 days'
    ),
    CONSTRAINT namespace_policies_revision_positive CHECK (revision > 0)
);

INSERT INTO namespace_policies (namespace_id)
SELECT id FROM namespaces;

CREATE FUNCTION jobman_control_create_namespace_policy()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO namespace_policies (namespace_id) VALUES (NEW.id);
    RETURN NEW;
END;
$$;

CREATE TRIGGER namespaces_create_default_policy
AFTER INSERT ON namespaces
FOR EACH ROW EXECUTE FUNCTION jobman_control_create_namespace_policy();

CREATE TABLE service_recovery_state (
    singleton boolean PRIMARY KEY DEFAULT true,
    reconciliation_hold boolean NOT NULL DEFAULT false,
    restore_epoch bigint NOT NULL DEFAULT 1,
    reason text,
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT service_recovery_state_singleton CHECK (singleton),
    CONSTRAINT service_recovery_state_epoch_positive CHECK (restore_epoch > 0),
    CONSTRAINT service_recovery_state_reason_bounded CHECK (
        reason IS NULL OR length(reason) BETWEEN 1 AND 1024
    ),
    CONSTRAINT service_recovery_state_reason_consistent CHECK (
        reconciliation_hold OR reason IS NULL
    )
);

INSERT INTO service_recovery_state (singleton) VALUES (true);

CREATE INDEX jobs_namespace_active_index
    ON jobs (namespace_id, phase, created_at, id)
    WHERE phase NOT IN ('accepted', 'terminal');

CREATE INDEX idempotency_records_retention_index
    ON idempotency_records (completed_at)
    WHERE completed_at IS NOT NULL;

CREATE INDEX outbox_published_retention_index
    ON outbox (published_at)
    WHERE published_at IS NOT NULL;
