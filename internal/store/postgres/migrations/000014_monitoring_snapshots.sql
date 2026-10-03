CREATE TABLE control_instance (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);
INSERT INTO control_instance (singleton) VALUES (true);
ALTER TABLE jobs
    ADD COLUMN started_at timestamptz,
    ADD COLUMN started_recorded_at timestamptz,
    ADD COLUMN started_provenance text,
    ADD COLUMN completed_recorded_at timestamptz,
    ADD COLUMN completed_provenance text;
CREATE INDEX jobs_namespace_created_desc_index
    ON jobs (namespace_id, created_at DESC, id DESC);
CREATE INDEX jobs_namespace_completion_index
    ON jobs (namespace_id, completed_at DESC, id) WHERE phase = 'terminal';
CREATE INDEX jobs_namespace_owner_created_index
    ON jobs (namespace_id, owner_principal_id, created_at DESC, id DESC) WHERE NOT imported;

-- Backfill only actual retained observations, never arbitrary updated_at.
UPDATE jobs SET completed_provenance = 'history_import', completed_recorded_at = created_at
WHERE imported AND completed_at IS NOT NULL;
WITH starts AS (
    SELECT DISTINCT ON (r.job_id) r.job_id, ev.observed_at, ev.ingested_at, ev.event_type
    FROM runs AS r JOIN executions AS e ON e.run_id = r.id
    JOIN execution_events AS ev ON ev.execution_id = e.id
    WHERE ev.event_type = 'process.started'
       OR (ev.event_type = 'scheduler.observed' AND ev.document #>> '{spec,scheduler,state}' = 'running')
    ORDER BY r.job_id, r.run_number, ev.source_sequence
)
UPDATE jobs AS j SET started_at = starts.observed_at, started_recorded_at = starts.ingested_at,
    started_provenance = starts.event_type FROM starts WHERE j.id = starts.job_id;
WITH completions AS (
    SELECT DISTINCT ON (r.job_id) r.job_id, ev.observed_at, ev.ingested_at, ev.event_type
    FROM runs AS r JOIN executions AS e ON e.run_id = r.id
    JOIN execution_events AS ev ON ev.execution_id = e.id
    WHERE ev.event_type IN ('process.completed', 'scheduler.completed')
    ORDER BY r.job_id, r.run_number DESC, ev.source_sequence DESC
)
UPDATE jobs AS j SET completed_at = completions.observed_at, completed_recorded_at = completions.ingested_at,
    completed_provenance = completions.event_type FROM completions
WHERE j.id = completions.job_id AND j.phase = 'terminal' AND NOT j.imported;

CREATE FUNCTION record_job_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE observation record;
BEGIN
    IF OLD.phase <> 'running' AND NEW.phase = 'running' AND NEW.started_recorded_at IS NULL THEN
        SELECT ev.observed_at, ev.ingested_at, ev.event_type INTO observation
        FROM runs AS r JOIN executions AS e ON e.run_id = r.id
        JOIN execution_events AS ev ON ev.execution_id = e.id
        WHERE r.job_id = NEW.id AND (ev.event_type = 'process.started'
           OR (ev.event_type IN ('scheduler.observed', 'scheduler.submitted')
               AND ev.document #>> '{spec,scheduler,state}' = 'running'))
        ORDER BY r.run_number DESC, ev.source_sequence LIMIT 1;
        IF FOUND THEN
            NEW.started_at := observation.observed_at;
            NEW.started_recorded_at := observation.ingested_at;
            NEW.started_provenance := observation.event_type;
        ELSE
            NEW.started_recorded_at := transaction_timestamp();
            NEW.started_provenance := 'control_transition';
        END IF;
    END IF;
    IF OLD.phase <> 'terminal' AND NEW.phase = 'terminal' THEN
        IF NEW.imported THEN
            NEW.completed_provenance := 'history_import';
            NEW.completed_recorded_at := transaction_timestamp();
        ELSE
            SELECT ev.observed_at, ev.ingested_at, ev.event_type INTO observation
            FROM runs AS r JOIN executions AS e ON e.run_id = r.id
            JOIN execution_events AS ev ON ev.execution_id = e.id
            WHERE r.job_id = NEW.id AND ev.event_type IN ('process.completed', 'scheduler.completed')
            ORDER BY r.run_number DESC, ev.source_sequence DESC LIMIT 1;
            IF FOUND THEN
                NEW.completed_at := observation.observed_at;
                NEW.completed_recorded_at := observation.ingested_at;
                NEW.completed_provenance := observation.event_type;
            ELSE
                NEW.completed_at := transaction_timestamp();
                NEW.completed_recorded_at := transaction_timestamp();
                NEW.completed_provenance := 'control_transition';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER jobs_lifecycle BEFORE UPDATE OF phase ON jobs
    FOR EACH ROW EXECUTE FUNCTION record_job_lifecycle();
