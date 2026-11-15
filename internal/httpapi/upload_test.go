package httpapi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jianlgler/musicbox/internal/httpapi"
)

const maxUpload = 200 << 20

var flacFile = []byte("fLaC\x00\x00\x00\x22 pretend this is the rest of a FLAC file")

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// upload sends body as POST /api/v1/tracks. TMPDIR points at an empty directory, returned so the
// test can check the temp file was removed.
func upload(t *testing.T, srv http.Handler, body io.Reader) (*httptest.ResponseRecorder, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	req := httptest.NewRequest("POST", "/api/v1/tracks", body)
	req.Header.Set("Content-Type", "text/html") // a lie: the server must ignore it
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec, tmp
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("temp file left behind: %s", e.Name())
	}
}

func TestUploadNewTrack(t *testing.T) {
	tr := &fakeTracks{created: true}
	obj := &fakeObjects{}
	srv := httpapi.New(fakeDB{}, tr, obj)

	rec, tmp := upload(t, srv, strings.NewReader(string(flacFile)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, body(rec))
	}
	key := "audio/" + hashOf(flacFile)
	if got := obj.objects[key]; string(got) != string(flacFile) {
		t.Errorf("stored %q under %s, want the uploaded bytes", got, key)
	}
	if got := obj.types[key]; got != "audio/flac" {
		t.Errorf("stored Content-Type %q, want audio/flac (detected, not the client's text/html)", got)
	}
	if tr.gotSHA256 != hashOf(flacFile) || tr.gotSize != int64(len(flacFile)) {
		t.Errorf("Create(%s, %d), want the server-computed hash and real size", tr.gotSHA256, tr.gotSize)
	}
	b := body(rec)
	if !strings.Contains(b, `"track":{"id":"11111111-2222-3333-4444-555555555555"`) || !strings.Contains(b, `"status":"pending"`) {
		t.Errorf("body = %s", b)
	}
	if strings.Contains(b, "sha256") || strings.Contains(b, hashOf(flacFile)) {
		t.Errorf("body exposes the hash: %s", b)
	}
	assertNoTempFiles(t, tmp)
}

func TestUploadSameFileTwice(t *testing.T) {
	obj := &fakeObjects{}
	srv := httpapi.New(fakeDB{}, &fakeTracks{created: true}, obj)
	upload(t, srv, strings.NewReader(string(flacFile)))

	// Second time: the object exists and the data layer reports an existing track
	srv = httpapi.New(fakeDB{}, &fakeTracks{created: false}, obj)
	rec, tmp := upload(t, srv, strings.NewReader(string(flacFile)))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a duplicate", rec.Code)
	}
	if obj.puts != 1 {
		t.Errorf("Put called %d times, want 1: identical bytes must not be uploaded again", obj.puts)
	}
	assertNoTempFiles(t, tmp)
}

// A crash after Put but before the row leaves an orphan object. Re-uploading must still create the row.
func TestUploadOrphanObject(t *testing.T) {
	obj := &fakeObjects{objects: map[string][]byte{"audio/" + hashOf(flacFile): flacFile}, types: map[string]string{}}
	tr := &fakeTracks{created: true}
	srv := httpapi.New(fakeDB{}, tr, obj)

	rec, _ := upload(t, srv, strings.NewReader(string(flacFile)))

	if rec.Code != http.StatusCreated || tr.createCalls != 1 || obj.puts != 0 {
		t.Errorf("status %d, Create calls %d, Put calls %d; want 201, 1, 0", rec.Code, tr.createCalls, obj.puts)
	}
}

// Rejected uploads must never reach storage or the database
func TestUploadRejected(t *testing.T) {
	cases := []struct {
		name     string
		body     io.Reader
		wantCode int
	}{
		{"empty", strings.NewReader(""), http.StatusBadRequest},
		{"html", strings.NewReader("<!DOCTYPE html><script>alert(1)</script>"), http.StatusUnsupportedMediaType},
		{"executable", strings.NewReader("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00"), http.StatusUnsupportedMediaType},
		{"multipart form", strings.NewReader("--boundary\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nfLaC"), http.StatusUnsupportedMediaType},
		// Chunked (no Content-Length): only the limit while reading catches it
		{"too large, chunked", io.MultiReader(strings.NewReader("fLaC"), io.LimitReader(zeros{}, maxUpload)), http.StatusRequestEntityTooLarge},
		{"client disconnects", io.MultiReader(strings.NewReader("fLaC\x00\x00\x00\x22"), failingReader{}), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, obj := &fakeTracks{created: true}, &fakeObjects{}
			srv := httpapi.New(fakeDB{}, tr, obj)

			rec, tmp := upload(t, srv, tc.body)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.wantCode, body(rec))
			}
			if obj.puts != 0 || tr.createCalls != 0 {
				t.Errorf("Put called %d times, Create %d times; want neither", obj.puts, tr.createCalls)
			}
			assertNoTempFiles(t, tmp)
		})
	}
}

// A Content-Length over the limit is refused before a single byte is read
func TestUploadTooLargeContentLength(t *testing.T) {
	r := &countingReader{}
	srv := httpapi.New(fakeDB{}, &fakeTracks{}, &fakeObjects{})
	req := httptest.NewRequest("POST", "/api/v1/tracks", r)
	req.ContentLength = maxUpload + 1
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if r.n != 0 {
		t.Errorf("read %d bytes of a body that was announced as too large", r.n)
	}
}

// Every server-side failure is a generic 500, and a failed Put never creates a row
func TestUploadServerErrors(t *testing.T) {
	secret := errors.New("dial tcp 10.0.0.5:8333: SignatureDoesNotMatch for AKIDEXAMPLE")
	cases := []struct {
		name          string
		tracks        *fakeTracks
		objects       *fakeObjects
		wantCreateRun bool
	}{
		{"storage head fails", &fakeTracks{created: true}, &fakeObjects{existsErr: secret}, false},
		{"storage put fails", &fakeTracks{created: true}, &fakeObjects{putErr: secret}, false},
		{"database fails", &fakeTracks{createErr: secret}, &fakeObjects{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httpapi.New(fakeDB{}, tc.tracks, tc.objects)

			rec, tmp := upload(t, srv, strings.NewReader(string(flacFile)))

			if rec.Code != http.StatusInternalServerError || body(rec) != `{"error":"internal error"}` {
				t.Errorf("got %d %s, want 500 with the generic body", rec.Code, body(rec))
			}
			if ran := tc.tracks.createCalls > 0; ran != tc.wantCreateRun {
				t.Errorf("Create called = %v, want %v (no row unless the object is stored)", ran, tc.wantCreateRun)
			}
			assertNoTempFiles(t, tmp)
		})
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}
