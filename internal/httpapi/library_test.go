package httpapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jianlgler/musicbox/internal/httpapi"
	"github.com/jianlgler/musicbox/internal/tracks"
)

const trackID = "0a1b2c3d-4e5f-6789-abcd-ef0123456789"

var (
	jpeg      = []byte("\xFF\xD8\xFF\xE0 rest of a jpeg")
	png       = []byte("\x89PNG\r\n\x1a\n rest of a png")
	coverHash = strings.Repeat("ab", 32)
)

func TestGetTrack(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		getErr   error
		wantCode int
		wantBody string
		wantCall bool
	}{
		{"found", "/api/v1/tracks/" + trackID, nil, 200, `"id":"` + trackID + `"`, true},
		{"uppercase hex", "/api/v1/tracks/" + strings.ToUpper(trackID), nil, 200, `"track"`, true},
		{"unknown", "/api/v1/tracks/" + trackID, tracks.ErrNotFound, 404, `{"error":"track not found"}`, true},
		{"database down", "/api/v1/tracks/" + trackID, errors.New("connection refused"), 500, `{"error":"internal error"}`, true},
		// Malformed ids never reach the database: Postgres would answer them with an error (500)
		{"not a uuid", "/api/v1/tracks/hello", nil, 404, `{"error":"track not found"}`, false},
		{"one char short", "/api/v1/tracks/" + trackID[:35], nil, 404, `"track not found"`, false},
		{"hyphen moved", "/api/v1/tracks/0a1b2c3d4-e5f-6789-abcd-ef0123456789", nil, 404, `"track not found"`, false},
		{"hyphen replaced", "/api/v1/tracks/0a1b2c3dx4e5fx6789xabcdxef0123456789", nil, 404, `"track not found"`, false},
		{"not hex", "/api/v1/tracks/0a1b2c3d-4e5f-6789-abcd-ef012345678g", nil, 404, `"track not found"`, false},
		{"sql in path", "/api/v1/tracks/'%20OR%201=1--", nil, 404, `"track not found"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTracks{track: tracks.Track{ID: trackID, Status: "ready"}, getErr: tc.getErr}
			rec := do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", tc.path)

			if rec.Code != tc.wantCode || !strings.Contains(body(rec), tc.wantBody) {
				t.Errorf("got %d %s, want %d containing %s", rec.Code, body(rec), tc.wantCode, tc.wantBody)
			}
			if called := fake.gotID != ""; called != tc.wantCall {
				t.Errorf("data layer called = %v (id %q), want %v", called, fake.gotID, tc.wantCall)
			}
		})
	}
}

func TestTrackCover(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image []byte
		want  string
	}{{"jpeg", jpeg, "image/jpeg"}, {"png", png, "image/png"}} {
		t.Run(tc.name, func(t *testing.T) {
			objects := &fakeObjects{objects: map[string][]byte{"covers/" + coverHash: tc.image}}
			srv := httpapi.New(fakeDB{}, &fakeTracks{coverHash: coverHash}, objects)

			rec := do(t, srv, "GET", "/api/v1/tracks/"+trackID+"/cover")

			if rec.Code != 200 || rec.Body.String() != string(tc.image) {
				t.Fatalf("got %d %q, want 200 with the image bytes", rec.Code, rec.Body.String())
			}
			h := rec.Header()
			if h.Get("Content-Type") != tc.want {
				t.Errorf("Content-Type = %q, want %q", h.Get("Content-Type"), tc.want)
			}
			if h.Get("Cache-Control") != "private, max-age=86400" {
				t.Errorf("Cache-Control = %q: covers may be cached, but only privately", h.Get("Cache-Control"))
			}
			// The rest of the security headers still apply
			if h.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", h.Get("X-Content-Type-Options"))
			}
			if strings.Contains(h.Get("ETag")+h.Get("Location"), coverHash) {
				t.Error("the cover's hash leaks in a header")
			}
		})
	}
}

func TestTrackCoverErrors(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		tracks   *fakeTracks
		objects  *fakeObjects
		wantCode int
		wantBody string
	}{
		{"not a uuid", "/api/v1/tracks/x/cover", &fakeTracks{}, &fakeObjects{}, 404, `{"error":"track not found"}`},
		{"unknown track", "", &fakeTracks{getErr: tracks.ErrNotFound}, &fakeObjects{}, 404, `{"error":"track not found"}`},
		{"no cover", "", &fakeTracks{getErr: tracks.ErrNoCover}, &fakeObjects{}, 404, `{"error":"track has no cover"}`},
		{"database down", "", &fakeTracks{getErr: errors.New("refused")}, &fakeObjects{}, 500, `{"error":"internal error"}`},
		{"bad hash in the row", "", &fakeTracks{coverHash: "../audio/x"}, &fakeObjects{}, 500, `{"error":"internal error"}`},
		// The row says there's a cover: a missing object is our inconsistency, not the client's mistake
		{"object missing", "", &fakeTracks{coverHash: coverHash}, &fakeObjects{}, 500, `{"error":"internal error"}`},
		{"storage down", "", &fakeTracks{coverHash: coverHash}, &fakeObjects{getErr: errors.New("dial tcp")}, 500, `{"error":"internal error"}`},
		// Never serve bytes whose type we can't name: as text/html they'd be a stored XSS
		{"object is html", "", &fakeTracks{coverHash: coverHash},
			&fakeObjects{objects: map[string][]byte{"covers/" + coverHash: []byte("<html><script>alert(1)</script>")}}, 500, `{"error":"internal error"}`},
		{"object empty", "", &fakeTracks{coverHash: coverHash},
			&fakeObjects{objects: map[string][]byte{"covers/" + coverHash: {}}}, 500, `{"error":"internal error"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/api/v1/tracks/" + trackID + "/cover"
			}
			rec := do(t, httpapi.New(fakeDB{}, tc.tracks, tc.objects), "GET", path)

			if rec.Code != tc.wantCode || body(rec) != tc.wantBody {
				t.Errorf("got %d %s, want %d %s", rec.Code, body(rec), tc.wantCode, tc.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json for an error", ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q: an error must not be cached", cc)
			}
		})
	}
}

