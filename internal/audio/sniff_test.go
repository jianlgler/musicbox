package audio_test

import (
	"testing"

	"github.com/jianlgler/musicbox/internal/audio"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want string // "" means rejected
	}{
		{"flac", []byte("fLaC\x00\x00\x00\x22"), "audio/flac"},
		{"ogg", []byte("OggS\x00\x02"), "audio/ogg"},
		{"wav", []byte("RIFF\x24\x08\x00\x00WAVE"), "audio/wav"},
		{"m4a", []byte("\x00\x00\x00\x20ftypM4A "), "audio/mp4"},
		{"m4b audiobook", []byte("\x00\x00\x00\x20ftypM4B "), "audio/mp4"},
		{"mp3 with ID3", []byte("ID3\x04\x00\x00"), "audio/mpeg"},
		{"mp3 frame 128k 44.1kHz", []byte{0xFF, 0xFB, 0x90, 0x64}, "audio/mpeg"},

		{"empty", nil, ""},
		{"html", []byte("<!DOCTYPE html><script>"), ""},
		{"elf executable", []byte("\x7fELF\x02\x01\x01"), ""},
		{"zip", []byte("PK\x03\x04"), ""},
		{"mp4 video brand", []byte("\x00\x00\x00\x20ftypisom"), ""},
		{"riff but avi", []byte("RIFF\x24\x08\x00\x00AVI "), ""},
		{"riff too short", []byte("RIFF\x24\x08"), ""},
		{"multipart body", []byte("--------------------------boundary"), ""},
		{"aac adts", []byte{0xFF, 0xF1, 0x50, 0x80}, ""},
		{"mp3 reserved version", []byte{0xFF, 0xEB, 0x90, 0x64}, ""},
		{"mp3 bad bitrate", []byte{0xFF, 0xFB, 0xF0, 0x64}, ""},
		{"mp3 reserved sample rate", []byte{0xFF, 0xFB, 0x9C, 0x64}, ""},
		{"mp3 sync only", []byte{0xFF, 0xFB}, ""},
		{"lowercase flac", []byte("flac"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := audio.Detect(tc.head)
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("Detect(%q) = %q, %v; want %q", tc.head, got, ok, tc.want)
			}
		})
	}
}
