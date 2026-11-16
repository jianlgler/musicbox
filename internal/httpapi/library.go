package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

const (
	maxNameLen  = 500 // characters: no stored artist or album name is longer (tracks_text_lengths)
	maxQueryLen = 100 // characters in a search
)

// TrackReader is what the single-track and cover endpoints need
type TrackReader interface {
	Get(ctx context.Context, id string) (tracks.Track, error)
	CoverHash(ctx context.Context, id string) (string, error)
}

// Library is what the browsing and search endpoints need
type Library interface {
	Artists(ctx context.Context, limit int) ([]tracks.Artist, error)
	Albums(ctx context.Context, artist string, limit int) ([]tracks.Album, error)
	AlbumTracks(ctx context.Context, artist, title string) (tracks.Album, []tracks.Track, error)
	Search(ctx context.Context, query string, limit int) ([]tracks.Track, error)
}

// GET /api/v1/tracks/{id}
func (s *Server) handleGetTrack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		notFound(w, r, "track not found")
		return
	}
	t, err := s.tracks.Get(r.Context(), id)
	if errors.Is(err, tracks.ErrNotFound) {
		notFound(w, r, "track not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"track": t})
}

// GET /api/v1/tracks/{id}/cover: the image bytes, not JSON
func (s *Server) handleTrackCover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		notFound(w, r, "track not found")
		return
	}
	hash, err := s.tracks.CoverHash(r.Context(), id)
	switch {
	case errors.Is(err, tracks.ErrNotFound):
		notFound(w, r, "track not found")
		return
	case errors.Is(err, tracks.ErrNoCover):
		notFound(w, r, "track has no cover")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	key, err := storage.CoverKey(hash)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	obj, err := s.objects.Get(r.Context(), key)
	if err != nil {
		// Missing too: the row points at an object that should exist, so it's our fault, not the client's
		s.internalError(w, r, fmt.Errorf("[COVER] get %s: %w", key, err))
		return
	}
	defer obj.Close()

	// The type comes from the bytes again, not from what was stored next to them: whatever this
	// says is what the browser will use to interpret the response
	body := bufio.NewReader(obj)
	head, _ := body.Peek(8)
	contentType := imageType(head)
	if contentType == "" {
		s.internalError(w, r, fmt.Errorf("[COVER] %s is not a JPEG or PNG", key))
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	// Overrides the middleware's no-store: a track's cover never changes (the file can't, and the
	// cover is read from it). private keeps it out of shared caches; it's still library data.
	h.Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	// Size and type were checked at ingest, and the object is named by its hash
	if _, err := io.Copy(w, body); err != nil {
		log.Printf("[COVER] %s %q - send: %v", r.Method, r.URL.Path, err) // status already sent
	}
}

// GET /api/v1/artists?limit=N
func (s *Server) handleArtists(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	list, err := s.tracks.Artists(r.Context(), limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"artists": list})
}

// GET /api/v1/albums?artist=NAME&limit=N
func (s *Server) handleAlbums(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	artist, ok := textParam(w, r, "artist", false, maxNameLen)
	if !ok {
		return
	}
	list, err := s.tracks.Albums(r.Context(), artist, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"albums": list})
}

// GET /api/v1/albums/tracks?artist=NAME&album=TITLE
//
// Names go in the query string, not the path: they can contain "/" and anything else, and the
// query string has one well-defined encoding for all of it.
func (s *Server) handleAlbumTracks(w http.ResponseWriter, r *http.Request) {
	artist, ok := textParam(w, r, "artist", true, maxNameLen)
	if !ok {
		return
	}
	title, ok := textParam(w, r, "album", true, maxNameLen)
	if !ok {
		return
	}
	album, list, err := s.tracks.AlbumTracks(r.Context(), artist, title)
	if errors.Is(err, tracks.ErrNotFound) {
		notFound(w, r, "album not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"album": album, "tracks": list})
}

// GET /api/v1/search?q=TEXT&limit=N
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	q, ok := textParam(w, r, "q", true, maxQueryLen)
	if !ok {
		return
	}
	list, err := s.tracks.Search(r.Context(), q, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"tracks": list})
}

// parseLimit reads ?limit= (default 50, 1–200). On a bad value it writes the 400 and returns false.
func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	q := r.URL.Query().Get("limit")
	if q == "" {
		return defaultLimit, true
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 || n > maxLimit {
		// %q quotes and escapes the input, so a "\n" in it can't forge an extra log line
		log.Printf("[BAD_REQUEST] %s %q - invalid limit %q", r.Method, r.URL.Path, q)
		badRequest(w, r, fmt.Sprintf("limit must be an integer between 1 and %d", maxLimit))
		return 0, false
	}
	return n, true
}

// textParam reads a text query parameter, trimmed. On a bad value it writes the 400 and returns false.
//
// The checks are what Postgres would otherwise turn into a 500: it rejects invalid UTF-8 and NUL
// in text, and the query string can carry both (%FF, %00) because it's percent-decoded to raw bytes.
// Repeating a parameter (?q=a&q=b) is refused instead of silently picking one.
func textParam(w http.ResponseWriter, r *http.Request, name string, required bool, maxLen int) (string, bool) {
	values := r.URL.Query()[name]
	if len(values) > 1 {
		badRequest(w, r, name+" must be given once")
		return "", false
	}
	v := ""
	if len(values) == 1 {
		v = strings.TrimSpace(values[0])
	}
	switch {
	case !utf8.ValidString(v) || strings.ContainsRune(v, 0):
		log.Printf("[BAD_REQUEST] %s %q - invalid %s %q", r.Method, r.URL.Path, name, v)
		badRequest(w, r, name+" must be valid UTF-8 text")
	case v == "" && required:
		badRequest(w, r, name+" is required")
	case utf8.RuneCountInString(v) > maxLen:
		badRequest(w, r, fmt.Sprintf("%s must be at most %d characters", name, maxLen))
	default:
		return v, true
	}
	return "", false
}

// validUUID checks the canonical 8-4-4-4-12 hex form before the id reaches Postgres, which would
// answer a malformed uuid with an error (a 500) instead of "no such row"
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// imageType names the two formats ingest keeps, from their magic bytes
func imageType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\xFF\xD8\xFF")):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	}
	return ""
}

func badRequest(w http.ResponseWriter, r *http.Request, msg string) {
	writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": msg})
}

func notFound(w http.ResponseWriter, r *http.Request, msg string) {
	writeJSON(w, r, http.StatusNotFound, map[string]string{"error": msg})
}
