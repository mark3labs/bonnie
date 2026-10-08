package main

import (
	"net"
	"os"
	"strings"
	"testing"
)

func TestDevTUIChildOutputIsNotTerminalFile(t *testing.T) {
	t.Parallel()
	d := newDevServer(t.TempDir(), devOpts{tui: true})
	if _, ok := d.log.(*os.File); ok {
		t.Fatal("TUI child writes directly to a terminal file")
	}
}

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

// Web selection overrides only the implicit TUI default.
func TestDevWebInterfaceSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                 string
		web, tui, explicit, wantTUI, wantErr bool
	}{
		{"default", false, true, false, true, false},
		{"web", true, true, false, false, false},
		{"web no tui", true, false, true, false, false},
		{"conflict", true, true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := devOpts{web: tc.web, tui: tc.tui}
			err := o.selectInterface(tc.explicit)
			if (err != nil) != tc.wantErr || o.tui != tc.wantTUI {
				t.Fatalf("selection = tui %v, error %v", o.tui, err)
			}
			if d := newDevServer(t.TempDir(), o); d.web != tc.web {
				t.Fatal("web selection did not reach child configuration")
			}
		})
	}
}

// Cobra must detect an explicit true value before it reads or builds a tree.
func TestDevWebRejectsExplicitTUI(t *testing.T) {
	t.Parallel()
	cmd := newDevCmd()
	cmd.SetArgs([]string{"--web", "--tui=true", "missing-tree"})
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "--tui=true") {
		t.Fatalf("command = %v, want interface conflict", err)
	}
}
