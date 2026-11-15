package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jianlgler/musicbox/internal/db"
	"github.com/jianlgler/musicbox/internal/httpapi"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

func main() {
	// Fail fast
	// Never log dsn
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("[CONFIG] DATABASE_URL is not set")
	}

	// Startup gets 5 seconds to reach the database, so a dead DB can't hang us forever
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn) // Only parses the config, doesn't connect
	if err != nil {
		log.Fatalf("[CONFIG] parse DATABASE_URL: %v", err)
	}
	defer pool.Close() // It closes when the main returns

	if err := pool.Ping(ctx); err != nil { // This one actually connects
		log.Fatalf("[DB] connect to database: %v", err)
	}
	log.Printf("[DB] connected to database")

	// Bring the schema up to date before serving any request.
	// Own timeout: may wait for another instance's migrations, so 5s is too tight
	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelMigrate()
	if err := db.Migrate(migrateCtx, pool); err != nil {
		log.Fatalf("migrate: %v", err) // It skips defer
	}

	// Object storage. Never log the keys; New validates that all four are set.
	objects, err := storage.New(storage.Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Bucket:    os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
	})
	if err != nil {
		log.Fatalf("[CONFIG] %v", err)
	}
	storageCtx, cancelStorage := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStorage()
	if err := objects.Ping(storageCtx); err != nil { // wrong keys or missing bucket fail here, not on the first upload
		log.Fatalf("[STORAGE] connect to object storage: %v", err)
	}
	log.Printf("[STORAGE] connected to object storage")

	srv := &http.Server{
		Addr:    "127.0.0.1:8080",
		Handler: httpapi.New(pool, tracks.NewStore(pool), objects), // Created once, shared by every request
		// Slowloris: without it, a client sending headers one byte at a time holds a connection forever
		ReadHeaderTimeout: 5 * time.Second,
		// The same attack on the body. Uploads raise their own deadline (see httpapi/upload.go)
		ReadTimeout: 30 * time.Second,
		// Close keep-alive connections nobody uses
		IdleTimeout: 2 * time.Minute,
	}

	log.Printf("[HTTP] listening on %s", srv.Addr)
	log.Fatalf("[HTTP] serve: %v", srv.ListenAndServe()) // Blocks forever and only returns on an error, such as the port already in use
}
