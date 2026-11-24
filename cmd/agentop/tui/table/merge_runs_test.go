package table

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestMergeRuns_JoinsForegroundRunsOverSpaces(t *testing.T) {
	const fg, bg, r = "\x1b[38;2;1;2;3m", "\x1b[38;2;1;2;3;48;2;4;5;6m", "\x1b[0m"
	for _, tc := range []struct{ in, want string }{
		{fg + "a" + r + "  " + fg + "b" + r + " " + fg + "c" + r, fg + "a  b c" + r},
		{fg + "a" + r + "  " + "\x1b[38;2;9;9;9m" + "b" + r, fg + "a" + r + "  " + "\x1b[38;2;9;9;9m" + "b" + r},
		{bg + "a" + r + " " + bg + "b" + r, bg + "a" + r + " " + bg + "b" + r},
		{fg + "a" + r + " x " + fg + "b" + r, fg + "a" + r + " x " + fg + "b" + r},
		{"plain", "plain"},
	} {
		got := mergeRuns(tc.in)
		if got != tc.want {
			t.Errorf("mergeRuns(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if ansi.Strip(got) != ansi.Strip(tc.in) {
			t.Errorf("mergeRuns(%q) changed the text: %q", tc.in, ansi.Strip(got))
		}
	}
}
