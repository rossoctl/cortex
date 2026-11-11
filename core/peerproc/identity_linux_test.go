//go:build linux

package peerproc

import "testing"

// The command name is parenthesised and may itself contain spaces and parentheses.
func TestParseStatIdentity(t *testing.T) {
	line := []byte("1234 (a b) (c)) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000 10 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0")
	state, ticks, err := parseStatIdentity(line)
	if err != nil || state != "S" || ticks != 98765 {
		t.Errorf("= %q, %d, %v; want S, 98765", state, ticks, err)
	}
	if _, _, err := parseStatIdentity([]byte("1234 no-paren")); err == nil {
		t.Error("a line with no ')' parsed")
	}
	if _, _, err := parseStatIdentity([]byte("1 (x) S 1")); err == nil {
		t.Error("a short line parsed")
	}
}
