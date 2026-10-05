-- Binding edits can change effective access without changing a retained grant.
-- Advance the same per-principal fence consumed by delegated authorization.
CREATE FUNCTION bump_directory_binding_versions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND (OLD.group_id,OLD.namespace_id,OLD.role,OLD.enabled)
   IS NOT DISTINCT FROM (NEW.group_id,NEW.namespace_id,NEW.role,NEW.enabled) THEN
  RETURN NEW;
 END IF;
 UPDATE authorization_versions AS version SET revision=version.revision+1,updated_at=clock_timestamp()
 WHERE EXISTS(SELECT 1 FROM membership_grants AS grant_row
  WHERE grant_row.namespace_id=version.namespace_id AND grant_row.principal_id=version.principal_id
   AND grant_row.provenance='directory' AND grant_row.revoked_at IS NULL AND (
    (TG_OP<>'INSERT' AND grant_row.namespace_id=OLD.namespace_id AND grant_row.source_key=OLD.group_id::text AND grant_row.role=OLD.role)
    OR (TG_OP<>'DELETE' AND grant_row.namespace_id=NEW.namespace_id AND grant_row.source_key=NEW.group_id::text AND grant_row.role=NEW.role)
   ));
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER directory_binding_versions AFTER INSERT OR UPDATE OR DELETE ON directory_role_bindings
 FOR EACH ROW EXECUTE FUNCTION bump_directory_binding_versions();

-- Serialize creation timestamps through a row held until the inserting
-- transaction commits. A later visible target can never have a lower stamp.
-- Seed existing rows without rewriting their public identities or timestamps.
LOCK TABLE targets IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE target_catalog_clocks (
 namespace_id uuid PRIMARY KEY REFERENCES namespaces(id) ON DELETE RESTRICT,
 last_created_at timestamptz NOT NULL
);
INSERT INTO target_catalog_clocks(namespace_id,last_created_at)
 SELECT namespace_id,max(created_at) FROM targets GROUP BY namespace_id;
CREATE FUNCTION stamp_target_catalog_creation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF OLD.created_at IS DISTINCT FROM NEW.created_at OR OLD.namespace_id IS DISTINCT FROM NEW.namespace_id THEN
   RAISE EXCEPTION 'target catalog identity is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 INSERT INTO target_catalog_clocks(namespace_id,last_created_at)
 VALUES(NEW.namespace_id,clock_timestamp())
 ON CONFLICT(namespace_id) DO UPDATE
 SET last_created_at=GREATEST(target_catalog_clocks.last_created_at+interval '1 microsecond',clock_timestamp())
 RETURNING last_created_at INTO NEW.created_at;
 NEW.updated_at := GREATEST(NEW.updated_at,NEW.created_at);
 RETURN NEW;
END;
$$;
CREATE TRIGGER target_catalog_creation BEFORE INSERT OR UPDATE OF created_at,namespace_id ON targets
 FOR EACH ROW EXECUTE FUNCTION stamp_target_catalog_creation();

CREATE OR REPLACE FUNCTION enqueue_monitoring_terminal_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_run record; original_event_id uuid; observation_time timestamptz; recovery boolean;
BEGIN
 IF OLD.phase='terminal' OR NEW.phase<>'terminal' THEN RETURN NEW; END IF;
 original_event_id := gen_random_uuid();
 SELECT id,run_number INTO current_run FROM runs WHERE job_id=NEW.id AND namespace_id=NEW.namespace_id ORDER BY run_number DESC LIMIT 1;
 SELECT reconciliation_hold INTO recovery FROM service_recovery_state WHERE singleton;
 -- Record when the AFTER trigger observes the transition, after any row-lock
 -- wait. Statement and transaction start times may precede activation.
 observation_time := clock_timestamp();
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
