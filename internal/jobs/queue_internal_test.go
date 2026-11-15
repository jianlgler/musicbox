package jobs

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Errors can carry bytes from an uploaded file. Postgres would reject the UPDATE (and the job
// would be stuck in running) if last_error had invalid UTF-8 or a NUL byte.
func TestCleanError(t *testing.T) {
	cases := map[string]string{
		"bad tag: \xff\xfe": "bad tag: �",
		"nul\x00byte":       "nulbyte",
		"plain":             "plain",
	}
	for in, want := range cases {
		if got := cleanError(errors.New(in)); got != want {
			t.Errorf("cleanError(%q) = %q, want %q", in, got, want)
		}
	}
	if cleanError(nil) != "" {
		t.Error("cleanError(nil) should be empty")
	}

	// Cut inside a multi-byte character: still valid UTF-8, and bounded
	long := strings.Repeat("a", maxErrorLen-1) + "é" + strings.Repeat("b", 5000)
	got := cleanError(errors.New(long))
	if !utf8.ValidString(got) {
		t.Error("truncated error is not valid UTF-8")
	}
	if len(got) > maxErrorLen+len("…") {
		t.Errorf("length %d, want at most %d", len(got), maxErrorLen+len("…"))
	}
}
