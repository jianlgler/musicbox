package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jianlgler/musicbox/internal/db"
)

func main() {
	// Fail fast
	// Never log dsn
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	// Startup gets 5 seconds to reach the database, so a dead DB can't hang us forever
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn) // Only parses the config, doesn't connect
	if err != nil {
		log.Fatalf("parse DATABASE_URL: %v", err)
	}
	defer pool.Close() // It closes when the main returns

	if err := pool.Ping(ctx); err != nil { // This one actually connects
		log.Fatalf("connect to database: %v", err)
	}
	log.Printf("connected to database")

	// Bring the schema up to date before serving any request.
	// Own timeout: may wait for another instance's migrations, so 5s is too tight
	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelMigrate()
	if err := db.Migrate(migrateCtx, pool); err != nil {
		log.Fatalf("[MIGRATIONS] migrate: %v", err) // It skips defer
	}

	mux := http.NewServeMux()

	// Liveness
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

		// A probe that hangs is worse than one that fails
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			log.Printf("[NOT_READY] database ping failed: %v", err)
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
	})

	addr := "127.0.0.1:8080"
	log.Printf("listening on %s", addr)       // Blocks forever and only returns on an error, such as the port already
	log.Fatal(http.ListenAndServe(addr, mux)) // Prints that error and exits
}

// Helper func
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[WRITE_ERROR] %s %s - %v", r.Method, r.URL.Path, err)
	}
}
