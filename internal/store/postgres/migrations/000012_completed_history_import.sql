ALTER TABLE jobs
    ADD COLUMN imported boolean NOT NULL DEFAULT false,
    ADD COLUMN import_source jsonb,
    ADD COLUMN completed_at timestamptz;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_import_source_object CHECK (
        import_source IS NULL OR jsonb_typeof(import_source) = 'object'
    ),
    ADD CONSTRAINT jobs_import_consistent CHECK (
        (imported = false AND import_source IS NULL)
        OR (imported = true AND import_source IS NOT NULL
            AND phase = 'terminal' AND outcome IS NOT NULL AND completed_at IS NOT NULL)
    ),
    ADD CONSTRAINT jobs_completion_consistent CHECK (
        completed_at IS NULL OR phase = 'terminal'
    );

CREATE UNIQUE INDEX jobs_import_source_unique
    ON jobs (namespace_id, (import_source->>'store'), (import_source->>'jobId'))
    WHERE imported;
