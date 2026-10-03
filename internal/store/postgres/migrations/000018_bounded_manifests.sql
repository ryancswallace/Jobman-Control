-- Existing immutable streams receive an explicit revision baseline, not a
-- fabricated reconstruction of their historical change count.
ALTER TABLE log_streams ADD COLUMN manifest_revision bigint NOT NULL DEFAULT 1 CHECK(manifest_revision>0);
CREATE FUNCTION bump_log_manifest_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.manifest_revision:=OLD.manifest_revision+1;
 RETURN NEW;
END;
$$;
CREATE TRIGGER log_manifest_revision BEFORE UPDATE ON log_streams
 FOR EACH ROW EXECUTE FUNCTION bump_log_manifest_revision();
CREATE INDEX log_chunks_offset_seek_idx ON log_chunks(execution_id,stream,byte_offset DESC,sequence DESC);

-- Portable cursor ordering cannot depend on the database's configured locale.
CREATE INDEX execution_artifacts_cursor_idx ON execution_artifacts(execution_id,name COLLATE "C");
