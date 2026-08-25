ALTER TABLE execution_events DROP CONSTRAINT execution_events_type_known;
ALTER TABLE execution_events
    ADD CONSTRAINT execution_events_type_known CHECK (
        event_type IN (
            'process.started', 'process.completed',
            'scheduler.uncertain', 'scheduler.submitted',
            'scheduler.observed', 'scheduler.completed'
        )
    );

ALTER TABLE executions
    ADD COLUMN native_backend text,
    ADD COLUMN native_state text,
    ADD COLUMN native_reason text,
    ADD COLUMN native_cluster text,
    ADD COLUMN native_observed_at timestamptz;
ALTER TABLE executions
    ADD CONSTRAINT executions_native_backend_known CHECK (
        native_backend IS NULL OR native_backend = 'slurm'
    ),
    ADD CONSTRAINT executions_native_state_known CHECK (
        native_state IS NULL OR native_state IN (
            'uncertain', 'queued', 'running', 'suspended', 'completing', 'unknown',
            'completed', 'failed', 'cancelled', 'timed_out', 'preempted',
            'node_failed', 'out_of_memory', 'boot_failed', 'deadline', 'lost'
        )
    ),
    ADD CONSTRAINT executions_native_scheduler_consistent CHECK (
        (native_backend IS NULL AND native_state IS NULL AND native_reason IS NULL
            AND native_cluster IS NULL AND native_observed_at IS NULL)
        OR (native_backend = 'slurm' AND native_state IS NOT NULL
            AND native_observed_at IS NOT NULL)
    );
