package ingest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jianlgler/musicbox/internal/ingest"
	"github.com/jianlgler/musicbox/internal/jobs"
	"github.com/jianlgler/musicbox/internal/probe"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

var flac = "fLaC\x00\x00\x00\x22 the rest of a FLAC file"

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type fakeTracks struct {
	file     tracks.File
	fileErr  error
	setErr   error
	setCalls int
	got      tracks.Metadata
}

func (f *fakeTracks) File(context.Context, string) (tracks.File, error) { return f.file, f.fileErr }

func (f *fakeTracks) SetMetadata(_ context.Context, _ string, m tracks.Metadata) error {
	f.setCalls++
	f.got = m
	return f.setErr
}

type fakeObjects struct {
	objects   map[string]string
	types     map[string]string
	getErr    error
	readErr   error
	existsErr error
	puts      int
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	var r io.Reader = strings.NewReader(body)
	if f.readErr != nil {
		r = io.MultiReader(r, errReader{f.readErr})
	}
	return io.NopCloser(r), nil
}

func (f *fakeObjects) Exists(_ context.Context, key string) (bool, error) {
	_, ok := f.objects[key]
	return ok, f.existsErr
}

func (f *fakeObjects) Put(_ context.Context, key string, body io.Reader, _ int64, contentType string) error {
	f.puts++
	b, _ := io.ReadAll(body)
	f.objects[key], f.types[key] = string(b), contentType
	return nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// fakeProber records the file it was given and what else was in its directory
type fakeProber struct {
	result    probe.Result
	err       error
	content   string
	neighbors []string
	calls     int
}

func (p *fakeProber) Probe(_ context.Context, path string) (probe.Result, error) {
	p.calls++
	b, _ := os.ReadFile(path)
	p.content = string(b)
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		p.neighbors = append(p.neighbors, e.Name())
	}
	return p.result, p.err
}

func ptr[T any](v T) *T { return &v }

// setup: a track row for content, storage holding body under its key
func setup(content, body string) (*fakeTracks, *fakeObjects) {
	f := tracks.File{SHA256: hashOf(content), Size: int64(len(content))}
	return &fakeTracks{file: f}, &fakeObjects{objects: map[string]string{"audio/" + f.SHA256: body}, types: map[string]string{}}
}

func handle(t *testing.T, tr *fakeTracks, obj *fakeObjects, p *fakeProber) error {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	err := ingest.NewHandler(tr, obj, p).Handle(context.Background(), &jobs.Job{ID: 1, TrackID: "t", Attempt: 1, MaxAttempts: 5})
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
	return err
}

func TestHandleSavesMetadataAndCover(t *testing.T) {
	tr, obj := setup(flac, flac)
	cover := "\xFF\xD8\xFF fake jpeg bytes"
	p := &fakeProber{result: probe.Result{Title: ptr("Song"), Artist: ptr("Band"), TrackNumber: ptr(3), DurationMS: 1234, Cover: []byte(cover), CoverType: "image/jpeg"}}

	if err := handle(t, tr, obj, p); err != nil {
		t.Fatal(err)
	}
	if p.content != flac {
		t.Errorf("prober got %q, want the downloaded bytes", p.content)
	}
	if len(p.neighbors) != 1 {
		t.Errorf("the file shares its directory with %v: the parser's sandbox would see them", p.neighbors)
	}
	m := tr.got
	if *m.Title != "Song" || *m.Artist != "Band" || *m.TrackNumber != 3 || m.DurationMS != 1234 {
		t.Errorf("metadata = %+v", m)
	}
	coverKey := "covers/" + hashOf(cover)
	if m.CoverSHA256 == nil || *m.CoverSHA256 != hashOf(cover) || obj.objects[coverKey] != cover || obj.types[coverKey] != "image/jpeg" {
		t.Errorf("cover: hash %v, stored %q as %q", m.CoverSHA256, obj.objects[coverKey], obj.types[coverKey])
	}
}

// Tracks of one album embed the same image: it's stored once
func TestHandleCoverStoredOnce(t *testing.T) {
	tr, obj := setup(flac, flac)
	cover := "\xFF\xD8\xFF same album cover"
	obj.objects["covers/"+hashOf(cover)] = cover
	p := &fakeProber{result: probe.Result{Cover: []byte(cover), CoverType: "image/jpeg"}}

	if err := handle(t, tr, obj, p); err != nil {
		t.Fatal(err)
	}
	if obj.puts != 0 || tr.got.CoverSHA256 == nil {
		t.Errorf("Put called %d times (want 0), cover hash %v", obj.puts, tr.got.CoverSHA256)
	}
}

