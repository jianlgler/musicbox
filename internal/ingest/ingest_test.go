package ingest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jianlgler/musicbox/internal/ingest"
	"github.com/jianlgler/musicbox/internal/jobs"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

var flac = "fLaC\x00\x00\x00\x22 the rest of a FLAC file"

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type fakeTracks struct {
	file tracks.File
	err  error
}

func (f fakeTracks) File(context.Context, string) (tracks.File, error) { return f.file, f.err }

type fakeObjects struct {
	objects map[string]string
	err     error
	readErr error
}

func (f fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
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

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// stored returns a track row and storage holding body under the track's key
func stored(rowContent, body string) (fakeTracks, fakeObjects) {
	f := tracks.File{SHA256: hashOf(rowContent), Size: int64(len(rowContent))}
	return fakeTracks{file: f}, fakeObjects{objects: map[string]string{"audio/" + f.SHA256: body}}
}

func TestHandle(t *testing.T) {
	notAudio := "<!DOCTYPE html><script>"
	cases := []struct {
		name          string
		tracks        fakeTracks
		objects       fakeObjects
		wantErr       bool
		wantPermanent bool
		wantInError   string
	}{
		{name: "intact", tracks: first(stored(flac, flac)), objects: second(stored(flac, flac))},
		{"object missing", fakeTracks{file: tracks.File{SHA256: hashOf(flac), Size: int64(len(flac))}}, fakeObjects{objects: map[string]string{}}, true, true, "missing"},
		{"object replaced", first(stored(flac, flac)), fakeObjects{objects: map[string]string{"audio/" + hashOf(flac): strings.Replace(flac, "rest", "best", 1)}}, true, true, "corrupted"},
		{"object truncated", first(stored(flac, flac)), fakeObjects{objects: map[string]string{"audio/" + hashOf(flac): flac[:10]}}, true, true, "want"},
		{"object larger", first(stored(flac, flac)), fakeObjects{objects: map[string]string{"audio/" + hashOf(flac): flac + strings.Repeat("x", 1<<20)}}, true, true, "larger"},
		{"not audio", first(stored(notAudio, notAudio)), second(stored(notAudio, notAudio)), true, true, "not a supported audio"},
		{"track deleted", fakeTracks{err: tracks.ErrNotFound}, fakeObjects{}, true, true, "not found"},
		{"database down", fakeTracks{err: errors.New("connection refused")}, fakeObjects{}, true, false, ""},
		{"storage down", first(stored(flac, flac)), fakeObjects{err: errors.New("dial tcp: connection refused")}, true, false, ""},
		{"connection drops mid-read", first(stored(flac, flac)), fakeObjects{objects: second(stored(flac, flac[:8])).objects, readErr: errors.New("unexpected EOF from peer")}, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ingest.NewHandler(tc.tracks, tc.objects)
			err := h.Handle(context.Background(), &jobs.Job{ID: 1, TrackID: "t", Attempt: 1, MaxAttempts: 5})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if jobs.IsPermanent(err) != tc.wantPermanent {
				t.Errorf("permanent = %v, want %v (err %v)", jobs.IsPermanent(err), tc.wantPermanent, err)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantInError)
			}
		})
	}
}

func first(t fakeTracks, _ fakeObjects) fakeTracks   { return t }
func second(_ fakeTracks, o fakeObjects) fakeObjects { return o }
