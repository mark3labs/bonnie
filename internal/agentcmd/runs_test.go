package agentcmd

import (
	"testing"
	"unicode/utf8"
)

// TestOneLineKeepsUTF8 guards the runs table against splitting a character.
func TestOneLineKeepsUTF8(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"a\n  b\tc", 10, "a b c"},
		{"héllo wörld", 6, "héllo…"},
		{"日本語のタイトル", 4, "日本語…"},
		{"🙂🙃🙂", 2, "🙂…"},
		{"x", 0, "…"},
		{"x", -1, "…"},
		{"", 0, ""},
	} {
		got := oneLine(c.in, c.n)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("oneLine(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
