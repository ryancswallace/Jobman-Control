-- Existing memberships remain the legacy administrative write surface. All
-- authorization reads use the contributing grants, including this mirrored
-- legacy contribution. An old binary rejects this newer migration ledger.
CREATE TABLE membership_grants (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    role text NOT NULL CHECK (role IN ('viewer', 'submitter', 'operator', 'namespace_admin')),
    provenance text NOT NULL CHECK (provenance IN ('legacy', 'manual', 'directory')),
    source_key text NOT NULL CHECK (length(source_key) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    revoked_at timestamptz,
    UNIQUE (namespace_id, principal_id, provenance, source_key)
);
CREATE INDEX membership_grants_principal_index
    ON membership_grants (principal_id, namespace_id) WHERE revoked_at IS NULL;
CREATE TABLE authorization_versions (
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (namespace_id, principal_id)
);
CREATE FUNCTION bump_authorization_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.role = NEW.role AND OLD.revoked_at IS NOT DISTINCT FROM NEW.revoked_at THEN
        RETURN NEW;
    END IF;
    INSERT INTO authorization_versions (namespace_id, principal_id, revision)
    VALUES (NEW.namespace_id, NEW.principal_id, 1)
    ON CONFLICT (namespace_id, principal_id) DO UPDATE
    SET revision = authorization_versions.revision + 1, updated_at = transaction_timestamp();
    RETURN NEW;
END;
$$;
CREATE TRIGGER membership_grants_version AFTER INSERT OR UPDATE ON membership_grants
    FOR EACH ROW EXECUTE FUNCTION bump_authorization_version();
INSERT INTO membership_grants (id, namespace_id, principal_id, role, provenance, source_key, created_at, updated_at)
SELECT md5(namespace_id::text || ':' || principal_id::text || ':legacy')::uuid,
    namespace_id, principal_id, role, 'legacy', 'legacy', created_at, updated_at
FROM memberships;
INSERT INTO audit_events (namespace_id, actor_principal_id, action, resource_type, resource_id, details)
SELECT namespace_id, principal_id, 'membership.legacy.migrated', 'principal', principal_id,
    jsonb_build_object('role', role, 'provenance', 'legacy') FROM memberships;
CREATE FUNCTION mirror_legacy_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        UPDATE membership_grants SET revoked_at = transaction_timestamp(), updated_at = transaction_timestamp()
        WHERE namespace_id = OLD.namespace_id AND principal_id = OLD.principal_id AND provenance = 'legacy';
        RETURN OLD;
    END IF;
    INSERT INTO membership_grants (id, namespace_id, principal_id, role, provenance, source_key, created_at, updated_at)
    VALUES (md5(NEW.namespace_id::text || ':' || NEW.principal_id::text || ':legacy')::uuid,
        NEW.namespace_id, NEW.principal_id, NEW.role, 'legacy', 'legacy', NEW.created_at, NEW.updated_at)
    ON CONFLICT (namespace_id, principal_id, provenance, source_key) DO UPDATE
    SET role = EXCLUDED.role, revoked_at = NULL, updated_at = EXCLUDED.updated_at;
    RETURN NEW;
END;
$$;
CREATE TRIGGER memberships_grants AFTER INSERT OR UPDATE OR DELETE ON memberships
    FOR EACH ROW EXECUTE FUNCTION mirror_legacy_membership();
CREATE VIEW authorized_memberships AS
SELECT namespace_id, principal_id, array_agg(DISTINCT role ORDER BY role) AS roles
FROM membership_grants WHERE revoked_at IS NULL GROUP BY namespace_id, principal_id;