func TestHandleNoCover(t *testing.T) {
	tr, obj := setup(flac, flac)
	if err := handle(t, tr, obj, &fakeProber{result: probe.Result{CoverRejected: "not a JPEG or PNG"}}); err != nil {
		t.Fatal(err)
	}
	if tr.got.CoverSHA256 != nil || obj.puts != 0 || tr.setCalls != 1 {
		t.Errorf("cover %v, puts %d, SetMetadata calls %d; a rejected cover must not fail the track", tr.got.CoverSHA256, obj.puts, tr.setCalls)
	}
}

func TestHandleErrors(t *testing.T) {
	notAudio := "<!DOCTYPE html><script>"
	cases := []struct {
		name          string
		setup         func() (*fakeTracks, *fakeObjects)
		prober        *fakeProber
		wantPermanent bool
		wantInError   string
		wantProbed    bool
	}{
		{"object missing", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac)
			o.objects = map[string]string{}
			return tr, o
		}, &fakeProber{}, true, "missing", false},
		{"object replaced", func() (*fakeTracks, *fakeObjects) { return setup(flac, strings.Replace(flac, "rest", "best", 1)) }, &fakeProber{}, true, "corrupted", false},
		{"object truncated", func() (*fakeTracks, *fakeObjects) { return setup(flac, flac[:10]) }, &fakeProber{}, true, "want", false},
		{"object larger", func() (*fakeTracks, *fakeObjects) { return setup(flac, flac+strings.Repeat("x", 1<<20)) }, &fakeProber{}, true, "larger", false},
		{"not audio", func() (*fakeTracks, *fakeObjects) { return setup(notAudio, notAudio) }, &fakeProber{}, true, "not a supported audio", false},
		{"track deleted", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac)
			tr.fileErr = tracks.ErrNotFound
			return tr, o
		}, &fakeProber{}, true, "not found", false},
		{"parser: unreadable", func() (*fakeTracks, *fakeObjects) { return setup(flac, flac) }, &fakeProber{err: probe.ErrUnreadable}, true, "readable", true},
		{"parser: timeout", func() (*fakeTracks, *fakeObjects) { return setup(flac, flac) }, &fakeProber{err: probe.ErrTimeout}, true, "timed out", true},
		{"parser: crash", func() (*fakeTracks, *fakeObjects) { return setup(flac, flac) }, &fakeProber{err: fmt.Errorf("%w: signal: killed", probe.ErrCrashed)}, true, "crashed", true},
		{"parser: can't start", func() (*fakeTracks, *fakeObjects) { return setup(flac, flac) }, &fakeProber{err: errors.New("fork/exec: resource temporarily unavailable")}, false, "", true},
		{"database down", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac)
			tr.fileErr = errors.New("connection refused")
			return tr, o
		}, &fakeProber{}, false, "", false},
		{"saving metadata fails", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac)
			tr.setErr = errors.New("connection refused")
			return tr, o
		}, &fakeProber{}, false, "", true},
		{"storage down", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac)
			o.getErr = errors.New("dial tcp: refused")
			return tr, o
		}, &fakeProber{}, false, "", false},
		{"connection drops mid-read", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac[:8])
			o.readErr = errors.New("unexpected EOF from peer")
			return tr, o
		}, &fakeProber{}, false, "", false},
		{"storing the cover fails", func() (*fakeTracks, *fakeObjects) {
			tr, o := setup(flac, flac)
			o.existsErr = errors.New("dial tcp: refused")
			return tr, o
		},
			&fakeProber{result: probe.Result{Cover: []byte("\xFF\xD8\xFF"), CoverType: "image/jpeg"}}, false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, obj := tc.setup()
			err := handle(t, tr, obj, tc.prober)
			if err == nil {
				t.Fatal("expected an error")
			}
			if jobs.IsPermanent(err) != tc.wantPermanent {
				t.Errorf("permanent = %v, want %v (err %v)", jobs.IsPermanent(err), tc.wantPermanent, err)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantInError)
			}
			if probed := tc.prober.calls > 0; probed != tc.wantProbed {
				t.Errorf("parser ran = %v, want %v (never parse bytes that failed verification)", probed, tc.wantProbed)
			}
		})
	}
}
