// Package audio knows about audio file formats. For now: recognising them from their first bytes.
package audio

import "bytes"

// SniffLen is how many leading bytes Detect needs to recognise every supported format
const SniffLen = 12

// Detect returns the MIME type of a supported audio file from its first bytes ("magic numbers"),
// or ok=false. It's an allowlist: anything not recognised is rejected.
//
// It only looks at the header, so it stops HTML, scripts, executables and random data from being
// stored as audio. It does not prove the rest of the file is valid: the ingest worker does that.
// The client's Content-Type is never used, because the client chooses it.
func Detect(head []byte) (mimeType string, ok bool) {
	switch {
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "audio/flac", true
	case bytes.HasPrefix(head, []byte("OggS")): // Vorbis and Opus
		return "audio/ogg", true
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")):
		return "audio/wav", true
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")) && isAudioBrand(head[8:12]):
		return "audio/mp4", true
	case bytes.HasPrefix(head, []byte("ID3")): // MP3 starting with an ID3v2 tag (the usual case)
		return "audio/mpeg", true
	case isMP3Frame(head): // MP3 without a tag: starts directly with a frame header
		return "audio/mpeg", true
	}
	return "", false
}

// MP4 is a container for video too; only the audio-only brands are accepted
func isAudioBrand(brand []byte) bool {
	return bytes.Equal(brand, []byte("M4A ")) || bytes.Equal(brand, []byte("M4B "))
}

// isMP3Frame checks a 4-byte MPEG audio frame header. Two bytes of sync alone (0xFFE)
// would match far too much data, so the reserved values are rejected too.
func isMP3Frame(h []byte) bool {
	if len(h) < 4 || h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return false // no 11-bit frame sync
	}
	version := (h[1] >> 3) & 0x03
	layer := (h[1] >> 1) & 0x03
	bitrate := h[2] >> 4
	sampleRate := (h[2] >> 2) & 0x03
	return version != 0x01 && // reserved
		layer == 0x01 && // Layer III (layer 0 is reserved; it's also how AAC ADTS starts, so AAC is excluded)
		bitrate != 0x00 && bitrate != 0x0F && // "free" and "bad"
		sampleRate != 0x03 // reserved
}
