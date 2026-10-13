package main

import (
	"testing"
)

// TestChatCommandMounts: the chat command parses and is wired into the root,
// with sensible defaults, so `bonnie chat` is present in help.
func TestChatCommandMounts(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	chat, _, err := root.Find([]string{"chat"})
	if err != nil {
		t.Fatalf("chat command not found: %v", err)
	}
	if chat.Use != "chat" {
		t.Fatalf("chat.Use = %q, want chat", chat.Use)
	}
	addr := chat.Flags().Lookup("addr")
	if addr == nil || addr.DefValue != "127.0.0.1:8080" {
		t.Fatalf("addr flag = %+v, want default 127.0.0.1:8080", addr)
	}
	if run := chat.Flags().Lookup("run"); run == nil || run.DefValue != "" {
		t.Fatalf("run flag = %+v, want empty default", run)
	}
}

// TestDevCommandMountsTUI: `bonnie dev` defaults to opening the TUI, and
// --no-tui disables it.
func TestDevCommandMountsTUI(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	dev, _, err := root.Find([]string{"dev"})
	if err != nil {
		t.Fatalf("dev command not found: %v", err)
	}
	f := dev.Flags().Lookup("tui")
	if f == nil {
		t.Fatal("dev has no --tui flag")
	}
	if f.DefValue != "true" {
		t.Fatalf("dev --tui default = %q, want true (dev opens the TUI)", f.DefValue)
	}
}
