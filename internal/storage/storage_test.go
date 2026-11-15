package storage_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jianlgler/musicbox/internal/storage"
)

const (
	testAccessKey = "AKIDTESTTESTTESTTEST"
	testSecretKey = "supersecretsupersecretsupersecretsecret0"
	testHash      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// fakeS3 records each request and answers with whatever handle returns
type fakeS3 struct {
	requests []*http.Request
	bodies   []string
	handle   func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, string(body))
	f.handle(w, r)
}

func newStore(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*storage.Store, *fakeS3) {
	t.Helper()
	fake := &fakeS3{handle: handle}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	// localhost, not 127.0.0.1: with an IP the SDK falls back to path style on its own,
	// so only a hostname shows that UsePathStyle is really set
	endpoint := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	s, err := storage.New(storage.Config{Endpoint: endpoint, Bucket: "musicbox", AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, fake
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestNewValidatesConfig(t *testing.T) {
	good := storage.Config{Endpoint: "http://127.0.0.1:8333", Bucket: "musicbox", AccessKey: testAccessKey, SecretKey: testSecretKey}
	cases := map[string]func(c *storage.Config){
		"missing endpoint": func(c *storage.Config) { c.Endpoint = "" },
		"missing bucket":   func(c *storage.Config) { c.Bucket = "" },
		"missing key":      func(c *storage.Config) { c.AccessKey = "" },
		"missing secret":   func(c *storage.Config) { c.SecretKey = "" },
		"no scheme":        func(c *storage.Config) { c.Endpoint = "127.0.0.1:8333" },
		"ftp scheme":       func(c *storage.Config) { c.Endpoint = "ftp://127.0.0.1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := good
			mutate(&c)
			_, err := storage.New(c)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), c.SecretKey) && c.SecretKey != "" {
				t.Errorf("error leaks the secret key: %v", err)
			}
		})
	}
	if _, err := storage.New(good); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestAudioKey(t *testing.T) {
	key, err := storage.AudioKey(testHash)
	if err != nil || key != "audio/"+testHash {
		t.Fatalf("AudioKey(valid) = %q, %v", key, err)
	}
	bad := []string{
		"",
		strings.ToUpper(testHash), // uppercase: two spellings of one hash would be two keys
		testHash[:63],
		testHash + "0",
		"../../" + testHash[6:],
		testHash[:62] + "/x",
		testHash[:63] + "\n",
	}
	for _, in := range bad {
		if key, err := storage.AudioKey(in); err == nil {
			t.Errorf("AudioKey(%q) = %q, want an error", in, key)
		}
	}
}

// The request must be path style, signed with SigV4 for our key, and never carry the secret
func TestPutSignsPathStyleRequest(t *testing.T) {
	s, fake := newStore(t, ok)
	key, _ := storage.AudioKey(testHash)

	if err := s.Put(context.Background(), key, strings.NewReader("hello"), 5, "audio/mpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(fake.requests))
	}
	r := fake.requests[0]
	if r.Method != http.MethodPut || r.URL.Path != "/musicbox/"+key {
		t.Errorf("got %s %s, want PUT /musicbox/%s", r.Method, r.URL.Path, key)
	}
	if got := r.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q", got)
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+testAccessKey+"/") {
		t.Errorf("not a SigV4 signature for our key: %q", auth)
	}
	if !strings.Contains(fake.bodies[0], "hello") {
		t.Errorf("body %q doesn't contain the payload", fake.bodies[0])
	}
	for name, values := range r.Header {
		for _, v := range values {
			if strings.Contains(v, testSecretKey) {
				t.Errorf("header %s carries the secret key", name)
			}
		}
	}
	if strings.Contains(r.URL.String(), testSecretKey) || strings.Contains(fake.bodies[0], testSecretKey) {
		t.Error("secret key sent in the URL or body")
	}
}

func TestExists(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{"present", http.StatusOK, true, false},
		{"missing", http.StatusNotFound, false, false},
		{"forbidden", http.StatusForbidden, false, true}, // must not be mistaken for "missing"
		{"server error", http.StatusInternalServerError, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newStore(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
			got, err := s.Exists(context.Background(), "audio/"+testHash)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("Exists = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
			if r := fake.requests[0]; r.Method != http.MethodHead {
				t.Errorf("method = %s, want HEAD (no download)", r.Method)
			}
		})
	}
}

func TestGet(t *testing.T) {
	s, _ := newStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	})
	body, err := s.Get(context.Background(), "audio/"+testHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "hello" {
		t.Errorf("body = %q", got)
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
	})
	_, err := s.Get(context.Background(), "audio/"+testHash)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteAndPing(t *testing.T) {
	s, fake := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := s.Delete(context.Background(), "audio/"+testHash); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if r := fake.requests[1]; r.Method != http.MethodHead || r.URL.Path != "/musicbox" {
		t.Errorf("Ping sent %s %s, want HEAD /musicbox", r.Method, r.URL.Path)
	}
}

func TestPingFailsOnWrongKey(t *testing.T) {
	s, _ := newStore(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	err := s.Ping(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testSecretKey) {
		t.Errorf("error leaks the secret key: %v", err)
	}
}
