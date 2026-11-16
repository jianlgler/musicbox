package tracks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// There are no artist or album tables: both are derived from the tags of ready tracks.
// An album is identified by (album artist, album title), where the album artist falls back to the
// track artist. That's what keeps a compilation (one album artist, many track artists) together
// while two different albums both called "Greatest Hits" stay apart.
const albumArtist = `coalesce(album_artist, artist)`

// MaxAlbumTracks bounds an album's track list (a box set can have a few hundred, not thousands)
const MaxAlbumTracks = 1000

// ErrNoCover is returned when the track exists but has no usable cover art
var ErrNoCover = errors.New("track has no cover")

type Artist struct {
	Name       string `json:"name"`
	AlbumCount int    `json:"album_count"`
	TrackCount int    `json:"track_count"`
}

type Album struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Year       *int   `json:"year"` // the latest year among its tracks
	TrackCount int    `json:"track_count"`
	DurationMS int64  `json:"duration_ms"`
	// A track whose cover stands for the album: GET /api/v1/tracks/{id}/cover. nil if none has one.
	CoverTrackID *string `json:"cover_track_id"`
}

// Get returns one track, whatever its status
func (s *Store) Get(ctx context.Context, id string) (Track, error) {
	var t Track
	err := scanTrack(s.pool.QueryRow(ctx, `SELECT `+trackColumns+` FROM tracks WHERE id = $1`, id), &t)
	if errors.Is(err, pgx.ErrNoRows) {
		return Track{}, ErrNotFound
	}
	if err != nil {
		return Track{}, fmt.Errorf("[TRACKS] select track %s: %w", id, err)
	}
	return t, nil
}

// CoverHash returns the hash naming the track's cover object (covers/<hash>). Internal only.
func (s *Store) CoverHash(ctx context.Context, id string) (string, error) {
	var hash *string
	err := s.pool.QueryRow(ctx, `SELECT cover_sha256 FROM tracks WHERE id = $1`, id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("[TRACKS] select cover of %s: %w", id, err)
	}
	if hash == nil {
		return "", ErrNoCover
	}
	return *hash, nil
}

// Artists lists album artists (see albumArtist) of ready tracks, alphabetically, ignoring case
func (s *Store) Artists(ctx context.Context, limit int) ([]Artist, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+albumArtist+`, count(DISTINCT album), count(*)
		FROM tracks
		WHERE status = 'ready' AND `+albumArtist+` IS NOT NULL
		GROUP BY `+albumArtist+`
		ORDER BY lower(`+albumArtist+`), `+albumArtist+`
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("[TRACKS] query artists: %w", err)
	}
	return collect(rows, "artists", func(row pgx.Row, a *Artist) error {
		return row.Scan(&a.Name, &a.AlbumCount, &a.TrackCount)
	})
}

// Albums lists albums of ready tracks, alphabetically by artist then title. artist "" means all
// artists; otherwise only that album artist's albums (exact match: names come from Artists).
func (s *Store) Albums(ctx context.Context, artist string, limit int) ([]Album, error) {
	return s.albums(ctx, artist, "", limit)
}

// AlbumTracks returns one album and its tracks in play order: disc, track number, then title
func (s *Store) AlbumTracks(ctx context.Context, artist, title string) (Album, []Track, error) {
	found, err := s.albums(ctx, artist, title, 1)
	if err != nil {
		return Album{}, nil, err
	}
	if len(found) == 0 {
		return Album{}, nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+trackColumns+`
		FROM tracks
		WHERE status = 'ready' AND `+albumArtist+` = $1 AND album = $2
		ORDER BY disc_number NULLS LAST, track_number NULLS LAST, lower(title), id
		LIMIT $3`, artist, title, MaxAlbumTracks)
	if err != nil {
		return Album{}, nil, fmt.Errorf("[TRACKS] query album tracks: %w", err)
	}
	list, err := collect(rows, "album tracks", scanTrack)
	return found[0], list, err
}

// albums is Albums with an optional exact title, so the summary AlbumTracks returns is computed
// by the same SQL as the list (two queries computing "the same" numbers drift apart)
func (s *Store) albums(ctx context.Context, artist, title string, limit int) ([]Album, error) {
	// The cover: the first track (in play order) that has one
	rows, err := s.pool.Query(ctx, `
		SELECT album, `+albumArtist+`, max(year), count(*), coalesce(sum(duration_ms), 0),
		       (array_agg(id ORDER BY disc_number NULLS LAST, track_number NULLS LAST, id)
		            FILTER (WHERE cover_sha256 IS NOT NULL))[1]
		FROM tracks
		WHERE status = 'ready' AND album IS NOT NULL AND `+albumArtist+` IS NOT NULL
		  AND ($1 = '' OR `+albumArtist+` = $1)
		  AND ($2 = '' OR album = $2)
		GROUP BY `+albumArtist+`, album
		ORDER BY lower(`+albumArtist+`), `+albumArtist+`, lower(album), album
		LIMIT $3`, artist, title, limit)
	if err != nil {
		return nil, fmt.Errorf("[TRACKS] query albums: %w", err)
	}
	return collect(rows, "albums", func(row pgx.Row, a *Album) error {
		return row.Scan(&a.Title, &a.Artist, &a.Year, &a.TrackCount, &a.DurationMS, &a.CoverTrackID)
	})
}

// Search finds ready tracks whose title, artist, album or album artist contains query, ignoring
// case. Title matches come first. The caller validates query (non-empty, bounded, no NUL).
//
// No index helps a "contains" search (LIKE '%x%' can't use a B-tree), so this reads every ready
// track. At 50,000 tracks that's a few milliseconds; pg_trgm is the fix if it ever isn't.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Track, error) {
	pattern := "%" + EscapeLike(query) + "%"
	rows, err := s.pool.Query(ctx, `
		SELECT `+trackColumns+`
		FROM tracks
		WHERE status = 'ready'
		  AND (title ILIKE $1 OR artist ILIKE $1 OR album ILIKE $1 OR album_artist ILIKE $1)
		ORDER BY title ILIKE $1 DESC, lower(title), id
		LIMIT $2`, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("[TRACKS] search: %w", err)
	}
	return collect(rows, "search results", scanTrack)
}

// EscapeLike makes s match itself literally inside a LIKE pattern. Without it a search for "%"
// matches everything, and "_" matches any one character. Backslash is LIKE's default escape
// character, so it's escaped first.
func EscapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// collect scans every row into a non-nil slice (an empty result encodes as [], not null)
func collect[T any](rows pgx.Rows, what string, scan func(pgx.Row, *T) error) ([]T, error) {
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, fmt.Errorf("[TRACKS] scan %s: %w", what, err)
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("[TRACKS] iterate %s: %w", what, err)
	}
	return result, nil
}
