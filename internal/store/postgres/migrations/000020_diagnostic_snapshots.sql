-- Bound per-execution lifecycle history probes before cross-run ordering.
CREATE INDEX execution_events_diagnostic_idx ON execution_events(execution_id,source_sequence DESC,event_id DESC);
