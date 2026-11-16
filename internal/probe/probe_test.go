package probe_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	// Registers GIF globally, as any package in the binary could: the cover allowlist
	// must not depend on which decoders happen to be registered
	_ "image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jianlgler/musicbox/internal/probe"
)

// TestMain lets this test binary be its own probe child. Files with special names make the
// child misbehave on purpose, to test how Isolated handles it.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == probe.ChildArg {
		switch filepath.Base(os.Args[2]) {
		case "hang":
			time.Sleep(time.Hour)
		case "crash":
			panic("parser bug")
		case "flood":
			os.Stdout.Write(bytes.Repeat([]byte("x"), 20<<20))
			os.Exit(0)
		case "badcover": // a compromised child returning an image the allowlist rejects
			gif := base64.StdEncoding.EncodeToString([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00"))
			fmt.Printf(`{"result":{"cover":%q}}`+"\n", gif)
			os.Exit(0)
		case "env":
			fmt.Printf(`{"error":%q}`+"\n", strings.Join(os.Environ(), " "))
			os.Exit(0)
		}
		os.Exit(probe.ChildMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func isolated(t *testing.T) probe.Isolated {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return probe.Isolated{Executable: self, Timeout: 5 * time.Second, TempDir: t.TempDir()}
}

// alone copies a fixture into a directory of its own, as the parser's sandbox requires
func alone(t *testing.T, fixture string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return write(t, "audio", b)
}

func write(t *testing.T, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func num(p *int) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

func TestReadFixtures(t *testing.T) {
	for _, f := range []string{"tagged.flac", "tagged.mp3", "tagged.ogg", "tagged.m4a"} {
		t.Run(f, func(t *testing.T) {
			r, err := probe.Read(alone(t, f))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			got := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", str(r.Title), str(r.Artist), str(r.Album), str(r.AlbumArtist), num(r.TrackNumber), num(r.DiscNumber), num(r.Year))
			want := "Test Tone|The Sines|Waveforms|Various|3|1|2024"
			if f == "tagged.ogg" || f == "tagged.m4a" {
				// ffmpeg doesn't write every field to every format; check the common ones
				got, want = strings.Join(strings.Split(got, "|")[:3], "|"), "Test Tone|The Sines|Waveforms"
			}
			if got != want {
				t.Errorf("tags = %s, want %s", got, want)
			}
			if r.DurationMS < 200 || r.DurationMS > 400 {
				t.Errorf("duration %d ms, want ~250", r.DurationMS)
			}
		})
	}
}

func TestReadCover(t *testing.T) {
	for _, f := range []string{"tagged.flac", "tagged.mp3"} {
		r, err := probe.Read(alone(t, f))
		if err != nil {
			t.Fatal(err)
		}
		if r.CoverType != "image/jpeg" || len(r.Cover) == 0 || r.CoverRejected != "" {
			t.Errorf("%s: cover %d bytes, type %q, rejected %q", f, len(r.Cover), r.CoverType, r.CoverRejected)
		}
	}
}

func TestReadNoTags(t *testing.T) {
	for _, f := range []string{"untagged.flac", "plain.wav"} {
		r, err := probe.Read(alone(t, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if r.Title != nil || r.Artist != nil || r.TrackNumber != nil || r.Cover != nil {
			t.Errorf("%s: got tags from an untagged file: %+v", f, r)
		}
		if r.DurationMS == 0 {
			t.Errorf("%s: no duration", f)
		}
	}
}

func TestReadUnreadable(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":      nil,
		"html":       []byte("<!DOCTYPE html><script>alert(1)</script>"),
		"flac magic": []byte("fLaC" + strings.Repeat("\x00", 100)),
	} {
		if _, err := probe.Read(write(t, "audio", b)); !errors.Is(err, probe.ErrUnreadable) {
			t.Errorf("%s: err = %v, want ErrUnreadable", name, err)
		}
	}
}

func TestText(t *testing.T) {
	cases := []struct {
		in   []string
		want string // "<nil>" for nil
	}{
		{[]string{"Plain"}, "Plain"},
		{[]string{"  padded  "}, "padded"},
		{[]string{"two\n\nlines\ttab"}, "two lines tab"},
		{[]string{"nul\x00byte"}, "nulbyte"},
		{[]string{"esc\x1b[31mred"}, "esc[31mred"}, // terminal escape codes
		{[]string{"bad \xff utf8"}, "bad � utf8"},  // Postgres rejects invalid UTF-8
		{[]string{"evil‮gpj.exe"}, "evilgpj.exe"},  // right-to-left override
		{[]string{"zero​width"}, "zerowidth"},      // zero-width space
		{[]string{"Ünïcödé 音楽"}, "Ünïcödé 音楽"},     // real text survives
		{[]string{"A", "", "  ", "B"}, "A; B"},     // multi-valued
		{[]string{""}, "<nil>"},
		{[]string{"\x00​ \n"}, "<nil>"},
		{nil, "<nil>"},
	}
	for _, tc := range cases {
		if got := str(probe.Text(tc.in)); got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := probe.Text([]string{strings.Repeat("é", probe.MaxTextLen+50)})
	if n := utf8.RuneCountInString(*long); n != probe.MaxTextLen || !utf8.ValidString(*long) {
		t.Errorf("long text: %d characters, valid %v; want %d", n, utf8.ValidString(*long), probe.MaxTextLen)
	}
}

func TestNumber(t *testing.T) {
	cases := map[string]string{
		"3": "3", "3/12": "3", " 07 ": "7", "2024-05-01": "2024", "2024": "2024",
		"": "<nil>", "x3": "<nil>", "0": "<nil>", "10000": "<nil>", "-1": "<nil>",
		"99999999999999999999":          "<nil>", // would overflow an int
		"0000000012":                    "12",    // leading zeros are fine
		strings.Repeat("0", 5000) + "7": "7",
	}
	for in, want := range cases {
		if got := num(probe.Number(in, 1, 9999)); got != want {
			t.Errorf("Number(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 250: 250, -5: 0, 1<<31 - 1: 1<<31 - 1, 1 << 31: 0, 1 << 62: 0} {
		if got := probe.Duration(in); got != want {
			t.Errorf("Duration(%d) = %d, want %d", in, got, want)
		}
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngHeaderClaiming builds a valid PNG signature and IHDR that claim w×h pixels, with no
// pixel data: what a decompression bomb looks like to DecodeConfig
func pngHeaderClaiming(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	chunk := append([]byte("IHDR"), ihdr...)
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	b.Write(chunk)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestCover(t *testing.T) {
	if _, typ, rej := probe.Cover(pngBytes(t, 4, 4)); typ != "image/png" || rej != "" {
		t.Errorf("small PNG: type %q, rejected %q", typ, rej)
	}
	for name, img := range map[string][]byte{
		"bomb 100000x100000":       pngHeaderClaiming(100000, 100000),
		"gif (decoder registered)": []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00"),
		"html":                     []byte("<svg onload=alert(1)>"),
		"broken png":               []byte("\x89PNG\r\n\x1a\ngarbage"),
		"too large":                append([]byte("\xFF\xD8\xFF"), make([]byte, probe.MaxCoverBytes)...),
	} {
		if data, typ, rej := probe.Cover(img); data != nil || typ != "" || rej == "" {
			t.Errorf("%s: kept (type %q), want rejected", name, typ)
		}
	}
}

func TestIsolatedReadsFixture(t *testing.T) {
	r, err := isolated(t).Probe(context.Background(), alone(t, "tagged.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if str(r.Title) != "Test Tone" || r.CoverType != "image/jpeg" {
		t.Errorf("title %s, cover type %q", str(r.Title), r.CoverType)
	}
}

func TestIsolatedUnreadable(t *testing.T) {
	_, err := isolated(t).Probe(context.Background(), write(t, "audio", []byte("not audio at all")))
	if !errors.Is(err, probe.ErrUnreadable) {
		t.Errorf("err = %v, want ErrUnreadable", err)
	}
}

func TestIsolatedKillsHungParser(t *testing.T) {
	p := isolated(t)
	p.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := p.Probe(context.Background(), write(t, "hang", nil))
	if !errors.Is(err, probe.ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s: the child wasn't killed at the deadline", d)
	}
}

func TestIsolatedCrash(t *testing.T) {
	_, err := isolated(t).Probe(context.Background(), write(t, "crash", nil))
	if !errors.Is(err, probe.ErrCrashed) || !strings.Contains(err.Error(), "panic: parser bug") {
		t.Errorf("err = %v, want ErrCrashed with the panic message", err)
	}
}

func TestIsolatedOutputFlood(t *testing.T) {
	_, err := isolated(t).Probe(context.Background(), write(t, "flood", nil))
	if !errors.Is(err, probe.ErrCrashed) || !strings.Contains(err.Error(), "output larger than") {
		t.Errorf("err = %v, want ErrCrashed because of the 20 MiB of output", err)
	}
}

// Never trust the child more than the file it parsed: its cover is checked again
func TestIsolatedRechecksChildCover(t *testing.T) {
	r, err := isolated(t).Probe(context.Background(), write(t, "badcover", nil))
	if err != nil {
		t.Fatal(err)
	}
	if r.Cover != nil || r.CoverRejected == "" {
		t.Errorf("kept a GIF cover from the child (type %q)", r.CoverType)
	}
}

// The worker's environment holds DATABASE_URL and the S3 keys; the child must not inherit them
func TestIsolatedChildGetsNoSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://musicbox:hunter2@127.0.0.1/musicbox")
	t.Setenv("S3_SECRET_KEY", "supersecretsupersecret")
	_, err := isolated(t).Probe(context.Background(), write(t, "env", nil))
	if err == nil {
		t.Fatal("expected the env dump as an error")
	}
	env := err.Error()
	for _, secret := range []string{"hunter2", "supersecret", "DATABASE_URL", "S3_", "HOME="} {
		if strings.Contains(env, secret) {
			t.Errorf("child environment contains %q: %s", secret, env)
		}
	}
	if !strings.Contains(env, "TMPDIR=") {
		t.Errorf("child environment %q lacks TMPDIR", env)
	}
}

// Fuzzing: arbitrary bytes, written as a file, must never make Read panic or return dirty text.
// Run with: go test ./internal/probe -run '^$' -fuzz FuzzRead -fuzztime 60s
func FuzzRead(f *testing.F) {
	entries, _ := os.ReadDir("testdata")
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join("testdata", e.Name())); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "audio")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := probe.Read(path)
		if err != nil {
			return
		}
		for _, s := range []*string{r.Title, r.Artist, r.Album, r.AlbumArtist} {
			checkClean(t, s)
		}
		if r.DurationMS < 0 || r.DurationMS > 1<<31-1 {
			t.Errorf("duration %d out of range", r.DurationMS)
		}
		if r.Cover != nil && r.CoverType != "image/jpeg" && r.CoverType != "image/png" {
			t.Errorf("cover kept with type %q", r.CoverType)
		}
	})
}

// FuzzText checks Text's promises on any input, much faster than FuzzRead
func FuzzText(f *testing.F) {
	for _, s := range []string{"Plain", "a\x00b", "\xff\xfe", "x‮y", strings.Repeat("é", 600)} {
		f.Add(s, "second value")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		checkClean(t, probe.Text([]string{a, b}))
	})
}

func FuzzNumber(f *testing.F) {
	for _, s := range []string{"3/12", "2024-05-01", "99999999999", "-1", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if n := probe.Number(s, 1, 9999); n != nil && (*n < 1 || *n > 9999) {
			t.Errorf("Number(%q) = %d, out of range", s, *n)
		}
	})
}

func checkClean(t *testing.T, s *string) {
	t.Helper()
	if s == nil {
		return
	}
	if *s == "" || !utf8.ValidString(*s) || utf8.RuneCountInString(*s) > probe.MaxTextLen || strings.TrimSpace(*s) != *s {
		t.Errorf("dirty text %q", *s)
	}
	for _, r := range *s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
			t.Errorf("text %q contains %U", *s, r)
		}
	}
	if strings.Contains(*s, "  ") {
		t.Errorf("text %q has a double space", *s)
	}
}
