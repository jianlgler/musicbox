// Black-box tests: package httpapi_test only sees what's exported (New and the interfaces),
// so the tests exercise the API the way a client does and survive internal renames.
package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jianlgler/musicbox/internal/httpapi"
	"github.com/jianlgler/musicbox/internal/tracks"
)

// fakeDB satisfies httpapi.Pinger
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

// fakeTracks satisfies httpapi.TrackLister. Pointer receiver, so the test can read what List recorded.
type fakeTracks struct {
	list     []tracks.Track
	err      error
	called   bool
	gotLimit int
}

func (f *fakeTracks) List(_ context.Context, limit int) ([]tracks.Track, error) {
	f.called, f.gotLimit = true, limit
	return f.list, f.err
}

// do sends one request through the whole server (middleware + mux + handler), in memory
func do(t *testing.T, srv http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper() // Failures point at the caller's line, not this one
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// body returns the response body without json.Encoder's trailing newline
func body(rec *httptest.ResponseRecorder) string {
	return strings.TrimSpace(rec.Body.String())
}

func TestHealthz(t *testing.T) {
	// The DB is down, liveness must not care
	srv := httpapi.New(fakeDB{err: errors.New("db down")}, &fakeTracks{})

	rec := do(t, srv, "GET", "/healthz")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got, want := body(rec), `{"status":"ok"}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name     string
		dbErr    error
		wantCode int
		wantBody string
	}{
		{"db up", nil, http.StatusOK, `{"status":"ready"}`},
		{"db down", errors.New("connection refused"), http.StatusServiceUnavailable, `{"status":"unavailable"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httpapi.New(fakeDB{err: tc.dbErr}, &fakeTracks{})

			rec := do(t, srv, "GET", "/readyz")

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if got := body(rec); got != tc.wantBody {
				t.Errorf("body = %s, want %s", got, tc.wantBody)
			}
		})
	}
}

func TestListTracksLimit(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantCode  int
		wantLimit int // 0 = List must not be called
	}{
		{"default", "", http.StatusOK, 50},
		{"min", "?limit=1", http.StatusOK, 1},
		{"max", "?limit=200", http.StatusOK, 200},
		{"zero", "?limit=0", http.StatusBadRequest, 0},
		{"above max", "?limit=201", http.StatusBadRequest, 0},
		{"negative", "?limit=-5", http.StatusBadRequest, 0},
		{"not a number", "?limit=abc", http.StatusBadRequest, 0},
		{"decimal", "?limit=1.5", http.StatusBadRequest, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTracks{list: []tracks.Track{}}
			srv := httpapi.New(fakeDB{}, fake)

			rec := do(t, srv, "GET", "/api/v1/tracks"+tc.query)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantLimit == 0 {
				// Negative check: an invalid request must never reach the data layer
				if fake.called {
					t.Errorf("List was called with limit %d, want no call", fake.gotLimit)
				}
				if got, want := body(rec), `{"error":"limit must be an integer between 1 and 200"}`; got != want {
					t.Errorf("body = %s, want %s", got, want)
				}
				return
			}
			if fake.gotLimit != tc.wantLimit {
				t.Errorf("List called with limit %d, want %d", fake.gotLimit, tc.wantLimit)
			}
		})
	}
}

func TestListTracksDatabaseError(t *testing.T) {
	// An error full of internals that must never reach the client
	fake := &fakeTracks{err: errors.New("connect 10.0.0.5 user=admin: boom")}
	srv := httpapi.New(fakeDB{}, fake)

	rec := do(t, srv, "GET", "/api/v1/tracks")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got, want := body(rec), `{"error":"internal error"}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	// Negative checks: no internal detail leaks
	for _, secret := range []string{"10.0.0.5", "admin", "boom"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("body leaks %q: %s", secret, rec.Body.String())
		}
	}
}

func TestListTracksEmpty(t *testing.T) {
	srv := httpapi.New(fakeDB{}, &fakeTracks{list: []tracks.Track{}})

	rec := do(t, srv, "GET", "/api/v1/tracks")

	// [] not null: clients loop over tracks
	if got, want := body(rec), `{"tracks":[]}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestListTracksBody(t *testing.T) {
	title := "First"
	fake := &fakeTracks{list: []tracks.Track{
		{ID: "id-1", Title: &title, Status: "ready", SizeBytes: 1000},
	}}
	srv := httpapi.New(fakeDB{}, fake)

	rec := do(t, srv, "GET", "/api/v1/tracks")

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	b := body(rec)
	for _, want := range []string{`"id":"id-1"`, `"title":"First"`, `"artist":null`, `"size_bytes":1000`} {
		if !strings.Contains(b, want) {
			t.Errorf("body missing %s: %s", want, b)
		}
	}
	// sha256 is deliberately not part of the API
	if strings.Contains(b, "sha256") {
		t.Errorf("body exposes sha256: %s", b)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := httpapi.New(fakeDB{}, &fakeTracks{})

	rec := do(t, srv, "POST", "/api/v1/tracks")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "frame-ancestors 'none'",
	}
	// A defined route, a rejected method and an unknown path: middleware must cover all of them
	for _, target := range []struct{ method, path string }{
		{"GET", "/healthz"},
		{"POST", "/api/v1/tracks"},
		{"GET", "/nope"},
	} {
		t.Run(target.method+" "+target.path, func(t *testing.T) {
			srv := httpapi.New(fakeDB{}, &fakeTracks{list: []tracks.Track{}})

			rec := do(t, srv, target.method, target.path)

			for header, value := range want {
				if got := rec.Header().Get(header); got != value {
					t.Errorf("%s = %q, want %q", header, got, value)
				}
			}
		})
	}
}
