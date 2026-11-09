CREATE VIEW service_lifecycle_outbox AS
SELECT id, namespace_id, topic, aggregate_type, aggregate_id, payload, created_at, published_at
FROM outbox
WHERE topic <> 'monitoring.job_terminal.v1' AND topic IN (
    'assignment.offered', 'collection.accepted', 'execution.observation_stale',
    'graph.accepted', 'history.imported', 'job.accepted',
    'target.generation_created', 'target.state_changed'
);

CREATE INDEX outbox_lifecycle_pending_index ON outbox (created_at, id)
    WHERE published_at IS NULL AND topic <> 'monitoring.job_terminal.v1';

CREATE UNIQUE INDEX service_operator_audit_lifecycle_receipt_index
    ON service_operator_audit (resource_id) WHERE action = 'lifecycle.ack';

CREATE FUNCTION jobman_control_export_lifecycle(batch_limit integer) RETURNS jsonb
LANGUAGE plpgsql STRICT STABLE SECURITY DEFINER AS $$
BEGIN
    IF batch_limit NOT BETWEEN 1 AND 100 THEN
        RAISE EXCEPTION 'lifecycle export limit must be between 1 and 100' USING ERRCODE = '22023';
    END IF;
    RETURN (
        SELECT jsonb_build_object(
            'version', 1,
            'restoreEpoch', state.restore_epoch,
            'events', COALESCE((
                SELECT jsonb_agg(to_jsonb(event) ORDER BY event.created_at, event.id)
                FROM (
                    SELECT id, namespace_id, topic, aggregate_type, aggregate_id, payload, created_at
                    FROM service_lifecycle_outbox
                    WHERE published_at IS NULL
                    ORDER BY created_at, id LIMIT batch_limit
                ) AS event
            ), '[]'::jsonb)
        )
        FROM service_recovery_state AS state WHERE state.singleton
    );
END;
$$;

CREATE FUNCTION jobman_control_ack_lifecycle(expected_epoch bigint, event_id uuid, receipt_ref text)
RETURNS boolean
LANGUAGE plpgsql STRICT SECURITY DEFINER AS $$
DECLARE
    observed_epoch bigint;
    held boolean;
    existing_receipt text;
BEGIN
    IF expected_epoch < 1 OR receipt_ref !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$' THEN
        RAISE EXCEPTION 'invalid lifecycle acknowledgement' USING ERRCODE = '22023';
    END IF;
    SELECT restore_epoch, reconciliation_hold INTO STRICT observed_epoch, held
    FROM service_recovery_state WHERE singleton FOR SHARE;
    IF held OR observed_epoch <> expected_epoch THEN
        RAISE EXCEPTION 'restore hold or epoch changed' USING ERRCODE = '40001';
    END IF;

    UPDATE service_lifecycle_outbox SET published_at = clock_timestamp()
    WHERE id = event_id AND published_at IS NULL;
    IF FOUND THEN
        INSERT INTO service_operator_audit (actor_role, action, resource_id, change_reference, details)
        VALUES (session_user, 'lifecycle.ack', event_id::text, receipt_ref,
            jsonb_build_object('epoch', observed_epoch));
        RETURN true;
    END IF;

    SELECT change_reference INTO existing_receipt FROM service_operator_audit
    WHERE action = 'lifecycle.ack' AND resource_id = event_id::text;
    IF existing_receipt = receipt_ref THEN
        RETURN false;
    END IF;
    RAISE EXCEPTION 'event is unavailable for this receipt' USING ERRCODE = '40001';
END;
$$;

DO $$
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.jobman_control_export_lifecycle(integer) SET search_path = pg_catalog, %I, pg_temp',
        current_schema(), current_schema()
    );
    EXECUTE format(
        'ALTER FUNCTION %I.jobman_control_ack_lifecycle(bigint, uuid, text) SET search_path = pg_catalog, %I, pg_temp',
        current_schema(), current_schema()
    );
END;
$$;
REVOKE ALL ON FUNCTION jobman_control_export_lifecycle(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION jobman_control_ack_lifecycle(bigint, uuid, text) FROM PUBLIC;
