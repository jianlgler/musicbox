package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jianlgler/musicbox/internal/jobs"
)

// fakeQueue hands out its jobs in order, then cancels the worker's context (queue drained)
type fakeQueue struct {
	mu        sync.Mutex
	pending   []*jobs.Job
	claimErrs []error // returned by the first Claim calls, before any job
	drained   context.CancelFunc
	finishErr error // returned by Complete/Retry/Fail

	completed []int64
	failed    map[int64]error
	retried   map[int64]time.Duration
}

func (q *fakeQueue) Claim(context.Context, time.Duration) (*jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.claimErrs) > 0 {
		err := q.claimErrs[0]
		q.claimErrs = q.claimErrs[1:]
		return nil, err
	}
	if len(q.pending) == 0 {
		q.drained()
		return nil, nil
	}
	j := q.pending[0]
	q.pending = q.pending[1:]
	return j, nil
}

func (q *fakeQueue) Complete(_ context.Context, j *jobs.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.completed = append(q.completed, j.ID)
	return q.finishErr
}

func (q *fakeQueue) Retry(_ context.Context, j *jobs.Job, _ error, delay time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retried[j.ID] = delay
	return q.finishErr
}

func (q *fakeQueue) Fail(_ context.Context, j *jobs.Job, cause error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[j.ID] = cause
	return q.finishErr
}

// handlerFunc turns a function into a jobs.Handler
type handlerFunc func(ctx context.Context, j *jobs.Job) error

func (f handlerFunc) Handle(ctx context.Context, j *jobs.Job) error { return f(ctx, j) }

var fastConfig = jobs.Config{Lease: time.Minute, JobTimeout: 30 * time.Second, PollInterval: time.Millisecond}

// run drains the queue through a worker and returns the fake to inspect
func run(t *testing.T, h jobs.Handler, queued ...*jobs.Job) *fakeQueue {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &fakeQueue{pending: queued, drained: cancel, failed: map[int64]error{}, retried: map[int64]time.Duration{}}
	w, err := jobs.NewWorker(q, h, fastConfig)
	if err != nil {
		t.Fatal(err)
	}
	w.Run(ctx)
	return q
}

func job(id int64, attempt int) *jobs.Job {
	return &jobs.Job{ID: id, TrackID: "track", Attempt: attempt, MaxAttempts: 5}
}

func TestNewWorkerRejectsTimeoutLongerThanLease(t *testing.T) {
	for _, timeout := range []time.Duration{time.Minute, 2 * time.Minute} {
		if _, err := jobs.NewWorker(&fakeQueue{}, nil, jobs.Config{Lease: time.Minute, JobTimeout: timeout}); err == nil {
			t.Errorf("job timeout %s with a 1m lease accepted: two workers could run one job", timeout)
		}
	}
	if _, err := jobs.NewWorker(&fakeQueue{}, nil, jobs.Config{}); err != nil {
		t.Errorf("defaults rejected: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	want := map[int]time.Duration{
		0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute,
		8: time.Hour, 1000: time.Hour, // capped, and no overflow on huge attempt counts
	}
	for attempt, d := range want {
		if got := jobs.Backoff(attempt); got != d {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, d)
		}
	}
}

func TestSuccessCompletes(t *testing.T) {
	q := run(t, handlerFunc(func(context.Context, *jobs.Job) error { return nil }), job(1, 1), job(2, 1))
	if len(q.completed) != 2 || len(q.failed) != 0 || len(q.retried) != 0 {
		t.Errorf("completed %v, failed %v, retried %v; want both completed", q.completed, q.failed, q.retried)
	}
}

func TestTransientErrorRetriesWithBackoff(t *testing.T) {
	q := run(t, handlerFunc(func(context.Context, *jobs.Job) error { return errors.New("storage unreachable") }), job(1, 2))
	delay, ok := q.retried[1]
	if !ok {
		t.Fatalf("not retried; failed %v", q.failed)
	}
	// Attempt 2 → 1 minute, plus up to 20% jitter
	if delay < time.Minute || delay >= 72*time.Second {
		t.Errorf("retry delay %s, want 1m to 1m12s", delay)
	}
}

func TestLastAttemptFails(t *testing.T) {
	q := run(t, handlerFunc(func(context.Context, *jobs.Job) error { return errors.New("storage unreachable") }), job(1, 5))
	if _, ok := q.failed[1]; !ok || len(q.retried) != 0 {
		t.Errorf("failed %v, retried %v; want failed after attempt 5/5", q.failed, q.retried)
	}
}

