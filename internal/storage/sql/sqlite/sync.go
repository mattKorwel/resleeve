package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

type syncStore struct {
	db *sql.DB
}

// Sync exposes the SyncStore implementation.
func (s *Store) Sync() rsql.SyncStore { return s.sync }

// outboxTimeLayout is a fixed-width RFC 3339 layout with full
// nanosecond precision (always 9 fractional digits). The outbox
// dequeue uses lexicographic comparison on next_attempt_at, so the
// stored timestamp MUST have a width-stable representation. The
// stdlib time.RFC3339Nano layout strips trailing zeros (e.g.
// "...12.93422Z" vs "...12.934221Z") which makes the lex order
// disagree with the time order at sub-microsecond boundaries — the
// flake hit by TestSyncClient_SealedMemoryBlobsAreOpaqueOnUpstream
// (and a few cousin tests) under -count=N: an enqueued row whose
// formatted timestamp lexically exceeds the dequeue probe's
// timestamp gets filtered out as "still in the future" even though
// it's strictly in the past. Padded layout (".000000000Z") preserves
// monotonicity.
const outboxTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatOutboxTime(t time.Time) string {
	return t.UTC().Format(outboxTimeLayout)
}

func (s *syncStore) EnqueueOutbox(ctx context.Context, kind, key string, blob []byte, nextAttemptAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO outbox (kind, key, blob, attempts, next_attempt_at)
		VALUES (?, ?, ?, 0, ?)`,
		kind, key, blob, formatOutboxTime(nextAttemptAt),
	)
	if err != nil {
		return fmt.Errorf("outbox enqueue: %w", err)
	}
	return nil
}

func (s *syncStore) DequeueOutbox(ctx context.Context, batchSize int) ([]rsql.OutboxRow, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, kind, key, blob, attempts, next_attempt_at, COALESCE(last_error, '')
		FROM outbox
		WHERE next_attempt_at <= ?
		ORDER BY seq ASC
		LIMIT ?`,
		formatOutboxTime(time.Now()), batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox dequeue: %w", err)
	}
	defer rows.Close()
	var out []rsql.OutboxRow
	for rows.Next() {
		var r rsql.OutboxRow
		var nextStr string
		if err := rows.Scan(&r.Seq, &r.Kind, &r.Key, &r.Blob, &r.Attempts, &nextStr, &r.LastError); err != nil {
			return nil, err
		}
		// Accept either the new fixed-width layout or the legacy
		// time.RFC3339Nano so any rows written by older daemon binaries
		// still parse cleanly.
		t, err := time.Parse(outboxTimeLayout, nextStr)
		if err != nil {
			t, _ = time.Parse(time.RFC3339Nano, nextStr)
		}
		r.NextAttemptAt = t
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *syncStore) AckOutbox(ctx context.Context, seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	placeholders := make([]string, len(seqs))
	args := make([]any, len(seqs))
	for i, seq := range seqs {
		placeholders[i] = "?"
		args[i] = seq
	}
	q := fmt.Sprintf(`DELETE FROM outbox WHERE seq IN (%s)`, strings.Join(placeholders, ","))
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("outbox ack: %w", err)
	}
	return nil
}

func (s *syncStore) BumpOutboxAttempt(ctx context.Context, seq int64, nextAttemptAt time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox
		SET attempts = attempts + 1,
		    next_attempt_at = ?,
		    last_error = ?
		WHERE seq = ?`,
		formatOutboxTime(nextAttemptAt), errMsg, seq,
	)
	if err != nil {
		return fmt.Errorf("outbox bump: %w", err)
	}
	return nil
}

func (s *syncStore) OutboxDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("outbox depth: %w", err)
	}
	return n, nil
}

func (s *syncStore) GetCursor(ctx context.Context, kind string) (string, error) {
	var cur string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM sync_state WHERE kind = ?`, kind).Scan(&cur)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sync cursor get: %w", err)
	}
	return cur, nil
}

func (s *syncStore) SetCursor(ctx context.Context, kind, cursor string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_state (kind, cursor, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		kind, cursor, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sync cursor set: %w", err)
	}
	return nil
}
