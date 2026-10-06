package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	maximumJobExecutionBytes       = 2 * 1024 * 1024
	maximumJobCommandArgs          = 4096
	maximumJobExecutionStringBytes = 64 * 1024
)

// readJobExecution is called only inside GetJob's authorized read transaction.
// Project only command and working directory, and bound bytes before transfer;
// never read the environment or the entire submitted workload into the service.
func readJobExecution(ctx context.Context, tx pgx.Tx, namespaceID, jobID, digest string) (*domain.JobExecution, string, error) {
	var raw []byte
	var size int
	err := tx.QueryRow(ctx, `
 SELECT CASE WHEN octet_length(projected::text) <= $4 THEN projected::text ELSE NULL END,
        octet_length(projected::text)
 FROM (
   SELECT jsonb_build_object('command', w.document #> '{spec,command}',
                            'workingDirectory', w.document #> '{spec,workingDirectory}') AS projected
   FROM jobs j JOIN workload_revisions w ON w.namespace_id=j.namespace_id AND w.digest=j.workload_digest
   WHERE j.namespace_id=$1 AND j.id=$2 AND j.workload_digest=$3
 ) AS execution_projection
 `, namespaceID, jobID, digest, maximumJobExecutionBytes).Scan(&raw, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "missing", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read job execution metadata: %w", err)
	}
	if size > maximumJobExecutionBytes {
		return nil, "too_large", nil
	}
	execution, reason := decodeJobExecution(raw)
	return execution, reason, nil
}

func decodeJobExecution(raw []byte) (result *domain.JobExecution, unavailableReason string) {
	if len(raw) > maximumJobExecutionBytes {
		return nil, "too_large"
	}
	var projected struct {
		Command *struct {
			Executable string          `json:"executable"`
			Args       json.RawMessage `json:"args"`
		} `json:"command"`
		WorkingDirectory *string `json:"workingDirectory"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&projected); err != nil {
		return nil, "unsupported"
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, "unsupported"
	}
	if projected.Command == nil || projected.WorkingDirectory == nil {
		return nil, "missing"
	}
	command, directory := projected.Command, *projected.WorkingDirectory
	if command.Executable == "" || directory == "" || strings.ContainsRune(command.Executable, 0) || strings.ContainsRune(directory, 0) {
		return nil, "unsupported"
	}
	if len(command.Executable) > maximumJobExecutionStringBytes || len(directory) > maximumJobExecutionStringBytes {
		return nil, "too_large"
	}
	// Canonical workloads omit args when empty. Explicit null arrays or array
	// elements are invalid; decoding into []string would silently turn a null
	// element into an empty argument and lose the original invocation's shape.
	var argumentValues []*string
	if len(command.Args) != 0 {
		if bytes.Equal(bytes.TrimSpace(command.Args), []byte("null")) || json.Unmarshal(command.Args, &argumentValues) != nil {
			return nil, "unsupported"
		}
	}
	if len(argumentValues) > maximumJobCommandArgs {
		return nil, "too_large"
	}
	args := make([]string, 0, len(argumentValues))
	for _, arg := range argumentValues {
		if arg == nil || strings.ContainsRune(*arg, 0) {
			return nil, "unsupported"
		}
		if len(*arg) > maximumJobExecutionStringBytes {
			return nil, "too_large"
		}
		args = append(args, *arg)
	}
	execution := &domain.JobExecution{Command: domain.JobCommand{Executable: command.Executable, Args: args}, WorkingDirectory: directory}
	encoded, err := json.Marshal(execution)
	if err != nil {
		return nil, "unsupported"
	}
	if len(encoded) > maximumJobExecutionBytes {
		return nil, "too_large"
	}
	return execution, ""
}