func TestPermanentErrorFailsAtOnce(t *testing.T) {
	q := run(t, handlerFunc(func(context.Context, *jobs.Job) error { return jobs.Permanent(errors.New("not audio")) }), job(1, 1))
	if _, ok := q.failed[1]; !ok || len(q.retried) != 0 {
		t.Errorf("failed %v, retried %v; want failed on attempt 1", q.failed, q.retried)
	}
}

// The gap noted in Step 1: a job reclaimed after its last attempt must not run again
func TestReclaimedPastLimitFailsWithoutRunning(t *testing.T) {
	ran := false
	q := run(t, handlerFunc(func(context.Context, *jobs.Job) error { ran = true; return nil }), job(1, 6))
	if ran {
		t.Error("handler ran on attempt 6/5")
	}
	if err, ok := q.failed[1]; !ok || !strings.Contains(err.Error(), "lease expired") {
		t.Errorf("failed[1] = %v, want a lease-expired failure", err)
	}
}

func TestPanicFailsJobAndWorkerContinues(t *testing.T) {
	q := run(t, handlerFunc(func(_ context.Context, j *jobs.Job) error {
		if j.ID == 1 {
			var m map[string]int
			m["crafted file"]++ // nil map write: panics
		}
		return nil
	}), job(1, 1), job(2, 1))
	if err, ok := q.failed[1]; !ok || !strings.Contains(err.Error(), "panic") {
		t.Errorf("failed[1] = %v, want a panic failure", err)
	}
	if len(q.completed) != 1 || q.completed[0] != 2 {
		t.Errorf("completed %v, want job 2: the worker must survive the panic", q.completed)
	}
}

func TestJobGetsATimeout(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	run(t, handlerFunc(func(ctx context.Context, _ *jobs.Job) error {
		deadline, hasDeadline = ctx.Deadline()
		return nil
	}), job(1, 1))
	if !hasDeadline || time.Until(deadline) > fastConfig.JobTimeout {
		t.Errorf("handler deadline %v (set %v), want within %s", deadline, hasDeadline, fastConfig.JobTimeout)
	}
}

func TestLeaseLostDoesNotStopTheWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &fakeQueue{pending: []*jobs.Job{job(1, 1), job(2, 1)}, drained: cancel, finishErr: jobs.ErrLeaseLost,
		failed: map[int64]error{}, retried: map[int64]time.Duration{}}
	// A 1-hour poll: if losing a lease were treated as a worker error, the worker would pause
	// before the next claim, and this test would time out instead of seeing job 2 right away
	cfg := fastConfig
	cfg.PollInterval = time.Hour
	w, _ := jobs.NewWorker(q, handlerFunc(func(context.Context, *jobs.Job) error { return nil }), cfg)
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("worker paused after losing a lease")
	}
	if len(q.completed) != 2 {
		t.Errorf("completed attempts %v, want both jobs tried", q.completed)
	}
}

func TestClaimErrorIsRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &fakeQueue{pending: []*jobs.Job{job(1, 1)}, drained: cancel, claimErrs: []error{errors.New("db down"), errors.New("db down")},
		failed: map[int64]error{}, retried: map[int64]time.Duration{}}
	w, _ := jobs.NewWorker(q, handlerFunc(func(context.Context, *jobs.Job) error { return nil }), fastConfig)
	w.Run(ctx)
	if len(q.completed) != 1 {
		t.Errorf("completed %v, want the job done once the database is back", q.completed)
	}
}

// Shutdown while a job runs: the job is not interrupted, its result is recorded, then Run returns
func TestGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var jobCtxErr error
	q := &fakeQueue{pending: []*jobs.Job{job(1, 1), job(2, 1)}, drained: func() {},
		failed: map[int64]error{}, retried: map[int64]time.Duration{}}
	w, _ := jobs.NewWorker(q, handlerFunc(func(jobCtx context.Context, _ *jobs.Job) error {
		close(started)
		<-release
		jobCtxErr = jobCtx.Err()
		return nil
	}), fastConfig)

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	<-started
	cancel() // SIGTERM arrives mid-job
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return after shutdown")
	}
	if jobCtxErr != nil {
		t.Errorf("job context was cancelled by shutdown: %v", jobCtxErr)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.completed) != 1 || q.completed[0] != 1 {
		t.Errorf("completed %v, want only job 1: its result is recorded, job 2 is never claimed", q.completed)
	}
}
