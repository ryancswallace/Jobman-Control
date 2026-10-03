CREATE TABLE directory_sources (
 source_id text PRIMARY KEY CHECK(length(source_id) BETWEEN 1 AND 128),
 revision bigint NOT NULL CHECK(revision>0),
 configuration_digest text NOT NULL CHECK(length(configuration_digest)=64),
 mapping jsonb NOT NULL,
 configured_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
 last_verified_at timestamptz,
 last_attempt_at timestamptz,
 last_error_code text,
 ignored_direct_members integer NOT NULL DEFAULT 0 CHECK(ignored_direct_members>=0)
);
ALTER TABLE directory_accounts ADD COLUMN source_id text REFERENCES directory_sources(source_id);
ALTER TABLE principal_aliases ADD COLUMN source_id text REFERENCES directory_sources(source_id);
CREATE INDEX directory_accounts_source_idx ON directory_accounts(source_id);
CREATE INDEX directory_aliases_source_idx ON principal_aliases(source_id);
CREATE INDEX directory_bindings_namespace_idx ON directory_role_bindings(namespace_id);
CREATE TABLE directory_audit_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 source_id text NOT NULL,
 action text NOT NULL,
 details jsonb NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);
CREATE INDEX directory_audit_retention_idx ON directory_audit_events(recorded_at);
-- Alias assignment and ordinary principal creation must never disagree. Use one
-- stable advisory lock for both writers to close concurrent provisioning races.
CREATE FUNCTION enforce_directory_alias_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended(NEW.issuer || chr(1) || NEW.subject,41));
 IF TG_TABLE_NAME='principal_aliases' THEN
  IF EXISTS(SELECT 1 FROM principals WHERE issuer=NEW.issuer AND subject=NEW.subject AND id!=NEW.principal_id) THEN
   RAISE EXCEPTION 'directory alias conflicts with a principal' USING ERRCODE='23505';
  END IF;
 ELSE
  IF EXISTS(SELECT 1 FROM principal_aliases a WHERE a.issuer=NEW.issuer AND a.subject=NEW.subject AND a.principal_id!=NEW.id
   AND (TG_OP!='INSERT' OR NOT EXISTS(SELECT 1 FROM principals p WHERE p.id=a.principal_id AND p.issuer=NEW.issuer AND p.subject=NEW.subject))) THEN
   RAISE EXCEPTION 'principal conflicts with a directory alias' USING ERRCODE='23505';
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER directory_alias_identity BEFORE INSERT OR UPDATE ON principal_aliases
 FOR EACH ROW EXECUTE FUNCTION enforce_directory_alias_identity();
CREATE TRIGGER directory_principal_identity BEFORE INSERT OR UPDATE ON principals
 FOR EACH ROW EXECUTE FUNCTION enforce_directory_alias_identity();
