CREATE TABLE service_operator_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_role text NOT NULL,
    action text NOT NULL,
    resource_id text NOT NULL,
    change_reference text NOT NULL CHECK (change_reference ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$'),
    details jsonb NOT NULL CHECK (jsonb_typeof(details) = 'object'),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION jobman_control_set_recovery(
    expected_epoch bigint, next_epoch bigint, enter_hold boolean, change_ref text
) RETURNS TABLE (hold boolean, epoch bigint, reason text, updated_at timestamptz)
LANGUAGE plpgsql STRICT SECURITY DEFINER AS $$
DECLARE
    updated service_recovery_state%ROWTYPE;
BEGIN
    IF expected_epoch < 1 OR
        (enter_hold AND next_epoch <= expected_epoch) OR
        (NOT enter_hold AND next_epoch <> expected_epoch) OR
        change_ref !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$' THEN
        RAISE EXCEPTION 'invalid recovery transition' USING ERRCODE = '22023';
    END IF;
    -- The coordinator holds a shared row lock until its assignment commits.
    UPDATE service_recovery_state AS state
    SET reconciliation_hold = enter_hold,
        restore_epoch = next_epoch,
        reason = CASE WHEN enter_hold THEN change_ref ELSE NULL END,
        updated_at = clock_timestamp()
    WHERE state.singleton AND state.restore_epoch = expected_epoch
        AND (enter_hold OR state.reconciliation_hold)
    RETURNING state.* INTO updated;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'recovery epoch or hold changed' USING ERRCODE = '40001';
    END IF;
    INSERT INTO service_operator_audit (actor_role, action, resource_id, change_reference, details)
    VALUES (session_user, CASE WHEN enter_hold THEN 'restore.hold' ELSE 'restore.release' END,
        'service', change_ref, jsonb_build_object('previousEpoch', expected_epoch, 'epoch', next_epoch));
    RETURN QUERY SELECT updated.reconciliation_hold, updated.restore_epoch,
        COALESCE(updated.reason, ''), updated.updated_at;
END;
$$;

-- Bind the definer to the installing schema, even when a caller changes its
-- search path. Temporary relations are searched last.
DO $$
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.jobman_control_set_recovery(bigint, bigint, boolean, text) SET search_path = pg_catalog, %I, pg_temp',
        current_schema(), current_schema()
    );
END;
$$;
REVOKE ALL ON FUNCTION jobman_control_set_recovery(bigint, bigint, boolean, text) FROM PUBLIC;
