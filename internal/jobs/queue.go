// Package jobs is the work queue in Postgres (table jobs, see docs/database.md#jobs) and the
// worker loop that drains it. Delivery is at-least-once: every job must be idempotent.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLeaseLost means another worker reclaimed the job (our lease expired). The result of our
// run must be thrown away: the other worker's run is the one that counts.
var ErrLeaseLost = errors.New("lease lost: job was reclaimed by another worker")

// Job is a claimed job. Attempt is the fencing token: every later update includes it.
type Job struct {
	ID          int64
	TrackID     string
	Attempt     int // 1 on the first run
	MaxAttempts int
}

type Queue struct {
	pool *pgxpool.Pool
}

func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool}
}

// Claim takes the oldest runnable job and leases it for lease. It returns nil, nil when there's
// nothing to do. One statement, committed immediately: the work happens outside any transaction.
func (q *Queue) Claim(ctx context.Context, lease time.Duration) (*Job, error) {
	var j Job
	err := q.pool.QueryRow(ctx, `
		UPDATE jobs
		SET status = 'running', attempts = attempts + 1,
		    locked_until = now() + make_interval(secs => $1), updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE (status = 'queued'  AND run_at <= now())
			   OR (status = 'running' AND locked_until < now())  -- lease expired: its worker died
			ORDER BY run_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED  -- other workers skip this row instead of waiting for it
		)
		RETURNING id, track_id, attempts, max_attempts`, lease.Seconds()).Scan(&j.ID, &j.TrackID, &j.Attempt, &j.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("[JOBS] claim: %w", err)
	}
	return &j, nil
}

// Complete marks the job done and its track ready, in one transaction.
// (Only ingest_track jobs exist; when another kind arrives, the track update moves to the job's handler.)
func (q *Queue) Complete(ctx context.Context, j *Job) error {
	return q.finish(ctx, j, `
		UPDATE jobs SET status = 'done', locked_until = NULL, last_error = NULL, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'running'`, nil, "ready")
}

// Retry puts the job back in the queue, runnable again after delay
func (q *Queue) Retry(ctx context.Context, j *Job, cause error, delay time.Duration) error {
	return q.finish(ctx, j, `
		UPDATE jobs SET status = 'queued', locked_until = NULL, run_at = now() + make_interval(secs => $3),
		       last_error = $4, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'running'`, []any{delay.Seconds(), cleanError(cause)}, "")
}

// Fail gives up on the job for good and marks its track failed
func (q *Queue) Fail(ctx context.Context, j *Job, cause error) error {
	return q.finish(ctx, j, `
		UPDATE jobs SET status = 'failed', locked_until = NULL, last_error = $3, updated_at = now()
		WHERE id = $1 AND attempts = $2 AND status = 'running'`, []any{cleanError(cause)}, "failed")
}

// finish runs a fenced update of the job, and optionally sets the track's status, in one transaction.
// "attempts = $2" is the fence: if another worker reclaimed the job, attempts has moved on, the
// update matches 0 rows, and nothing else in the transaction is applied.
func (q *Queue) finish(ctx context.Context, j *Job, jobSQL string, extra []any, trackStatus string) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("[JOBS] begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	args := append([]any{j.ID, j.Attempt}, extra...)
	tag, err := tx.Exec(ctx, jobSQL, args...)
	if err != nil {
		return fmt.Errorf("[JOBS] update job %d: %w", j.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost // rolled back by the defer: the track is left alone too
	}
	if trackStatus != "" {
		if _, err := tx.Exec(ctx, `UPDATE tracks SET status = $1 WHERE id = $2`, trackStatus, j.TrackID); err != nil {
			return fmt.Errorf("[JOBS] update track %s: %w", j.TrackID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("[JOBS] commit: %w", err)
	}
	return nil
}

const maxErrorLen = 1000

// cleanError makes an error safe to store. Errors from parsing an uploaded file can contain bytes
// from the file: Postgres text rejects invalid UTF-8 and NUL bytes (the update would fail and the
// job would be stuck), and an endless message would bloat the row.
func cleanError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToValidUTF8(err.Error(), "�")
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > maxErrorLen {
		s = s[:maxErrorLen]
		s = strings.ToValidUTF8(s, "") // the cut may land inside a multi-byte character
		s += "…"
	}
	return s
}
