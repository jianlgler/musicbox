// Package httpapi is the HTTP layer: routes, handlers, validation, JSON. It never writes SQL.
// Turns requests into calls on the data layer and returns JSONs
package httpapi // Consumer

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/jianlgler/musicbox/internal/tracks"
)

// Every list endpoint needs a cap
const (
	defaultLimit = 50
	maxLimit     = 200
)

// The interfaces describe only what the server needs, and are defined here, by the consumer.
// *pgxpool.Pool and *tracks.Store satisfy them implicitly; tests can pass fakes.
type Pinger interface {
	Ping(ctx context.Context) error
}

type TrackLister interface {
	List(ctx context.Context, limit int) ([]tracks.Track, error)
}

// TrackStore is everything the server needs from the data layer
type TrackStore interface {
	TrackLister
	TrackCreator
	TrackReader
	Library
}

type Server struct {
	db      Pinger
	tracks  TrackStore
	objects ObjectStore
	mux     *http.ServeMux
	handler http.Handler // mux wrapped in middleware
}

func New(db Pinger, tracks TrackStore, objects ObjectStore) *Server {
	s := &Server{db: db, tracks: tracks, objects: objects, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /api/v1/tracks", s.handleListTracks)
	s.mux.HandleFunc("POST /api/v1/tracks", s.handleUploadTrack)
	s.mux.HandleFunc("GET /api/v1/tracks/{id}", s.handleGetTrack)
	s.mux.HandleFunc("GET /api/v1/tracks/{id}/cover", s.handleTrackCover)
	s.mux.HandleFunc("GET /api/v1/artists", s.handleArtists)
	s.mux.HandleFunc("GET /api/v1/albums", s.handleAlbums)
	s.mux.HandleFunc("GET /api/v1/albums/tracks", s.handleAlbumTracks)
	s.mux.HandleFunc("GET /api/v1/search", s.handleSearch)

	// Outermost runs first and finishes last: logging sees the final status and the full duration.
	// Wrapped here and not in main, so tests through New cover the middleware too.
	s.handler = logRequests(securityHeaders(s.mux))

	return s
}

// ServeHTTP makes Server an http.Handler, so it can be passed to http.Server or wrapped by middleware
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Liveness
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// Readiness
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// A probe that hangs is worse than one that fails
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.Ping(ctx); err != nil {
		log.Printf("[NOT_READY] database ping failed: %v", err)
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
}

// List tracks, newest first: GET /api/v1/tracks?limit=N
func (s *Server) handleListTracks(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	// r.Context() is cancelled if the client disconnects, so the query stops too
	list, err := s.tracks.List(r.Context(), limit)
	if err != nil {
		// Details go to our logs, the client only gets a generic message
		log.Printf("list tracks: %v", err) // err already carries the [TRACKS] tag
		writeJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"tracks": list})
}

// Helper func
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[WRITE_ERROR] %s %q - %v", r.Method, r.URL.Path, err)
	}
}
