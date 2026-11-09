CREATE FUNCTION jobman_control_retire_agent(selected_agent_id uuid, selected_generation_id uuid, change_ref text)
RETURNS boolean
LANGUAGE plpgsql STRICT SECURITY DEFINER AS $$
DECLARE
    current_status text;
    retired_at timestamptz;
BEGIN
    IF change_ref !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$' THEN
        RAISE EXCEPTION 'invalid agent offboarding reference' USING ERRCODE = '22023';
    END IF;
    SELECT status INTO current_status FROM agents
    WHERE id = selected_agent_id AND target_generation_id = selected_generation_id FOR NO KEY UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'agent or generation unavailable' USING ERRCODE = '40001';
    END IF;
    IF current_status = 'retired' THEN
        RETURN false;
    END IF;

    retired_at := clock_timestamp();
    UPDATE agents SET status = 'retired', updated_at = retired_at WHERE id = selected_agent_id;
    UPDATE agent_certificates AS certificate SET revoked_at = retired_at
    WHERE certificate.agent_id = selected_agent_id AND certificate.revoked_at IS NULL;
    UPDATE agent_sessions AS session SET revoked_at = retired_at
    WHERE session.agent_id = selected_agent_id AND session.revoked_at IS NULL;
    INSERT INTO service_operator_audit (actor_role, action, resource_id, change_reference, details)
    VALUES (session_user, 'agent.retired', selected_agent_id::text, change_ref,
        jsonb_build_object('targetGenerationId', selected_generation_id));
    RETURN true;
END;
$$;

DO $$
BEGIN
    EXECUTE format(
        'ALTER FUNCTION %I.jobman_control_retire_agent(uuid, uuid, text) SET search_path = pg_catalog, %I, pg_temp',
        current_schema(), current_schema()
    );
END;
$$;
REVOKE ALL ON FUNCTION jobman_control_retire_agent(uuid, uuid, text) FROM PUBLIC;
