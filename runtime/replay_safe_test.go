package runtime

import "testing"

// AddReplaySafeTools adds to the set; SetReplaySafeTools replaces it. Two
// independent options that each declare one tool must both stay declared.
func TestAddReplaySafeToolsKeepsEarlierTools(t *testing.T) {
	t.Parallel()
	s := NewSession("r", NewMemoryJournal())
	s.AddReplaySafeTools("lookup")
	s.AddReplaySafeTools("modify_agent")
	if !s.replaySafe["lookup"] || !s.replaySafe["modify_agent"] {
		t.Fatalf("replay-safe set = %v, want both tools", s.replaySafe)
	}
	s.SetReplaySafeTools("only")
	if len(s.replaySafe) != 1 || !s.replaySafe["only"] {
		t.Fatalf("after Set: %v, want only", s.replaySafe)
	}
}
