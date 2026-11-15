package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jianlgler/musicbox/internal/audio"
	"github.com/jianlgler/musicbox/internal/storage"
	"github.com/jianlgler/musicbox/internal/tracks"
)

const (
	// A 10-minute FLAC is ~60 MB; 200 MiB leaves room without letting one request fill the disk
	maxUploadBytes = 200 << 20
	// The server's ReadTimeout is short for every other route; an upload gets this much instead.
	// 200 MiB in 10 minutes needs ~350 KB/s, slower clients should split or retry.
	uploadReadTimeout = 10 * time.Minute
)

// TrackCreator is what the upload handler needs from the data layer
type TrackCreator interface {
	Create(ctx context.Context, sha256 string, size int64) (tracks.Track, bool, error)
}

// ObjectStore is what the upload handler needs from object storage. *storage.Store satisfies it.
type ObjectStore interface {
	Exists(ctx context.Context, key string) (bool, error)
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
}

// Upload a track: POST /api/v1/tracks with the raw audio file as the body.
//
// Order matters, because storage and Postgres can't share a transaction:
// hash → store the object → insert the row and its job. A crash in between leaves at worst an
// orphan object (cleanable), never a row pointing at audio that doesn't exist.
func (s *Server) handleUploadTrack(w http.ResponseWriter, r *http.Request) {
	// Extend the read deadline for this request only. ErrNotSupported only happens with test recorders.
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("[UPLOAD] set read deadline: %v", err)
	}

	// Refuse early when the client announces a body that's too big, before reading anything.
	// Content-Length can lie or be absent (chunked), so MaxBytesReader below is the real limit.
	if r.ContentLength > maxUploadBytes {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, tooLargeBody())
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)

	// 1. Check the type from the first bytes, before writing anything to disk
	head := make([]byte, audio.SniffLen)
	n, err := io.ReadFull(body, head)
	head = head[:n]
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// Shorter than SniffLen: still fine if it's a recognised format (Detect checks lengths)
	case err != nil:
		s.uploadReadFailed(w, r, err)
		return
	}
	if n == 0 {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "request body is empty"})
		return
	}
	contentType, ok := audio.Detect(head)
	if !ok {
		writeJSON(w, r, http.StatusUnsupportedMediaType, map[string]string{
			"error": "unsupported file type: send an MP3, FLAC, Ogg, WAV or M4A file as the raw request body",
		})
		return
	}

	// 2. Spool to a temporary file while hashing. The key is the hash, and the hash is only
	// known after the last byte, so the bytes must wait somewhere. Uses $TMPDIR (default /tmp,
	// which is RAM-backed tmpfs on Fedora: set TMPDIR to a disk directory for big libraries).
	tmp, err := os.CreateTemp("", "musicbox-upload-*")
	if err != nil {
		s.internalError(w, r, fmt.Errorf("[UPLOAD] create temp file: %w", err))
		return
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // Never keep a copy of the upload around
	}()

	hash := sha256.New()
	disk := &errRecorder{w: tmp}
	size, err := io.Copy(io.MultiWriter(disk, hash), io.MultiReader(bytes.NewReader(head), body))
	if err != nil {
		if disk.err != nil { // our side failed (disk full): 500
			s.internalError(w, r, fmt.Errorf("[UPLOAD] write temp file: %w", disk.err))
			return
		}
		s.uploadReadFailed(w, r, err) // the client's side failed: too big, too slow, disconnected
		return
	}
	sum := hex.EncodeToString(hash.Sum(nil)) // computed by us, never taken from the client

	key, err := storage.AudioKey(sum)
	if err != nil { // can't happen with a real SHA-256, but never build a key from unchecked input
		s.internalError(w, r, err)
		return
	}

	// 3. Store the object, unless identical bytes are already there (same hash = same content)
	ctx := r.Context() // cancelled if the client disconnects
	exists, err := s.objects.Exists(ctx, key)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !exists {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			s.internalError(w, r, fmt.Errorf("[UPLOAD] rewind temp file: %w", err))
			return
		}
		if err := s.objects.Put(ctx, key, tmp, size, contentType); err != nil {
			s.internalError(w, r, err)
			return
		}
	}

	// 4. Row and ingest job, in one transaction
	track, created, err := s.tracks.Create(ctx, sum, size)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	log.Printf("[UPLOAD] track %s: %d bytes, %s, new=%t, object already stored=%t", track.ID, size, contentType, created, exists)
	status := http.StatusOK // the same file was already in the library
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, r, status, map[string]any{"track": track})
}

// uploadReadFailed answers when reading the body failed: the limit was hit, or the client was too slow or left
func (s *Server) uploadReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, tooLargeBody())
		return
	}
	log.Printf("[UPLOAD] read body: %v", err)
	writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "upload interrupted"})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %q: %v", r.Method, r.URL.Path, err) // details only in our log
	writeJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func tooLargeBody() map[string]string {
	return map[string]string{"error": fmt.Sprintf("file is larger than %d MiB", maxUploadBytes>>20)}
}

// errRecorder remembers a write error, so the caller can tell "the disk failed" (500)
// from "reading the client's body failed" (4xx): io.Copy returns both the same way.
type errRecorder struct {
	w   io.Writer
	err error
}

func (e *errRecorder) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}