func TestArtistsAndAlbums(t *testing.T) {
	year := 1999
	fake := &fakeTracks{
		artists: []tracks.Artist{{Name: "Band", AlbumCount: 2, TrackCount: 20}},
		albums:  []tracks.Album{{Title: "Debut", Artist: "Band", Year: &year, TrackCount: 10, DurationMS: 2400000}},
	}
	srv := httpapi.New(fakeDB{}, fake, &fakeObjects{})

	rec := do(t, srv, "GET", "/api/v1/artists?limit=10")
	if want := `{"artists":[{"name":"Band","album_count":2,"track_count":20}]}`; rec.Code != 200 || body(rec) != want {
		t.Errorf("artists: got %d %s, want %s", rec.Code, body(rec), want)
	}
	if fake.gotLimit != 10 {
		t.Errorf("Artists limit = %d, want 10", fake.gotLimit)
	}

	// The name is percent-decoded and trimmed; "/" and "&" are just characters inside it
	rec = do(t, srv, "GET", "/api/v1/albums?artist=%20AC%2FDC%20%26%20Co%20")
	want := `{"albums":[{"title":"Debut","artist":"Band","year":1999,"track_count":10,"duration_ms":2400000,"cover_track_id":null}]}`
	if rec.Code != 200 || body(rec) != want {
		t.Errorf("albums: got %d %s, want %s", rec.Code, body(rec), want)
	}
	if fake.gotArtist != "AC/DC & Co" || fake.gotLimit != 50 {
		t.Errorf("Albums called with artist %q limit %d, want %q and 50", fake.gotArtist, fake.gotLimit, "AC/DC & Co")
	}

	// No artist: all albums
	fake.gotArtist = "unset"
	if rec = do(t, srv, "GET", "/api/v1/albums"); rec.Code != 200 || fake.gotArtist != "" {
		t.Errorf("got %d, artist %q; want 200 and no filter", rec.Code, fake.gotArtist)
	}
}

func TestLibraryEmpty(t *testing.T) {
	// The real store returns empty slices; [] not null is checked against it in Step 7.
	// Here: the handler passes them through as they are.
	srv := httpapi.New(fakeDB{}, &fakeTracks{artists: []tracks.Artist{}, albums: []tracks.Album{}, list: []tracks.Track{}}, &fakeObjects{})
	for path, want := range map[string]string{
		"/api/v1/artists":     `{"artists":[]}`,
		"/api/v1/albums":      `{"albums":[]}`,
		"/api/v1/search?q=zz": `{"tracks":[]}`,
	} {
		if rec := do(t, srv, "GET", path); body(rec) != want {
			t.Errorf("%s = %s, want %s", path, body(rec), want)
		}
	}
}

