-- Public certificates and derived-token references only; never private keys.
CREATE TABLE agent_recoveries (
    enrollment_id uuid PRIMARY KEY REFERENCES agent_enrollment_tokens (id) ON DELETE RESTRICT,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE RESTRICT,
    agent_id uuid NOT NULL,
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    session_id uuid NOT NULL REFERENCES agent_sessions (id) ON DELETE RESTRICT,
    certificate_serial text NOT NULL REFERENCES agent_certificates (serial) ON DELETE RESTRICT,
    certificate_pem text NOT NULL CHECK (octet_length(certificate_pem) BETWEEN 1 AND 65536),
    ca_certificate_pem text NOT NULL CHECK (octet_length(ca_certificate_pem) BETWEEN 1 AND 65536),
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    FOREIGN KEY (namespace_id, agent_id) REFERENCES agents (namespace_id, id) ON DELETE RESTRICT
);

CREATE INDEX agent_recoveries_agent_index ON agent_recoveries (agent_id);
