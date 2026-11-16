// Package ingest is the work done for each uploaded track, in the background:
//  1. download the object and verify it (still there, same size, same SHA-256, audio header)
//  2. parse tags, properties and cover art in an isolated child process (internal/probe)
//  3. store the cover under covers/<sha256>, then save the metadata
//
// Every step is idempotent (same file, same values, same keys), as at-least-once delivery requires.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/jianlgler/musicbox/internal/audio"
	"github.com/jianlgler/musicbox/internal/jobs"
	"github.com/jianlgler/musicbox/internal/probe"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

// TrackFiles is what ingest needs from the data layer. *tracks.Store satisfies it.
type TrackFiles interface {
	File(ctx context.Context, trackID string) (tracks.File, error)
	SetMetadata(ctx context.Context, trackID string, m tracks.Metadata) error
}

// Objects is what ingest needs from object storage. *storage.Store satisfies it.
type Objects interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Exists(ctx context.Context, key string) (bool, error)
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
}

// Prober parses one file. probe.Isolated satisfies it; tests use a fake.
type Prober interface {
	Probe(ctx context.Context, path string) (probe.Result, error)
}

type Handler struct {
	tracks  TrackFiles
	objects Objects
	prober  Prober
}

func NewHandler(t TrackFiles, o Objects, p Prober) *Handler {
	return &Handler{tracks: t, objects: o, prober: p}
}

// Handle runs one ingest job. Errors wrapped in jobs.Permanent fail the job at once; any other
// error is retried with backoff.
func (h *Handler) Handle(ctx context.Context, j *jobs.Job) error {
	f, err := h.tracks.File(ctx, j.TrackID)
	if errors.Is(err, tracks.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err // database trouble: retry
	}

	// A directory of its own, 0700: the parser's sandbox can see the whole directory the file
	// is in, so nothing else may be in it, and other users on the machine can't read it
	dir, err := os.MkdirTemp("", "musicbox-ingest-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "audio")

	if err := h.download(ctx, f, path); err != nil {
		return err
	}

	res, err := h.prober.Probe(ctx, path)
	switch {
	case errors.Is(err, probe.ErrUnreadable), errors.Is(err, probe.ErrTimeout), errors.Is(err, probe.ErrCrashed):
		return jobs.Permanent(err) // the same bytes would do the same thing again
	case err != nil:
		return fmt.Errorf("probe: %w", err) // e.g. couldn't start the child: retry
	}

	m := tracks.Metadata{
		Title: res.Title, Artist: res.Artist, Album: res.Album, AlbumArtist: res.AlbumArtist,
		TrackNumber: res.TrackNumber, DiscNumber: res.DiscNumber, Year: res.Year, DurationMS: res.DurationMS,
	}
	if res.CoverRejected != "" {
		log.Printf("[INGEST] track %s: embedded cover ignored: %q", j.TrackID, res.CoverRejected)
	}
	if res.Cover != nil {
		sum, err := h.storeCover(ctx, res.Cover, res.CoverType)
		if err != nil {
			return err // storage trouble: retry
		}
		m.CoverSHA256 = &sum
	}

	if err := h.tracks.SetMetadata(ctx, j.TrackID, m); err != nil {
		if errors.Is(err, tracks.ErrNotFound) {
			return jobs.Permanent(err)
		}
		return err
	}
	return nil
}

// download copies the track's object to path, checking it against the row as it goes
func (h *Handler) download(ctx context.Context, f tracks.File, path string) error {
	key, err := storage.AudioKey(f.SHA256)
	if err != nil {
		return jobs.Permanent(err) // can't happen: the column has the same CHECK
	}
	body, err := h.objects.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		// The upload stores the object before the row, so this means it was deleted from storage
		return jobs.Permanent(fmt.Errorf("object %s is missing", key))
	}
	if err != nil {
		return err // storage unreachable: retry
	}
	defer body.Close()

	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer out.Close()

	// Never read more than the expected size (+1 to notice that there is more): an object
	// replaced by something huge can't fill the disk
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(body, f.Size+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", key, err) // connection dropped or disk full: retry
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	// Content-addressed storage can check itself: the key says what the bytes must hash to.
	// A mismatch means the object was corrupted or replaced; retrying won't fix it.
	if size > f.Size {
		return jobs.Permanent(fmt.Errorf("object %s is larger than the %d bytes uploaded", key, f.Size))
	}
	if size < f.Size {
		return jobs.Permanent(fmt.Errorf("object %s has %d bytes, want %d", key, size, f.Size))
	}
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != f.SHA256 {
		return jobs.Permanent(fmt.Errorf("object %s hashes to %s: corrupted or replaced in storage", key, sum))
	}

	head := make([]byte, audio.SniffLen)
	in, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reopen %s: %w", path, err)
	}
	defer in.Close()
	n, _ := io.ReadFull(in, head)
	if _, ok := audio.Detect(head[:n]); !ok {
		return jobs.Permanent(fmt.Errorf("object %s is not a supported audio file", key))
	}
	return nil
}

// storeCover stores img under covers/<sha256> unless it's already there, and returns the hash
func (h *Handler) storeCover(ctx context.Context, img []byte, mimeType string) (string, error) {
	digest := sha256.Sum256(img)
	sum := hex.EncodeToString(digest[:])
	key, err := storage.CoverKey(sum)
	if err != nil {
		return "", jobs.Permanent(err)
	}
	exists, err := h.objects.Exists(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := h.objects.Put(ctx, key, bytes.NewReader(img), int64(len(img)), mimeType); err != nil {
			return "", err
		}
	}
	return sum, nil
}
