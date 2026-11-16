package tracks_test

import (
	"testing"

	"github.com/jianlgler/musicbox/internal/tracks"
)

func TestEscapeLike(t *testing.T) {
	for in, want := range map[string]string{
		"plain":      "plain",
		"100%":       `100\%`,
		"a_b":        `a\_b`,
		`back\slash`: `back\\slash`,
		`\%`:         `\\\%`, // the backslash is escaped first, or it would escape the new one
		"":           "",
		"%_%":        `\%\_\%`,
	} {
		if got := tracks.EscapeLike(in); got != want {
			t.Errorf("EscapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}
