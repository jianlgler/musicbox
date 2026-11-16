// Package tracks is the data layer for tracks: SQL lives here, HTTP never does.
package tracks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Track struct {
	ID          string    `json:"id"`
	Title       *string   `json:"title"`  // NULL until the ingest worker reads the tags
	Artist      *string   `json:"artist"` // *string: nil becomes JSON null
	Album       *string   `json:"album"`
	AlbumArtist *string   `json:"album_artist"`
	TrackNumber *int      `json:"track_number"`
	DiscNumber  *int      `json:"disc_number"`
	Year        *int      `json:"year"`
	DurationMS  *int64    `json:"duration_ms"`
	HasCover    bool      `json:"has_cover"` // the cover's hash stays internal, like the file's
	Status      string    `json:"status"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
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
		SELECT `+trackColumns+`
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
		if err := scanTrack(rows, &t); err != nil {
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

// Same columns, same order, for every query that returns a Track
const trackColumns = `id, title, artist, album, album_artist, track_number, disc_number, year,
	duration_ms, cover_sha256 IS NOT NULL, status, size_bytes, created_at`

func scanTrack(row pgx.Row, t *Track) error {
	return row.Scan(&t.ID, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.TrackNumber, &t.DiscNumber,
		&t.Year, &t.DurationMS, &t.HasCover, &t.Status, &t.SizeBytes, &t.CreatedAt)
}

// Create records an uploaded file whose bytes are already in storage, and enqueues its ingest job.
// If a track with this sha256 exists, it returns that track with created=false and enqueues nothing:
// uploading the same file twice is harmless. The caller is responsible for computing sha256
// from the bytes itself, never taking it from the client.
func (s *Store) Create(ctx context.Context, sha256 string, size int64) (t Track, created bool, err error) {
	// One transaction: a track can never be left pending without a job to finish it
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Track{}, false, fmt.Errorf("[TRACKS] begin: %w", err)
	}
	// After a successful Commit this is a no-op; on any early return it undoes everything
	defer func() { _ = tx.Rollback(ctx) }()

	// ON CONFLICT DO NOTHING + RETURNING returns no row when the sha256 already exists.
	// Two identical uploads at the same moment: the second INSERT waits for the first to commit,
	// then does nothing, so there's still exactly one row.
	err = scanTrack(tx.QueryRow(ctx, `
		INSERT INTO tracks (sha256, size_bytes) VALUES ($1, $2)
		ON CONFLICT (sha256) DO NOTHING
		RETURNING `+trackColumns, sha256, size), &t)

	if errors.Is(err, pgx.ErrNoRows) {
		// Already there. Read Committed: this new statement sees the row the other upload committed.
		if err := scanTrack(tx.QueryRow(ctx, `SELECT `+trackColumns+` FROM tracks WHERE sha256 = $1`, sha256), &t); err != nil {
			return Track{}, false, fmt.Errorf("[TRACKS] select existing track: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Track{}, false, fmt.Errorf("[TRACKS] commit: %w", err)
		}
		return t, false, nil
	}
	if err != nil {
		return Track{}, false, fmt.Errorf("[TRACKS] insert track: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO jobs (kind, track_id) VALUES ('ingest_track', $1)`, t.ID); err != nil {
		return Track{}, false, fmt.Errorf("[TRACKS] enqueue ingest job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Track{}, false, fmt.Errorf("[TRACKS] commit: %w", err)
	}
	return t, true, nil
}

// ErrNotFound is returned when no track has the given id
var ErrNotFound = errors.New("track not found")

// File identifies a track's stored bytes. Internal only: never sent to clients.
type File struct {
	SHA256 string
	Size   int64
}

// File returns what the ingest worker needs to find and verify a track's object
func (s *Store) File(ctx context.Context, trackID string) (File, error) {
	var f File
	err := s.pool.QueryRow(ctx, `SELECT sha256, size_bytes FROM tracks WHERE id = $1`, trackID).Scan(&f.SHA256, &f.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("[TRACKS] select file of %s: %w", trackID, err)
	}
	return f, nil
}

// Metadata is what the ingest worker read from a track's file. nil fields are stored as NULL.
type Metadata struct {
	Title, Artist, Album, AlbumArtist *string
	TrackNumber, DiscNumber, Year     *int
	DurationMS                        int64
	CoverSHA256                       *string // object covers/<hash>, already stored
}

// SetMetadata replaces the track's metadata with m. It's a plain overwrite with the same values
// every time the same file is parsed, so it's safe for an ingest job that runs twice.
func (s *Store) SetMetadata(ctx context.Context, trackID string, m Metadata) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tracks
		SET title = $2, artist = $3, album = $4, album_artist = $5,
		    track_number = $6, disc_number = $7, year = $8, duration_ms = $9, cover_sha256 = $10
		WHERE id = $1`,
		trackID, m.Title, m.Artist, m.Album, m.AlbumArtist, m.TrackNumber, m.DiscNumber, m.Year, m.DurationMS, m.CoverSHA256)
	if err != nil {
		return fmt.Errorf("[TRACKS] set metadata of %s: %w", trackID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