func TestAlbumTracks(t *testing.T) {
	title := "Intro"
	fake := &fakeTracks{
		album: tracks.Album{Title: "Debut", Artist: "Band", TrackCount: 1},
		list:  []tracks.Track{{ID: trackID, Title: &title, Status: "ready"}},
	}
	rec := do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", "/api/v1/albums/tracks?artist=Band&album=Debut")

	if rec.Code != 200 || !strings.HasPrefix(body(rec), `{"album":{"title":"Debut","artist":"Band"`) || !strings.Contains(body(rec), `"tracks":[{"id":"`+trackID) {
		t.Errorf("got %d %s", rec.Code, body(rec))
	}
	if fake.gotArtist != "Band" || fake.gotAlbum != "Debut" {
		t.Errorf("AlbumTracks(%q, %q), want (Band, Debut)", fake.gotArtist, fake.gotAlbum)
	}

	fake.libErr = tracks.ErrNotFound
	rec = do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", "/api/v1/albums/tracks?artist=Band&album=Nope")
	if rec.Code != 404 || body(rec) != `{"error":"album not found"}` {
		t.Errorf("unknown album: got %d %s", rec.Code, body(rec))
	}
}

func TestSearch(t *testing.T) {
	fake := &fakeTracks{list: []tracks.Track{}}
	rec := do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", "/api/v1/search?q=%20100%25%20pure%20&limit=5")

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, body(rec))
	}
	// The handler passes the text as typed; escaping LIKE wildcards is the store's job
	if fake.gotQuery != "100% pure" || fake.gotLimit != 5 {
		t.Errorf("Search(%q, %d), want (%q, 5)", fake.gotQuery, fake.gotLimit, "100% pure")
	}
}

// Every bad parameter is a 400 that never reaches the data layer
func TestLibraryBadParameters(t *testing.T) {
	long := strings.Repeat("é", 501) // 501 characters, 1002 bytes: the limit counts characters
	cases := []struct{ name, path, wantErr string }{
		{"search without q", "/api/v1/search", "q is required"},
		{"search blank q", "/api/v1/search?q=%20%20", "q is required"},
		{"search q too long", "/api/v1/search?q=" + strings.Repeat("a", 101), "q must be at most 100 characters"},
		{"search q twice", "/api/v1/search?q=a&q=b", "q must be given once"},
		{"search NUL", "/api/v1/search?q=a%00b", "q must be valid UTF-8 text"},
		{"search invalid UTF-8", "/api/v1/search?q=%FF%FE", "q must be valid UTF-8 text"},
		{"search bad limit", "/api/v1/search?q=a&limit=0", "limit must be an integer between 1 and 200"},
		{"artists bad limit", "/api/v1/artists?limit=1000", "limit must be an integer between 1 and 200"},
		{"albums bad limit", "/api/v1/albums?limit=x", "limit must be an integer between 1 and 200"},
		{"albums artist too long", "/api/v1/albums?artist=" + long, "artist must be at most 500 characters"},
		{"albums artist NUL", "/api/v1/albums?artist=%00", "artist must be valid UTF-8 text"},
		{"album without album", "/api/v1/albums/tracks?artist=Band", "album is required"},
		{"album without artist", "/api/v1/albums/tracks?album=Debut", "artist is required"},
		{"album title too long", "/api/v1/albums/tracks?artist=Band&album=" + long, "album must be at most 500 characters"},
		{"album invalid UTF-8", "/api/v1/albums/tracks?artist=%C3&album=Debut", "artist must be valid UTF-8 text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTracks{}
			rec := do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", tc.path)

			if want := `{"error":"` + tc.wantErr + `"}`; rec.Code != 400 || body(rec) != want {
				t.Errorf("got %d %s, want 400 %s", rec.Code, body(rec), want)
			}
			if fake.libCalls != 0 {
				t.Errorf("data layer called %d times, want 0", fake.libCalls)
			}
		})
	}
}

func TestLibraryNameAtLimit(t *testing.T) {
	fake := &fakeTracks{albums: []tracks.Album{}}
	name := strings.Repeat("é", 500)
	if rec := do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", "/api/v1/albums?artist="+name); rec.Code != 200 || fake.gotArtist != name {
		t.Errorf("a 500-character name: got %d, want 200 and the name passed through", rec.Code)
	}
}

func TestLibraryDatabaseError(t *testing.T) {
	for _, path := range []string{"/api/v1/artists", "/api/v1/albums", "/api/v1/albums/tracks?artist=a&album=b", "/api/v1/search?q=a"} {
		fake := &fakeTracks{libErr: errors.New("connect 10.0.0.5 user=admin: boom")}
		rec := do(t, httpapi.New(fakeDB{}, fake, &fakeObjects{}), "GET", path)
		if rec.Code != http.StatusInternalServerError || body(rec) != `{"error":"internal error"}` {
			t.Errorf("%s: got %d %s, want 500 with a generic body", path, rec.Code, body(rec))
		}
	}
}
