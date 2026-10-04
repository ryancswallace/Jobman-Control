package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// The signed cursor is a read selector, never authority. Every use also
// reauthorizes the current principal under the current service/user intersection.
type runPageCursor struct {
	NamespaceID string    `json:"namespaceId"`
	JobID       string    `json:"jobId"`
	PrincipalID string    `json:"principalId"`
	InstanceID  string    `json:"instanceId"`
	Epoch       int64     `json:"epoch"`
	Grant       int64     `json:"grant"`
	Ceiling     int64     `json:"ceiling"`
	Before      int64     `json:"before"`
	Expires     time.Time `json:"expires"`
}

const runSelect = `SELECT r.id::text,r.run_number,r.phase,r.desired_state,COALESCE(r.outcome,''),r.created_at,r.updated_at,
 COALESCE(e.id::text,''),COALESCE(e.phase,''),COALESCE(e.target_id::text,''),COALESCE(e.target_generation_id::text,''),COALESCE(tg.execution_backend,''),COALESCE(e.observation_confidence,'')
 FROM runs r LEFT JOIN executions e ON e.run_id=r.id AND e.namespace_id=r.namespace_id
 LEFT JOIN target_generations tg ON tg.id=e.target_generation_id AND tg.namespace_id=e.namespace_id `

func scanJobRun(row pgx.Row) (domain.JobRun, error) {
	var r domain.JobRun
	err := row.Scan(&r.ID, &r.Number, &r.Phase, &r.DesiredState, &r.Outcome, &r.CreatedAt, &r.UpdatedAt, &r.ExecutionID, &r.ExecutionPhase, &r.TargetID, &r.TargetGenerationID, &r.Backend, &r.Confidence)
	r.CreatedAt = r.CreatedAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()
	return r, err
}

const runCursorDomain = "jobman.control.run-cursor/v1"

func (store *Store) runCursorMAC(raw []byte) []byte {
	mac := hmac.New(sha256.New, store.tokenKey)
	_, _ = mac.Write([]byte(runCursorDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(raw)
	return mac.Sum(nil)
}

func (store *Store) encodeRunCursor(cursor runPageCursor) string {
	if len(store.tokenKey) < 32 {
		return ""
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(store.runCursorMAC(raw))
}

func (store *Store) decodeRunCursor(value string) (runPageCursor, error) {
	var c runPageCursor
	if value == "" || len(value) > 1024 || len(store.tokenKey) < 32 {
		return c, domain.ErrConflict
	}
	payload, signature, ok := strings.Cut(value, ".")
	if !ok || len(signature) != 43 {
		return c, domain.ErrConflict
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return c, domain.ErrConflict
	}
	signatureBytes, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(signatureBytes, store.runCursorMAC(raw)) {
		return c, domain.ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || !domain.IsID(c.NamespaceID) || !domain.IsID(c.JobID) || !domain.IsID(c.PrincipalID) || !domain.IsID(c.InstanceID) || c.Ceiling < 1 || c.Before < 1 || c.Before > c.Ceiling || c.Epoch < 1 || c.Grant < 1 || c.Expires.IsZero() || store.encodeRunCursor(c) != value {
		return c, domain.ErrConflict
	}
	return c, nil
}

// ListRuns uses the existing (job_id,run_number) index. The first page captures
// a run-number ceiling; newly created runs appear after an explicit restart.
func (store *Store) ListRuns(ctx context.Context, p domain.Principal, namespace, jobID string, q domain.RunListOptions) (domain.RunPage, error) {
	if !domain.IsID(jobID) || q.Limit < 1 || q.Limit > 100 || len(q.PageToken) > 1024 {
		return domain.RunPage{}, errors.New("run query is invalid")
	}
	if len(store.tokenKey) < 32 {
		return domain.RunPage{}, domain.ErrAuthorizationUnavailable
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.RunPage, error) {
		out := domain.RunPage{Items: []domain.JobRun{}}
		auth, err := authorizeNamespace(ctx, tx, p, namespace, domain.CapabilityJobsRead)
		if err != nil {
			return out, err
		}
		out.ManifestAuthority, err = readManifestAuthority(ctx, tx, auth, jobID)
		if err != nil {
			return out, err
		}
		var instance string
		if queryErr := tx.QueryRow(ctx, `SELECT id::text FROM control_instance WHERE singleton`).Scan(&instance); queryErr != nil {
			return out, queryErr
		}
		c := runPageCursor{NamespaceID: auth.namespaceID, JobID: jobID, PrincipalID: auth.principalID, InstanceID: instance, Epoch: out.RecoveryEpoch, Grant: out.AuthorizationVersion, Expires: out.AsOf.Add(10 * time.Minute)}
		if q.PageToken != "" {
			saved, e := store.decodeRunCursor(q.PageToken)
			if e != nil {
				return out, e
			}
			if saved.NamespaceID != c.NamespaceID || saved.JobID != c.JobID || saved.PrincipalID != c.PrincipalID || saved.InstanceID != c.InstanceID || saved.Epoch != c.Epoch || saved.Grant != c.Grant || !out.AsOf.Before(saved.Expires) || saved.Expires.After(out.AsOf.Add(10*time.Minute)) {
				return out, domain.ErrConflict
			}
			c = saved
		} else if queryErr := tx.QueryRow(ctx, `SELECT COALESCE(max(run_number),0) FROM runs WHERE namespace_id=$1 AND job_id=$2`, auth.namespaceID, jobID).Scan(&c.Ceiling); queryErr != nil {
			return out, queryErr
		}
		if queryErr := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1 AND job_id=$2 AND run_number<=$3`, auth.namespaceID, jobID, c.Ceiling).Scan(&out.Total); queryErr != nil {
			return out, queryErr
		}
		rows, err := tx.Query(ctx, runSelect+` WHERE r.namespace_id=$1 AND r.job_id=$2 AND r.run_number<=$3 AND ($4::bigint=0 OR r.run_number<$4) ORDER BY r.run_number DESC LIMIT $5`, auth.namespaceID, jobID, c.Ceiling, c.Before, q.Limit+1)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		for rows.Next() {
			r, e := scanJobRun(rows)
			if e != nil {
				return out, e
			}
			out.Items = append(out.Items, r)
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		if len(out.Items) > q.Limit {
			out.Items = out.Items[:q.Limit]
			c.Before = out.Items[len(out.Items)-1].Number
			out.NextPageToken = store.encodeRunCursor(c)
			if out.NextPageToken == "" {
				return out, domain.ErrAuthorizationUnavailable
			}
		}
		return out, nil
	})
}

// GetRun reads only the selected job’s run under current namespace authority.
func (store *Store) GetRun(ctx context.Context, p domain.Principal, namespace, jobID, runID string) (domain.RunDetail, error) {
	if !domain.IsID(jobID) || !domain.IsID(runID) {
		return domain.RunDetail{}, domain.ErrNotFound
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.RunDetail, error) {
		var out domain.RunDetail
		auth, err := authorizeNamespace(ctx, tx, p, namespace, domain.CapabilityJobsRead)
		if err != nil {
			return out, err
		}
		out.ManifestAuthority, err = readManifestAuthority(ctx, tx, auth, jobID)
		if err != nil {
			return out, err
		}
		out.Run, err = scanJobRun(tx.QueryRow(ctx, runSelect+` WHERE r.namespace_id=$1 AND r.job_id=$2 AND r.id=$3`, auth.namespaceID, jobID, runID))
		if errors.Is(err, pgx.ErrNoRows) {
			return out, domain.ErrNotFound
		}
		return out, err
	})
}
