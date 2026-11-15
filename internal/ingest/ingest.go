// Package ingest is the work done for each uploaded track, in the background.
//
// Step 4: verify the stored object (still there, same size, same SHA-256, still an audio header).
// Step 5 adds reading tags and cover art in the same pass over the bytes.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/jianlgler/musicbox/internal/audio"
	"github.com/jianlgler/musicbox/internal/jobs"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

// TrackFiles is what ingest needs from the data layer. *tracks.Store satisfies it.
type TrackFiles interface {
	File(ctx context.Context, trackID string) (tracks.File, error)
}

// Objects is what ingest needs from object storage. *storage.Store satisfies it.
type Objects interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

type Handler struct {
	tracks  TrackFiles
	objects Objects
}

func NewHandler(t TrackFiles, o Objects) *Handler {
	return &Handler{tracks: t, objects: o}
}

// Handle is idempotent: it only reads, so running it twice gives the same answer.
// Errors wrapped in jobs.Permanent fail the job at once; any other error is retried with backoff.
func (h *Handler) Handle(ctx context.Context, j *jobs.Job) error {
	f, err := h.tracks.File(ctx, j.TrackID)
	if errors.Is(err, tracks.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err // database trouble: retry
	}
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

	// Never read more than the size we expect (+1 to notice that there is more):
	// an object replaced in storage by something huge can't make the worker read forever.
	// TeeReader: every byte read, header included, also goes through the hash, in order.
	hash := sha256.New()
	r := io.TeeReader(io.LimitReader(body, f.Size+1), hash)

	head := make([]byte, audio.SniffLen)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read %s: %w", key, err) // connection dropped: retry
	}
	rest, err := io.Copy(io.Discard, r)
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	size := int64(n) + rest

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
	if _, ok := audio.Detect(head[:n]); !ok {
		return jobs.Permanent(fmt.Errorf("object %s is not a supported audio file", key))
	}
	return nil
}
