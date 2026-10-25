// Package tracks is the data layer for tracks: SQL lives here, HTTP never does.
package tracks

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Track struct {
	ID        string    `json:"id"`
	Title     *string   `json:"title"`  // NULL until the ingest worker reads the tags
	Artist    *string   `json:"artist"` // *string: nil becomes JSON null
	Album     *string   `json:"album"`
	Status    string    `json:"status"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is a type (not plain functions) so the HTTP layer can swap in a fake in tests
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// List returns the newest tracks first. The caller is responsible for validating limit.
func (s *Store) List(ctx context.Context, limit int) ([]Track, error) {
	// id breaks ties between tracks created in the same microsecond, so the order is stable
	rows, err := s.pool.Query(ctx, `
		SELECT id, title, artist, album, status, size_bytes, created_at
		FROM tracks
		ORDER BY created_at DESC, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("[TRACKS] query tracks: %w", err)
	}
	defer rows.Close() // Gives the connection back to the pool

	result := []Track{} // Not a nil slice: an empty list must encode as [], not null
	for rows.Next() {
		var t Track
		// Same order as the SELECT
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.Status, &t.SizeBytes, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("[TRACKS] scan track: %w", err)
		}
		result = append(result, t)
	}
	// rows.Next() also returns false when iteration fails midway, only rows.Err() tells them apart
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("[TRACKS] iterate tracks: %w", err)
	}
	return result, nil
}
