package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ryancswallace/jobman/diagnostic"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	diagnosticEventLimit      = 256
	diagnosticDependencyLimit = 128
)

// Metadata tokens exclude free-form scheduler reasons, paths and diagnostics.
func diagnosticToken(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func (builder *diagnosticBuilder) readRuns(ctx context.Context, tx pgx.Tx, selection diagnostic.SharedSelection) error {
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.run_number,COALESCE(e.id::text,''),r.phase,COALESCE(r.outcome,''),r.created_at,
 COALESCE(e.native_state,''),left(COALESCE(e.native_reason,''),161),e.native_observed_at,
 CASE WHEN e.process_result->>'exitCode' IS NOT NULL THEN (e.process_result->>'exitCode')::bigint ELSE NULL END,left(COALESCE(e.process_result->>'signal',''),161),
 (SELECT ev.observed_at FROM execution_events ev WHERE ev.execution_id=e.id AND (ev.event_type='process.started' OR (ev.event_type IN ('scheduler.submitted','scheduler.observed') AND ev.document #>> '{spec,scheduler,state}'='running')) ORDER BY ev.source_sequence,ev.event_id LIMIT 1),
 (SELECT ev.observed_at FROM execution_events ev WHERE ev.execution_id=e.id AND ev.event_type IN ('process.completed','scheduler.completed') ORDER BY ev.source_sequence DESC,ev.event_id DESC LIMIT 1)
 FROM runs r LEFT JOIN executions e ON e.run_id=r.id WHERE r.job_id=$1 AND (NULLIF($2,'')::uuid IS NULL OR r.id=NULLIF($2,'')::uuid) ORDER BY r.run_number DESC LIMIT 33`, selection.JobID, selection.RunID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if len(builder.snapshot.Runs) == diagnostic.SharedMaximumRuns {
			builder.omit(diagnostic.OmissionHistoryTruncated, selection.JobID)
			break
		}
		var run diagnostic.SharedRun
		var number int64
		var phase, outcome, state, reason, signal string
		var created time.Time
		var schedulerAt, started, completed *time.Time
		var exit *int64
		if err := rows.Scan(&run.ID, &number, &run.ExecutionID, &phase, &outcome, &created, &state, &reason, &schedulerAt, &exit, &signal, &started, &completed); err != nil {
			return err
		}
		if number < 1 {
			return domain.ErrConflict
		}
		run.Number = uint64(number)
		builder.snapshot.Runs = append(builder.snapshot.Runs, run)
		prefix := "run." + run.ID + "."
		builder.add(prefix+"phase", diagnostic.CodeRunPhase, run.ID, phase, nil, diagnostic.QualityObserved)
		if outcome != "" {
			builder.add(prefix+"outcome", diagnostic.CodeRunOutcome, run.ID, outcome, nil, diagnostic.QualityObserved)
		}
		builder.timestamp(prefix+"reserved_at", diagnostic.CodeRunReservedAt, run.ID, &created)
		builder.timestamp(prefix+"started_at", diagnostic.CodeRunStartedAt, run.ID, started)
		builder.timestamp(prefix+"completed_at", diagnostic.CodeRunCompletedAt, run.ID, completed)
		builder.omit("run_revision_unavailable", prefix+"revision")
		if exit != nil {
			builder.add(prefix+"exit_code", diagnostic.CodeRunExitCode, run.ID, *exit, completed, diagnostic.QualityObserved)
		}
		if signal != "" {
			if diagnosticToken(signal) {
				builder.add(prefix+"exit_signal", diagnostic.CodeRunExitSignal, run.ID, signal, completed, diagnostic.QualityObserved)
			} else {
				builder.omit("unstructured_process_signal_excluded", prefix+"exit_signal")
			}
		}
		if schedulerAt != nil && state != "" {
			if !diagnosticToken(reason) && reason != "" {
				reason = ""
				builder.omit("unstructured_scheduler_reason_excluded", prefix+"scheduler")
			}
			builder.add(prefix+"scheduler", diagnostic.CodeSharedSchedulerObservation, run.ID, diagnostic.SharedSchedulerObservation{State: state, Reason: reason, ObservedAt: schedulerAt.UTC()}, schedulerAt, diagnostic.QualityObserved)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if selection.RunID != "" && len(builder.snapshot.Runs) == 0 {
		return domain.ErrNotFound
	}
	if len(builder.snapshot.Runs) == 0 {
		builder.omit("execution_history_unavailable", selection.JobID)
	}
	return nil
}

func (builder *diagnosticBuilder) executionIDs() []string {
	ids := make([]string, 0, len(builder.snapshot.Runs))
	for _, run := range builder.snapshot.Runs {
		if run.ExecutionID != "" {
			ids = append(ids, run.ExecutionID)
		}
	}
	return ids
}

func (builder *diagnosticBuilder) readEvents(ctx context.Context, tx pgx.Tx) error {
	ids := builder.executionIDs()
	if len(ids) == 0 {
		return nil
	}
	// Each selected execution probes an index for a bounded prefix before the
	// global page is sorted; long histories cannot materialize all event payloads.
	rows, err := tx.Query(ctx, `SELECT ev.event_id::text,ev.execution_id::text,ev.event_type,ev.observed_at,ev.ingested_at,COALESCE(ev.document #>> '{spec,result,outcome}','')
 FROM unnest($1::uuid[]) selected(id) JOIN LATERAL (SELECT event_id,execution_id,event_type,observed_at,ingested_at,document FROM execution_events WHERE execution_id=selected.id ORDER BY source_sequence DESC,event_id DESC LIMIT 257) ev ON true
 ORDER BY ev.ingested_at DESC,ev.event_id DESC LIMIT 257`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for count := 0; rows.Next(); count++ {
		if count == diagnosticEventLimit {
			builder.omit(diagnostic.OmissionEventsTruncated, builder.snapshot.Job.ID)
			break
		}
		var event diagnostic.SharedLifecycleEvent
		if err := rows.Scan(&event.ID, &event.ExecutionID, &event.Type, &event.ObservedAt, &event.RecordedAt, &event.Outcome); err != nil {
			return err
		}
		event.ObservedAt = event.ObservedAt.UTC()
		event.RecordedAt = event.RecordedAt.UTC()
		for _, run := range builder.snapshot.Runs {
			if run.ExecutionID == event.ExecutionID {
				event.RunID = run.ID
				break
			}
		}
		if event.RunID == "" {
			return errors.New("diagnostic event outside selected executions")
		}
		if event.Type == "process.started" {
			event.Phase = "running"
		}
		if event.Type == "process.completed" || event.Type == "scheduler.completed" {
			event.Phase = "terminal"
		}
		builder.add("event."+event.ID, diagnostic.CodeSharedLifecycleEvent, event.ID, event, &event.ObservedAt, diagnostic.QualityObserved)
	}
	return rows.Err()
}

func (builder *diagnosticBuilder) readDependencies(ctx context.Context, tx pgx.Tx, namespaceID string) error {
	rows, err := tx.Query(ctx, `SELECT upstream.id::text,edge.predicate,COALESCE(upstream.outcome,''),`+edgeStateSQL+`,COALESCE(downstream.graph_disposition,'')
 FROM graph_edges edge JOIN jobs downstream ON downstream.id=edge.downstream_job_id AND downstream.graph_id=edge.graph_id
 JOIN jobs upstream ON upstream.id=edge.upstream_job_id AND upstream.graph_id=edge.graph_id
 WHERE downstream.id=$1 AND downstream.namespace_id=$2 AND upstream.namespace_id=$2 ORDER BY upstream.id LIMIT 129`, builder.snapshot.Job.ID, namespaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for count := 0; rows.Next(); count++ {
		if count == diagnosticDependencyLimit {
			builder.omit("dependencies_truncated", builder.snapshot.Job.ID)
			break
		}
		var value diagnostic.SharedDependencyObservation
		var state string
		if err := rows.Scan(&value.JobID, &value.Predicate, &value.ObservedOutcome, &state, &value.Disposition); err != nil {
			return err
		}
		value.Satisfied = state == "satisfied"
		id := "dependency." + value.JobID
		builder.add(id, diagnostic.CodeSharedDependencyObservation, builder.snapshot.Job.ID, value, nil, diagnostic.QualityDerivedExact)
		if value.Predicate == "outcomes" {
			builder.omit("dependency_outcome_set_not_represented", id)
		}
	}
	return rows.Err()
}

func (builder *diagnosticBuilder) readLogs(ctx context.Context, tx pgx.Tx) error {
	ids := builder.executionIDs()
	if len(ids) == 0 {
		builder.omit(diagnostic.OmissionLogsUnavailable, builder.snapshot.Job.ID)
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT s.execution_id::text,s.stream,s.manifest_revision,s.byte_length,s.state,s.truncated
 FROM log_streams s WHERE s.execution_id=ANY($1::uuid[]) ORDER BY s.execution_id,s.stream LIMIT 65`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var ref diagnostic.SharedLogReference
		var revision, length int64
		var state string
		var truncated bool
		if err := rows.Scan(&ref.ExecutionID, &ref.Stream, &revision, &length, &state, &truncated); err != nil {
			return err
		}
		for _, run := range builder.snapshot.Runs {
			if run.ExecutionID == ref.ExecutionID {
				ref.RunID = run.ID
				break
			}
		}
		if ref.RunID == "" || revision < 1 || length < 0 || (state != "capturing" && state != "complete") {
			return domain.ErrConflict
		}
		ref.ManifestRevision = uint64(revision)
		ref.Bytes = uint64(length)
		ref.Complete = state == "complete"
		digest := sha256.Sum256([]byte(builder.snapshot.Source.ControlInstanceID + "\n" + builder.snapshot.Source.NamespaceID + "\n" + builder.snapshot.Job.ID + "\n" + ref.RunID + "\n" + ref.ExecutionID + "\n" + ref.Stream))
		ref.ID = "log." + hex.EncodeToString(digest[:])
		builder.snapshot.Logs = append(builder.snapshot.Logs, ref)
		prefix := "run." + ref.RunID + "."
		code := diagnostic.CodeLogStdoutBytes
		if ref.Stream == "stderr" {
			code = diagnostic.CodeLogStderrBytes
		}
		builder.add(prefix+ref.Stream+".bytes", code, ref.RunID, ref.Bytes, nil, diagnostic.QualityObserved)
		if truncated {
			builder.omit("log_recording_truncated", ref.ID)
		}
		seen[ref.RunID] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, run := range builder.snapshot.Runs {
		builder.add(fmt.Sprintf("run.%s.logs_available", run.ID), diagnostic.CodeLogAvailable, run.ID, seen[run.ID], nil, diagnostic.QualityObserved)
		if !seen[run.ID] {
			builder.omit(diagnostic.OmissionLogsUnavailable, run.ID)
		}
	}
	return nil
}
