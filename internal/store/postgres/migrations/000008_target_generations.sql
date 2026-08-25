ALTER TABLE target_generations
    ADD COLUMN provider jsonb NOT NULL DEFAULT '{"kind":"on-prem"}'::jsonb;

ALTER TABLE target_generations
    ADD CONSTRAINT target_generations_provider_object CHECK (
        jsonb_typeof(provider) = 'object'
        AND provider ? 'kind'
        AND provider->>'kind' IN ('on-prem', 'aws-parallelcluster')
    );

CREATE INDEX target_generations_provider_kind_index
    ON target_generations ((provider->>'kind'));
