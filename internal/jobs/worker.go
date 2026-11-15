package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"time"
)

// JobQueue is what the worker needs from the queue. *Queue satisfies it; tests use a fake.
type JobQueue interface {
	Claim(ctx context.Context, lease time.Duration) (*Job, error)
	Complete(ctx context.Context, j *Job) error
	Retry(ctx context.Context, j *Job, cause error, delay time.Duration) error
	Fail(ctx context.Context, j *Job, cause error) error
}

// Handler does the actual work of a job. It must be idempotent, and must stop when ctx is done.
type Handler interface {
	Handle(ctx context.Context, j *Job) error
}

type Config struct {
	Lease        time.Duration // how long a claimed job belongs to this worker (default 5 min)
	JobTimeout   time.Duration // how long a job may run; must be shorter than Lease (default 4 min)
	PollInterval time.Duration // wait between claims when the queue is empty (default 2 s)
}

type Worker struct {
	queue   JobQueue
	handler Handler
	cfg     Config
	jitter  func(max time.Duration) time.Duration // replaced in tests
}

func NewWorker(q JobQueue, h Handler, cfg Config) (*Worker, error) {
	if cfg.Lease == 0 {
		cfg.Lease = 5 * time.Minute
	}
	if cfg.JobTimeout == 0 {
		cfg.JobTimeout = 4 * time.Minute
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	// If a job could outlive its lease, another worker would reclaim it while it's still running:
	// two workers on one job. The margin leaves time to record the result before the lease ends.
	if cfg.JobTimeout >= cfg.Lease {
		return nil, fmt.Errorf("[WORKER] job timeout (%s) must be shorter than the lease (%s)", cfg.JobTimeout, cfg.Lease)
	}
	return &Worker{queue: q, handler: h, cfg: cfg, jitter: randomUpTo}, nil
}

// Run claims and runs jobs, one at a time, until ctx is cancelled. Cancelling ctx is a graceful
// shutdown: no new job is claimed, but a running job finishes (up to JobTimeout) and its result
// is recorded. Run more worker processes to run jobs in parallel: SKIP LOCKED keeps them apart.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := w.runOne(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("[WORKER] %v", err) // e.g. the database is down: wait, then try again
		}
		if !worked || err != nil {
			sleep(ctx, w.cfg.PollInterval)
		}
	}
}

// runOne claims one job and runs it. worked is false when the queue was empty.
func (w *Worker) runOne(ctx context.Context) (worked bool, err error) {
	job, err := w.queue.Claim(ctx, w.cfg.Lease)
	if err != nil || job == nil {
		return false, err
	}

	// From here on, shutdown must not interrupt: WithoutCancel keeps ctx's values but not its
	// cancellation. The job gets its own timeout, and recording the result gets a short one.
	base := context.WithoutCancel(ctx)

	var result error
	if job.Attempt > job.MaxAttempts {
		// Reclaimed after its last allowed attempt: the worker running it died or hung, and the
		// next run would probably do the same. Without this check it would be retried forever.
		result = Permanent(errors.New("lease expired during the last attempt (worker crashed or hung)"))
	} else {
		jobCtx, cancel := context.WithTimeout(base, w.cfg.JobTimeout)
		result = w.handle(jobCtx, job)
		cancel()
	}

	recordCtx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	return true, w.record(recordCtx, job, result)
}

// handle runs the handler, turning a panic into a permanent error: crafted input that crashes a
// parser must fail its own job, not kill the worker for everyone.
func (w *Worker) handle(ctx context.Context, job *Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Permanent(fmt.Errorf("panic: %v", r))
		}
	}()
	return w.handler.Handle(ctx, job)
}

func (w *Worker) record(ctx context.Context, job *Job, result error) error {
	var err error
	switch {
	case result == nil:
		err = w.queue.Complete(ctx, job)
		if err == nil {
			log.Printf("[WORKER] job %d (track %s) done on attempt %d", job.ID, job.TrackID, job.Attempt)
		}
	case IsPermanent(result) || job.Attempt >= job.MaxAttempts:
		err = w.queue.Fail(ctx, job, result)
		if err == nil {
			// %q: the error may contain text from the uploaded file
			log.Printf("[WORKER] job %d (track %s) failed on attempt %d/%d: %q", job.ID, job.TrackID, job.Attempt, job.MaxAttempts, result.Error())
		}
	default:
		delay := Backoff(job.Attempt)
		delay += w.jitter(delay / 5)
		err = w.queue.Retry(ctx, job, result, delay)
		if err == nil {
			log.Printf("[WORKER] job %d (track %s) attempt %d/%d failed, retry in %s: %q", job.ID, job.TrackID, job.Attempt, job.MaxAttempts, delay.Round(time.Second), result.Error())
		}
	}
	if errors.Is(err, ErrLeaseLost) {
		log.Printf("[WORKER] job %d: %v; result discarded", job.ID, err)
		return nil // not a worker failure: the job is in another worker's hands
	}
	return err
}

// Backoff is the wait before the next attempt: 30 s, 1 min, 2 min, 4 min…, capped at 1 hour.
// Exponential, so a dependency that's down isn't hammered by every job at a fixed rate.
func Backoff(attempt int) time.Duration {
	const base, max = 30 * time.Second, time.Hour
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

// Jitter spreads retries out: when storage is down, every job fails at once, and without it
// they'd all retry at the same instant again
func randomUpTo(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return rand.N(max)
}

// sleep waits d, or less if ctx is cancelled, so shutdown never waits for a poll interval
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Permanent marks an error as not worth retrying: a corrupt file will still be corrupt in 30 s
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }
