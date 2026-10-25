package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jianlgler/musicbox/internal/db"
	"github.com/jianlgler/musicbox/internal/tracks"
)

// Every list endpoint needs a cap, otherwise ?limit=99999999 loads the whole library into memory
const (
	defaultLimit = 50
	maxLimit     = 200
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

	trackStore := tracks.NewStore(pool) // Created once, shared by every request

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

	// List tracks, newest first: GET /api/v1/tracks?limit=N
	mux.HandleFunc("GET /api/v1/tracks", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

		// Never trust the query string: missing means default, anything else must be in range
		limit := defaultLimit
		if s := r.URL.Query().Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > maxLimit {
				// %q quotes and escapes the input, so a "\n" in it can't forge an extra log line
				log.Printf("[BAD_REQUEST] %s %s - invalid limit %q", r.Method, r.URL.Path, s)
				writeJSON(w, r, http.StatusBadRequest, map[string]string{
					"error": fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit),
				})
				return
			}
			limit = n
		}

		// r.Context() is cancelled if the client disconnects, so the query stops too
		list, err := trackStore.List(r.Context(), limit)
		if err != nil {
			// Details go to our logs, the client only gets a generic message
			log.Printf("list tracks: %v", err) // err already carries the [TRACKS] tag
			writeJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"tracks": list})
	})

	addr := "127.0.0.1:8080"
	log.Printf("[HTTP] listening on %s", addr)                     // Blocks forever and only returns on an error, such as the port already
	log.Fatalf("[HTTP] serve: %v", http.ListenAndServe(addr, mux)) // Prints that error and exits
}

// Helper func
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[WRITE_ERROR] %s %s - %v", r.Method, r.URL.Path, err)
	}
}
