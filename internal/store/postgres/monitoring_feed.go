package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type monitoringCursor struct {
	Instance string `json:"instance"`
	Epoch    int64  `json:"epoch,string"`
	Scope    string `json:"scope"`
	Position int64  `json:"position,string"`
}

const monitoringCursorDomain = "jobman.control.monitoring-cursor/v1"

func monitoringScope(principal domain.Principal) string {
	ids := slices.Clone(principal.Delegation.NamespaceIDs)
	slices.Sort(ids)
	digest := sha256.Sum256([]byte(principal.Delegation.ServiceID + "\x00" + strings.Join(ids, "\x00")))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (store *Store) monitoringCursorMAC(data []byte) []byte {
	mac := hmac.New(sha256.New, store.tokenKey)
	_, _ = mac.Write([]byte(monitoringCursorDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func (store *Store) encodeMonitoringCursor(cursor monitoringCursor) string {
	if len(store.tokenKey) < 32 {
		return ""
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(store.monitoringCursorMAC(data))
}

func (store *Store) decodeMonitoringCursor(value string) (monitoringCursor, error) {
	var cursor monitoringCursor
	if value == "" || len(value) > 1024 || len(store.tokenKey) < 32 {
		return cursor, domain.ErrEventCursorInvalid
	}
	payload, signature, ok := strings.Cut(value, ".")
	if !ok || len(signature) != 43 {
		return cursor, domain.ErrEventCursorInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return cursor, domain.ErrEventCursorInvalid
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(mac, store.monitoringCursorMAC(raw)) {
		return cursor, domain.ErrEventCursorInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cursor); err != nil {
		return cursor, domain.ErrEventCursorInvalid
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) || !domain.IsID(cursor.Instance) || cursor.Epoch < 1 || cursor.Position < 0 || len(cursor.Scope) != 43 || store.encodeMonitoringCursor(cursor) != value {
		return cursor, domain.ErrEventCursorInvalid
	}
	return cursor, nil
}

func (store *Store) readMonitoringCheckpoint(ctx context.Context, tx pgx.Tx, principal domain.Principal) (result domain.MonitoringCheckpoint, head, floor int64, resultErr error) {
	if len(store.tokenKey) < 32 {
		return result, 0, 0, domain.ErrAuthorizationUnavailable
	}
	if err := authorizeMonitoringService(ctx, tx, principal); err != nil {
		return result, 0, 0, err
	}
	err := tx.QueryRow(ctx, `SELECT identity.id::text,recovery.restore_epoch,transaction_timestamp(),state.head_position,state.retired_through,state.retention_seconds,
 (SELECT count(*) FROM outbox WHERE topic='monitoring.job_terminal.v1' AND published_at IS NULL AND namespace_id=ANY($1::uuid[])),
 (SELECT min(created_at) FROM outbox WHERE topic='monitoring.job_terminal.v1' AND published_at IS NULL AND namespace_id=ANY($1::uuid[]))
 FROM monitoring_feed_state AS state CROSS JOIN control_instance AS identity CROSS JOIN service_recovery_state AS recovery
 WHERE state.singleton AND identity.singleton AND recovery.singleton`, principal.Delegation.NamespaceIDs).Scan(&result.ControlInstanceID, &result.RecoveryEpoch, &result.AsOf, &head, &floor, &result.RetentionSeconds, &result.BacklogCount, &result.OldestUnpublishedRecordedAt)
	if err != nil {
		return result, 0, 0, fmt.Errorf("read monitoring checkpoint: %w", err)
	}
	cursor := monitoringCursor{Instance: result.ControlInstanceID, Epoch: result.RecoveryEpoch, Scope: monitoringScope(principal), Position: head}
	result.HeadCursor = store.encodeMonitoringCursor(cursor)
	cursor.Position = floor
	result.OldestCursor = store.encodeMonitoringCursor(cursor)
	return result, head, floor, nil
}

// MonitoringCheckpoint captures the committed feed head and database source
// clock in the same authorized snapshot. Unpublished backlog is separate.
func (store *Store) MonitoringCheckpoint(ctx context.Context, principal domain.Principal) (domain.MonitoringCheckpoint, error) {
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.MonitoringCheckpoint, error) {
		result, _, _, err := store.readMonitoringCheckpoint(ctx, tx, principal)
		return result, err
	})
}

// ReadMonitoringEvents seeks separately in each authorized namespace index.
// Even an empty scope page advances to the committed global head. No traversal
// of other namespaces or their payloads is needed to make progress.
func (store *Store) ReadMonitoringEvents(ctx context.Context, principal domain.Principal, value string, limit int) (domain.MonitoringEventPage, error) {
	if limit < 1 || limit > 200 {
		return domain.MonitoringEventPage{}, domain.ErrEventCursorInvalid
	}
	cursor, err := store.decodeMonitoringCursor(value)
	if err != nil {
		return domain.MonitoringEventPage{}, err
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.MonitoringEventPage, error) {
		checkpoint, head, floor, readErr := store.readMonitoringCheckpoint(ctx, tx, principal)
		if readErr != nil {
			return domain.MonitoringEventPage{}, readErr
		}
		result := domain.MonitoringEventPage{MonitoringCheckpoint: checkpoint, Items: make([]domain.MonitoringEvent, 0, limit)}
		if cursor.Instance != checkpoint.ControlInstanceID || cursor.Epoch != checkpoint.RecoveryEpoch {
			return result, domain.ErrEventRecoveryChanged
		}
		if cursor.Scope != monitoringScope(principal) {
			return result, domain.ErrEventScopeChanged
		}
		if cursor.Position < floor {
			return result, domain.ErrEventCursorExpired
		}
		if cursor.Position > head {
			return result, domain.ErrEventCursorInvalid
		}
		rows, queryErr := tx.Query(ctx, `WITH selected_positions AS MATERIALIZED (
 SELECT selected.position FROM unnest($1::uuid[]) AS scope(namespace_id)
 CROSS JOIN LATERAL(SELECT position FROM monitoring_feed WHERE namespace_id=scope.namespace_id AND position>$2 AND position<=$3 ORDER BY position LIMIT $4) AS selected
 ORDER BY selected.position LIMIT $4)
 SELECT feed.position,feed.payload FROM selected_positions JOIN monitoring_feed AS feed USING(position) ORDER BY feed.position`, principal.Delegation.NamespaceIDs, cursor.Position, head, limit+1)
		if queryErr != nil {
			return result, fmt.Errorf("read monitoring events: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			var event domain.MonitoringEvent
			var payload []byte
			if scanErr := rows.Scan(&event.Position, &payload); scanErr != nil {
				return result, scanErr
			}
			if len(result.Items) == limit {
				result.HasMore = true
				break
			}
			if decodeErr := json.Unmarshal(payload, &event); decodeErr != nil {
				return result, fmt.Errorf("decode monitoring event: %w", decodeErr)
			}
			result.Items = append(result.Items, event)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return result, rowsErr
		}
		cursor.Position = head
		if result.HasMore {
			cursor.Position = result.Items[len(result.Items)-1].Position
		}
		result.NextCursor = store.encodeMonitoringCursor(cursor)
		return result, nil
	})
}
