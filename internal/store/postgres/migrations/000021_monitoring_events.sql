-- Feed publication positions are allocated under a transaction-held row lock,
-- never by a sequence that a concurrent transaction can commit out of order.
CREATE TABLE monitoring_feed_state (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 head_position bigint NOT NULL DEFAULT 0 CHECK(head_position>=0),
 retired_through bigint NOT NULL DEFAULT 0 CHECK(retired_through BETWEEN 0 AND head_position),
 retention_seconds bigint NOT NULL DEFAULT 2592000 CHECK(retention_seconds BETWEEN 86400 AND 31536000),
 last_published_at timestamptz NOT NULL DEFAULT 'epoch'
);
INSERT INTO monitoring_feed_state(singleton) VALUES(true);
CREATE TABLE monitoring_feed (
 position bigint PRIMARY KEY CHECK(position>0),
 event_id uuid NOT NULL UNIQUE,
 namespace_id uuid NOT NULL REFERENCES namespaces(id) ON DELETE RESTRICT,
 payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object' AND octet_length(payload::text)<=8192),
 published_at timestamptz NOT NULL
);
CREATE INDEX monitoring_feed_namespace_position_idx ON monitoring_feed(namespace_id,position);
CREATE INDEX monitoring_terminal_pending_idx ON outbox(created_at,id)
 WHERE published_at IS NULL AND topic='monitoring.job_terminal.v1';
CREATE INDEX monitoring_terminal_outbox_idx ON outbox(namespace_id,created_at,id)
 WHERE published_at IS NULL AND topic='monitoring.job_terminal.v1';

-- AFTER sees the lifecycle observation filled by the existing BEFORE trigger.
-- INSERT of completed history and ordinary updates to a terminal job emit none.
CREATE FUNCTION enqueue_monitoring_terminal_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_run record; original_event_id uuid; observation_time timestamptz; recovery boolean;
BEGIN
 IF OLD.phase='terminal' OR NEW.phase<>'terminal' THEN RETURN NEW; END IF;
 original_event_id := gen_random_uuid();
 SELECT id,run_number INTO current_run FROM runs WHERE job_id=NEW.id AND namespace_id=NEW.namespace_id ORDER BY run_number DESC LIMIT 1;
 SELECT reconciliation_hold INTO recovery FROM service_recovery_state WHERE singleton;
 -- This is the actual statement recording the terminal transition. A transaction
 -- may have started before a subscription's activation boundary.
 observation_time := statement_timestamp();
 INSERT INTO outbox(id,namespace_id,topic,aggregate_type,aggregate_id,payload,created_at,available_at)
 VALUES(original_event_id,NEW.namespace_id,'monitoring.job_terminal.v1','job',NEW.id,
  jsonb_strip_nulls(jsonb_build_object(
   'eventId',original_event_id,'namespaceId',NEW.namespace_id,'jobId',NEW.id,
   'runId',current_run.id,'runNumber',current_run.run_number::text,
   'ownerPrincipalId',NEW.owner_principal_id,'oldPhase',OLD.phase,'newPhase',NEW.phase,
   'outcome',NEW.outcome,'jobRevision',NEW.revision::text,'observedCompletedAt',NEW.completed_at,
   'recordedAt',observation_time,'imported',NEW.imported,'reconciliation',COALESCE(recovery,false)
  )),observation_time,observation_time);
 RETURN NEW;
END;
$$;
CREATE TRIGGER jobs_monitoring_terminal AFTER UPDATE OF phase ON jobs
 FOR EACH ROW EXECUTE FUNCTION enqueue_monitoring_terminal_transition();

-- A separate service-only assertion has no directory actor, and cannot become
-- a user principal through an empty or synthetic actor field.
ALTER TABLE delegation_service_keys DROP CONSTRAINT delegation_service_keys_operations_check;
ALTER TABLE delegation_service_keys ADD CONSTRAINT delegation_service_keys_operations_check CHECK(cardinality(operations) BETWEEN 1 AND 8);
ALTER TABLE delegation_assertions ALTER COLUMN principal_id DROP NOT NULL;
ALTER TABLE delegation_assertions ALTER COLUMN directory_id DROP NOT NULL;
ALTER TABLE delegation_assertions ALTER COLUMN actor_issuer DROP NOT NULL;
ALTER TABLE delegation_assertions ALTER COLUMN actor_subject DROP NOT NULL;
ALTER TABLE delegation_assertions ADD CONSTRAINT delegation_assertion_identity_shape CHECK(
 (operation='events.read' AND mode='worker' AND principal_id IS NULL AND directory_id IS NULL AND actor_issuer IS NULL AND actor_subject IS NULL)
 OR (operation<>'events.read' AND principal_id IS NOT NULL AND directory_id IS NOT NULL AND actor_issuer IS NOT NULL AND actor_subject IS NOT NULL)
);
