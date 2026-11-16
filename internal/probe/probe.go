// Package probe reads tags, audio properties and cover art from an audio file.
//
// Read runs TagLib (C++, compiled to WebAssembly) inside wazero's sandbox: the parser can only
// see the file's directory, read-only, and a memory bug in it stays inside the WASM instance.
// It can't be cancelled, so the worker never calls Read directly: it calls Isolated (isolated.go),
// which runs Read in a child process that is killed on timeout.
//
// Everything Read returns comes from an uploaded file and is cleaned here: valid UTF-8, no control
// characters, bounded lengths, numbers in range, and covers checked from their own bytes.
package probe

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the decoders image.DecodeConfig uses
	_ "image/png"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.senan.xyz/taglib"
)

const (
	MaxTextLen     = 500      // characters; tracks_text_lengths enforces the same
	MaxCoverBytes  = 10 << 20 // a front cover is usually 50 KB to 2 MB
	MaxCoverPixels = 8000     // per side; stops decompression bombs before anything decodes them
)

// ErrUnreadable means TagLib doesn't recognise the file as audio it can parse
var ErrUnreadable = errors.New("not a readable audio file")

// Result is what a file says about itself. Pointers are nil when the tag is missing or invalid.
type Result struct {
	Title       *string `json:"title"`
	Artist      *string `json:"artist"`
	Album       *string `json:"album"`
	AlbumArtist *string `json:"album_artist"`
	TrackNumber *int    `json:"track_number"`
	DiscNumber  *int    `json:"disc_number"`
	Year        *int    `json:"year"`
	DurationMS  int64   `json:"duration_ms"`
	Cover       []byte  `json:"cover,omitempty"` // JSON encodes []byte as base64
	CoverType   string  `json:"cover_type,omitempty"`
	// Why an embedded cover was dropped, for the log; a bad cover never fails the track
	CoverRejected string `json:"cover_rejected,omitempty"`
}

// Read parses path. The file must be alone in its directory: TagLib's sandbox mounts the whole
// directory, so anything next to it would be readable by the parser.
func Read(path string) (Result, error) {
	tags, err := taglib.ReadTags(path)
	if err != nil {
		// TagLib's own message ("invalid file") adds nothing, and it never includes file content
		return Result{}, ErrUnreadable
	}
	props, err := taglib.ReadProperties(path)
	if err != nil {
		return Result{}, fmt.Errorf("read properties: %w", err)
	}

	r := Result{
		Title:       Text(tags[taglib.Title]),
		Artist:      Text(tags[taglib.Artist]),
		Album:       Text(tags[taglib.Album]),
		AlbumArtist: Text(tags[taglib.AlbumArtist]),
		TrackNumber: Number(first(tags[taglib.TrackNumber]), 1, 9999),
		DiscNumber:  Number(first(tags[taglib.DiscNumber]), 1, 999),
		Year:        Number(first(tags[taglib.Date]), 1000, 9999),
		DurationMS:  Duration(props.Length.Milliseconds()),
	}

	if len(props.Images) > 0 {
		img, err := taglib.ReadImage(path)
		if err != nil {
			r.CoverRejected = "unreadable"
		} else if img != nil {
			r.Cover, r.CoverType, r.CoverRejected = Cover(img)
		}
	}
	return r, nil
}

// Text turns a multi-valued tag into one clean string, or nil if nothing is left.
// Multiple values (two artists) are joined with "; ".
func Text(values []string) *string {
	var parts []string
	for _, v := range values {
		if v = clean(v); v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	s := strings.Join(parts, "; ")
	if utf8.RuneCountInString(s) > MaxTextLen {
		s = string([]rune(s)[:MaxTextLen]) // cut on a character boundary, not a byte
	}
	s = strings.TrimSpace(s)
	return &s
}

// clean keeps text that's safe to store and show: valid UTF-8 (Postgres rejects anything else;
// ranging over a string already yields U+FFFD for each invalid byte, so the loop does it),
// no control characters (NUL breaks Postgres; newlines and escape codes break logs and terminals),
// no zero-width or bidi formatting characters (they can make "Admin" display as something else),
// and runs of whitespace collapsed to one space.
func clean(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r): // Cc and Cf (format: bidi, zero-width)
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// Number reads the leading integer of a tag value ("3/12" → 3, "2024-05-01" → 2024),
// or nil if there isn't one in [lo, hi]. It stops as soon as the value passes hi, so a tag of
// a thousand digits can't overflow.
func Number(s string, lo, hi int) *int {
	s = strings.TrimSpace(s)
	n, digits := 0, 0
	for ; digits < len(s) && s[digits] >= '0' && s[digits] <= '9'; digits++ {
		n = n*10 + int(s[digits]-'0')
		if n > hi {
			return nil
		}
	}
	if digits == 0 || n < lo {
		return nil
	}
	return &n
}

// Cover checks embedded image bytes. The MIME type stored in the tag is chosen by whoever made
// the file, so the type comes from the bytes; only JPEG and PNG are kept, and DecodeConfig reads
// just the header to check the size before anything would decode millions of pixels.
func Cover(img []byte) (data []byte, mimeType string, rejected string) {
	if len(img) > MaxCoverBytes {
		return nil, "", fmt.Sprintf("larger than %d MiB", MaxCoverBytes>>20)
	}
	switch {
	case bytes.HasPrefix(img, []byte("\xFF\xD8\xFF")):
		mimeType = "image/jpeg"
	case bytes.HasPrefix(img, []byte("\x89PNG\r\n\x1a\n")):
		mimeType = "image/png"
	default:
		return nil, "", "not a JPEG or PNG"
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		return nil, "", "invalid image header"
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxCoverPixels || cfg.Height > MaxCoverPixels {
		return nil, "", fmt.Sprintf("%dx%d pixels is out of range", cfg.Width, cfg.Height)
	}
	return img, mimeType, ""
}

// Duration keeps a length the duration_ms column (32-bit integer) can hold. A crafted header can
// claim any length; out of range is stored as 0 ("unknown") instead of failing the update.
func Duration(ms int64) int64 {
	const maxMS = 1<<31 - 1 // ~24 days
	if ms < 0 || ms > maxMS {
		return 0
	}
	return ms
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
