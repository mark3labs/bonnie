package natswire

import (
	"strings"
	"testing"
)

func TestValidToken(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"": false, "a": true, "Agent-01": true,
		strings.Repeat("a", 128): true, strings.Repeat("a", 129): false,
		"has.dot": false, "has space": false, "under_score": false,
		"ünï": false, "*": false, ">": false,
	} {
		if got := ValidToken(in); got != want {
			t.Errorf("ValidToken(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValidID(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"": false, "task-1": true, "with space inside": true, "ünïcode": true,
		strings.Repeat("a", 256): true, strings.Repeat("a", 257): false,
		strings.Repeat("ü", 128): true, strings.Repeat("ü", 129): false,
		" lead": false, "trail ": false, "ctl\x00": false, "ctl\u0085": false,
		"nats/x": false, "x/nats/": true,
	} {
		if got := ValidID(in); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", in, got, want)
		}
	}
}
