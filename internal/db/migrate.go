package db

import (
	"context"
	"embed" // provides access to files embedded in the program
	"fmt"
	"io/fs"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

/*
	DON'T INSERT SPACE IN BETWEEN THE FOLLOWING COMPILER DIRECTIVE
 	At build time, Go copies every file matching the pattern into the binary and makes them readable through migrationsFS
*/

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Keep the same in every instance of the API
const migrationLockID = 72707369

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// One dedicated connection: advisory locks belong to a connection, not to the pool
	conn, err := pool.Acquire(ctx) // borrows one connection explicitly and holds it
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// create schema_migrations if it doesn't exist (conn.Exec)
	_, err = conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name		text PRIMARY KEY,
			applied_at	timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("[MIGRATIONS] create schema_migrations; %w", err)
	}

	/****************************CONCURRENCY**************************************/

	// LOCK ACQUISITION: blocks until no other instance holds the lock
	// pg_advisory_lock waits until it's released
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("[MIGRATIONS] acquire migration lock: %w", err)
	}
	defer func() {
		// LOCK RELEASE: it runs when Migrate returns
		// WithoutCancel: still unlock even if ctx was cancelled mid-migration
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			log.Printf("[MIGRATIONS] release migration lock: %v", err)
		}
	}()
	/****************************END OF CONCURRENCY*******************************/

	/*********************************MIGRATION***********************************/
	// ReadFile returns []bytem later Exec will take a String
	entries, err := fs.ReadDir(migrationsFS, "migrations") // Sorted by filename
	if err != nil {
		return fmt.Errorf("[MIGRATIONS] read migrations: %w", err)
	}

	for _, e := range entries {
		name := e.Name()

		var applied bool
		/*
			The following runs a query that returns one row and copies its column into applied
		*/
		err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", name, // It returns true or false
		).Scan(&applied) // EXISTS always returns one row; a plain SELECT with no match would return no rows and Scan would fail

		if err != nil {
			return fmt.Errorf("[MIGRATIONS] check %s: %w", name, err)
		}

		if applied {
			continue // skips the rest of the loop
		}

		// Reads SQL code inside the entry (now in RAM, thanks to by //go:embed)
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}

		tx, err := conn.Begin(ctx) // Starts transaction on the dedicated connection
		if err != nil {
			return fmt.Errorf("[MIGRATIONS] begin %s: %w", name, err)
		}
		defer tx.Rollback(ctx) // If any step returns early, everything in the transaction is undone, no-op once committed

		// It runs the whole file content
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("[MIGRATIONS] apply %s: %w", name, err)
		}

		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
			return fmt.Errorf("[MIGRATIONS] record %s: %w", name, err)
		}

		// Makes the schema change and the record update permanent
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("[MIGRATIONS] commit %s: %w", name, err)
		}

		log.Printf("[MIGRATIONS] applied %s", name)
	}
	// If nil, every migration succeeded
	// Now deferred unlock and conn.Release() will run
	return nil
}
