// The ingest worker: claims jobs from Postgres and processes uploaded tracks in the background.
// Run as many as you like; FOR UPDATE SKIP LOCKED gives each one different jobs.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jianlgler/musicbox/internal/ingest"
	"github.com/jianlgler/musicbox/internal/jobs"
	"github.com/jianlgler/musicbox/internal/probe"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

func main() {
	// The same binary is the probe child: the worker re-runs itself to parse each file in a
	// separate process (see internal/probe). Checked first, before reading any config or secret.
	if len(os.Args) > 1 && os.Args[1] == probe.ChildArg {
		os.Exit(probe.ChildMain(os.Args[2:]))
	}

	// SIGINT (Ctrl-C) or SIGTERM (docker stop, systemd) cancels ctx: the graceful shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL") // never logged
	if dsn == "" {
		log.Fatal("[CONFIG] DATABASE_URL is not set")
	}
	startCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(startCtx, dsn)
	if err != nil {
		log.Fatalf("[CONFIG] parse DATABASE_URL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(startCtx); err != nil {
		log.Fatalf("[DB] connect to database: %v", err)
	}
	// No migrations here: the API owns the schema. The worker only needs the tables to exist.

	objects, err := storage.New(storage.Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Bucket:    os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
	})
	if err != nil {
		log.Fatalf("[CONFIG] %v", err)
	}
	if err := objects.Ping(startCtx); err != nil {
		log.Fatalf("[STORAGE] connect to object storage: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		log.Fatalf("[CONFIG] find own executable: %v", err)
	}
	prober := probe.Isolated{Executable: self, Timeout: probe.DefaultTimeout, TempDir: os.TempDir()}

	w, err := jobs.NewWorker(jobs.NewQueue(pool), ingest.NewHandler(tracks.NewStore(pool), objects, prober), jobs.Config{})
	if err != nil {
		log.Fatal(err)
	}

	// After the first signal, restore the default behaviour: a second Ctrl-C kills the process
	// at once, for when the running job is stuck and you don't want to wait for its timeout
	announced := make(chan struct{})
	go func() {
		<-ctx.Done()
		stop()
		log.Printf("[WORKER] shutting down: finishing the current job (Ctrl-C again to force)")
		close(announced)
	}()

	log.Printf("[WORKER] started")
	w.Run(ctx)  // returns once ctx is cancelled and the current job is recorded
	<-announced // otherwise main can return, and the process exit, before the message is printed
	log.Printf("[WORKER] stopped")
}
