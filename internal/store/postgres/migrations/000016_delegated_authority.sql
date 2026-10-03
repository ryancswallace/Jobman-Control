-- Public service registrations are operator-owned. Private signing keys never
-- enter Control. Existing unmanaged namespaces retain their legacy semantics.
CREATE TABLE delegation_service_keys (
 service_id text NOT NULL CHECK (length(service_id) BETWEEN 1 AND 128),
 key_id text NOT NULL CHECK (length(key_id) BETWEEN 1 AND 128),
 audience text NOT NULL CHECK (length(audience) BETWEEN 1 AND 512),
 public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
 certificate_thumbprints text[] NOT NULL CHECK (cardinality(certificate_thumbprints) BETWEEN 1 AND 8),
 namespace_ids uuid[] NOT NULL CHECK (cardinality(namespace_ids) BETWEEN 1 AND 320),
 operations text[] NOT NULL CHECK (cardinality(operations) BETWEEN 1 AND 7),
 enabled boolean NOT NULL DEFAULT true,
 updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
 PRIMARY KEY(service_id,key_id)
);
CREATE TABLE directory_accounts (
 directory_id uuid PRIMARY KEY,
 principal_id uuid NOT NULL UNIQUE REFERENCES principals(id) ON DELETE RESTRICT,
 enabled boolean NOT NULL DEFAULT false,
 last_verified_at timestamptz,
 UNIQUE(directory_id,principal_id)
);
CREATE TABLE principal_aliases (
 issuer text NOT NULL CHECK(length(issuer) BETWEEN 1 AND 512),
 subject text NOT NULL CHECK(length(subject) BETWEEN 1 AND 512),
 directory_id uuid NOT NULL,
 principal_id uuid NOT NULL,
 provenance text NOT NULL CHECK(provenance IN ('operator','verified-claim')),
 verified_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
 PRIMARY KEY(issuer,subject),
 FOREIGN KEY(directory_id,principal_id) REFERENCES directory_accounts(directory_id,principal_id) ON DELETE RESTRICT
);
CREATE TABLE namespace_directory_state (
 namespace_id uuid PRIMARY KEY REFERENCES namespaces(id) ON DELETE RESTRICT,
 source_id text NOT NULL CHECK(length(source_id) BETWEEN 1 AND 128),
 last_verified_at timestamptz,
 last_attempt_at timestamptz,
 last_error_code text,
 configuration_revision bigint NOT NULL DEFAULT 1 CHECK(configuration_revision>0)
);
CREATE TABLE directory_role_bindings (
 group_id uuid PRIMARY KEY,
 namespace_id uuid NOT NULL REFERENCES namespaces(id) ON DELETE RESTRICT,
 role text NOT NULL CHECK(role IN ('viewer','submitter','operator','namespace_admin')),
 enabled boolean NOT NULL DEFAULT true,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);
CREATE TABLE delegation_assertions (
 service_id text NOT NULL,
 assertion_id text NOT NULL CHECK(length(assertion_id) BETWEEN 22 AND 86),
 key_id text NOT NULL,
 principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
 directory_id uuid NOT NULL REFERENCES directory_accounts(directory_id) ON DELETE RESTRICT,
 actor_issuer text NOT NULL,
 actor_subject text NOT NULL,
 operation text NOT NULL,
 namespace_ids uuid[] NOT NULL,
 mode text NOT NULL CHECK(mode IN ('interactive','worker')),
 assertion_digest text NOT NULL,
 issued_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 accepted_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
 PRIMARY KEY(service_id,assertion_id),
 FOREIGN KEY(service_id,key_id) REFERENCES delegation_service_keys(service_id,key_id) ON DELETE RESTRICT
);
CREATE INDEX delegation_assertions_retention_idx ON delegation_assertions(accepted_at);
-- Managed namespaces accept only current contributions from their configured
-- directory bindings. Retained legacy/manual rows cannot silently bypass AD.
CREATE OR REPLACE VIEW authorized_memberships AS
 SELECT contribution.namespace_id,contribution.principal_id,array_agg(DISTINCT contribution.role ORDER BY contribution.role) AS roles
 FROM membership_grants AS contribution
 LEFT JOIN namespace_directory_state AS managed ON managed.namespace_id=contribution.namespace_id
 WHERE contribution.revoked_at IS NULL AND (
  managed.namespace_id IS NULL OR (
   contribution.provenance='directory'
   AND EXISTS(SELECT 1 FROM directory_accounts AS account WHERE account.principal_id=contribution.principal_id AND account.enabled)
   AND EXISTS(SELECT 1 FROM directory_role_bindings AS binding WHERE binding.group_id::text=contribution.source_key AND binding.namespace_id=contribution.namespace_id AND binding.role=contribution.role AND binding.enabled)
  )
 ) GROUP BY contribution.namespace_id,contribution.principal_id;
CREATE FUNCTION bump_directory_account_versions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.enabled IS DISTINCT FROM NEW.enabled THEN
  UPDATE authorization_versions SET revision=revision+1,updated_at=transaction_timestamp()
  WHERE principal_id=NEW.principal_id AND namespace_id IN(SELECT namespace_id FROM namespace_directory_state);
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER directory_account_versions AFTER UPDATE ON directory_accounts
 FOR EACH ROW EXECUTE FUNCTION bump_directory_account_versions();
CREATE FUNCTION bump_directory_namespace_versions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE authorization_versions SET revision=revision+1,updated_at=transaction_timestamp() WHERE namespace_id=NEW.namespace_id;
 RETURN NEW;
END;
$$;
CREATE TRIGGER directory_namespace_versions AFTER INSERT ON namespace_directory_state
 FOR EACH ROW EXECUTE FUNCTION bump_directory_namespace_versions();
-- Restored grants must be independently verified again before being served.
ALTER TABLE service_recovery_state ADD COLUMN delegation_issued_after timestamptz NOT NULL DEFAULT 'epoch';
CREATE FUNCTION invalidate_directory_proof_after_restore() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.restore_epoch IS DISTINCT FROM NEW.restore_epoch THEN
  -- Previously consumed assertions may be absent from a restored replay ledger.
  -- Include the maximum permitted future clock skew in the issuance floor.
  NEW.delegation_issued_after := statement_timestamp()+interval '5 seconds';
  UPDATE directory_accounts SET last_verified_at=NULL;
  UPDATE namespace_directory_state SET last_verified_at=NULL,last_error_code='restore_reverification_required';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER directory_restore_proof BEFORE UPDATE OF restore_epoch ON service_recovery_state
 FOR EACH ROW EXECUTE FUNCTION invalidate_directory_proof_after_restore();
