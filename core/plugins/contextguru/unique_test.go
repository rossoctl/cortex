package contextguru

import (
	"fmt"
	"testing"
)

// markUnique follows the engine's MarkUnique rule: new keys count, seen keys do not, a partial
// overlap counts in proportion, and a component with no keys counts as unique every time.
func TestSeenKeysMarkUnique(t *testing.T) {
	s := newSeenKeys()
	if got := s.markUnique("s1", "dedup", []string{"a", "b"}, 100); got != 100 {
		t.Errorf("first sight = %d, want 100", got)
	}
	if got := s.markUnique("s1", "dedup", []string{"a", "b"}, 100); got != 0 {
		t.Errorf("repeat = %d, want 0", got)
	}
	if got := s.markUnique("s1", "dedup", []string{"b", "c"}, 100); got != 50 {
		t.Errorf("half new = %d, want 50", got)
	}
	if got := s.markUnique("s1", "summarize", nil, 70); got != 70 {
		t.Errorf("keyless = %d, want 70", got)
	}
	if got := s.markUnique("s1", "summarize", nil, 70); got != 70 {
		t.Errorf("keyless again = %d, want 70: no key to dedup on", got)
	}
	if got := s.markUnique("s1", "dedup", []string{"z"}, 0); got != 0 {
		t.Errorf("zero saving = %d, want 0", got)
	}
}

// Keys are scoped by session and by component: a provider cache is per conversation, and two
// components can stash one content key.
func TestSeenKeysScope(t *testing.T) {
	s := newSeenKeys()
	s.markUnique("s1", "dedup", []string{"k"}, 10)
	if got := s.markUnique("s2", "dedup", []string{"k"}, 10); got != 10 {
		t.Errorf("other session = %d, want 10", got)
	}
	if got := s.markUnique("s1", "format", []string{"k"}, 10); got != 10 {
		t.Errorf("other component = %d, want 10", got)
	}
}

// The set is bounded: past maxSeenKeys it is cleared, and a repeat then counts once as unique.
func TestSeenKeysClearsWhenFull(t *testing.T) {
	s := newSeenKeys()
	s.markUnique("s1", "dedup", []string{"old"}, 10)
	for i := 0; i <= maxSeenKeys; i++ {
		s.markUnique("s1", "dedup", []string{fmt.Sprint(i)}, 1)
	}
	if n := len(s.keys); n > maxSeenKeys+1 {
		t.Fatalf("set holds %d keys, want at most %d", n, maxSeenKeys+1)
	}
	if got := s.markUnique("s1", "dedup", []string{"old"}, 10); got != 10 {
		t.Errorf("after a clear = %d, want 10", got)
	}
}

// A nil set binds to nil, which savingEventFrom reads as "every saving is new".
func TestSeenKeysNilIsAllUnique(t *testing.T) {
	var s *seenKeys
	if s.forSession("s1") != nil {
		t.Fatal("nil set must bind to a nil func")
	}
}
