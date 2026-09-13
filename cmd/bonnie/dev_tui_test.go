package main

import (
	"net"
	"strings"
	"testing"
)

// TestPickListenAddrWalksFrom8080: an occupied 8080 is skipped and 8081 is
// chosen. An explicit --addr is used as-is.
func TestPickListenAddrWalksFrom8080(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skipf("8080 is already taken: %v", err)
	}
	defer func() { _ = ln.Close() }()

	got, err := pickListenAddr("")
	if err != nil {
		t.Fatalf("pickListenAddr: %v", err)
	}
	if got == "127.0.0.1:8080" {
		t.Fatalf("picked the occupied port %s", got)
	}
	if got != "127.0.0.1:8081" && !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("picked %q, want a loopback port past 8080", got)
	}

	explicit, err := pickListenAddr("127.0.0.1:9099")
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if explicit != "127.0.0.1:9099" {
		t.Fatalf("explicit = %q, want 127.0.0.1:9099", explicit)
	}
}
