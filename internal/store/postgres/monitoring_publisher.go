package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfigureMonitoringRetention sets the independent source-feed policy. It must
// match across replicas. It neither deletes data nor changes outbox retention.
func (store *Store) ConfigureMonitoringRetention(ctx context.Context, retention time.Duration) error {
	if retention < 24*time.Hour || retention > 365*24*time.Hour || retention%time.Second != 0 {
		return errors.New("monitoring retention must be whole seconds between one and 365 days")
	}
	_, err := store.pool.Exec(ctx, `UPDATE monitoring_feed_state SET retention_seconds=$1 WHERE singleton`, int64(retention/time.Second))
	if err != nil {
		return fmt.Errorf("configure monitoring retention: %w", err)
	}
	return nil
}

// PublishMonitoringEvents allocates positions only after taking the singleton
// lock, and commits append + outbox acknowledgement + counter together. A job
// transaction that has not committed yet gets no position and remains eligible
// for a later publication. Restored outbox records retain their original UUID.
func (store *Store) PublishMonitoringEvents(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("monitoring publication limit must be between one and 1000")
	}
	return inTransaction(ctx, store.pool, func(tx pgx.Tx) (int, error) {
		var head int64
		var last time.Time
		if err := tx.QueryRow(ctx, `SELECT head_position,last_published_at FROM monitoring_feed_state WHERE singleton FOR UPDATE`).Scan(&head, &last); err != nil {
			return 0, fmt.Errorf("lock monitoring publication: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT id::text,namespace_id::text,payload FROM outbox WHERE topic='monitoring.job_terminal.v1' AND published_at IS NULL ORDER BY created_at,id LIMIT $1 FOR UPDATE`, limit)
		if err != nil {
			return 0, fmt.Errorf("claim monitoring outbox: %w", err)
		}
		type pendingEvent struct {
			id, namespace string
			payload       []byte
		}
		pending := make([]pendingEvent, 0, limit)
		for rows.Next() {
			var event pendingEvent
			if err = rows.Scan(&event.id, &event.namespace, &event.payload); err != nil {
				rows.Close()
				return 0, err
			}
			pending = append(pending, event)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
		var publishedAt time.Time
		if timeErr := tx.QueryRow(ctx, `SELECT GREATEST(clock_timestamp(),$1::timestamptz)`, last).Scan(&publishedAt); timeErr != nil {
			return 0, timeErr
		}
		for _, event := range pending {
			// A replayed acknowledgement cannot allocate a second position for an
			// event already retained in the durable feed.
			var exists bool
			if existsErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM monitoring_feed WHERE event_id=$1)`, event.id).Scan(&exists); existsErr != nil {
				return 0, existsErr
			}
			if !exists {
				if head == int64(^uint64(0)>>1) {
					return 0, errors.New("monitoring feed position exhausted")
				}
				head++
				if _, err = tx.Exec(ctx, `INSERT INTO monitoring_feed(position,event_id,namespace_id,payload,published_at) VALUES($1,$2,$3,$4,$5)`, head, event.id, event.namespace, event.payload, publishedAt); err != nil {
					return 0, fmt.Errorf("append monitoring event: %w", err)
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE outbox SET published_at=$2 WHERE id=$1`, event.id, publishedAt); err != nil {
				return 0, err
			}
		}
		if len(pending) > 0 {
			if _, err = tx.Exec(ctx, `UPDATE monitoring_feed_state SET head_position=$1,last_published_at=$2 WHERE singleton`, head, publishedAt); err != nil {
				return 0, err
			}
		}
		return len(pending), nil
	})
}

// PruneMonitoringEvents removes only a bounded contiguous published prefix.
// Publication and pruning share the counter lock; snapshots see both the new
// retention floor and deletion, or neither. Old outbox cleanup is independent.
func (store *Store) PruneMonitoringEvents(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 10000 {
		return 0, errors.New("monitoring retention limit must be between one and 10000")
	}
	return inTransaction(ctx, store.pool, func(tx pgx.Tx) (int, error) {
		var retention int64
		if err := tx.QueryRow(ctx, `SELECT retention_seconds FROM monitoring_feed_state WHERE singleton FOR UPDATE`).Scan(&retention); err != nil {
			return 0, err
		}
		var through *int64
		err := tx.QueryRow(ctx, `SELECT max(position) FROM (SELECT position,published_at FROM monitoring_feed ORDER BY position LIMIT $2) AS prefix WHERE published_at<clock_timestamp()-($1::bigint*interval '1 second')`, retention, limit).Scan(&through)
		if err != nil {
			return 0, err
		}
		if through == nil {
			return 0, nil
		}
		deleted, err := tx.Exec(ctx, `DELETE FROM monitoring_feed WHERE position<=$1`, *through)
		if err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE monitoring_feed_state SET retired_through=GREATEST(retired_through,$1) WHERE singleton`, *through); err != nil {
			return 0, err
		}
		return int(deleted.RowsAffected()), nil
	})
}
