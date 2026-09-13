package main

import (
	"testing"
)

func TestChatURL(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		":9090":          "http://127.0.0.1:9090",
		"localhost:9000": "http://localhost:9000",
		"http://x:1":     "http://x:1",
		"https://x:1":    "https://x:1",
		"":               "http://127.0.0.1:8080",
		":0":             "http://127.0.0.1:0",
	}
	for in, want := range cases {
		if got := chatURL(in); got != want {
			t.Errorf("chatURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTuiAddress(t *testing.T) {
	t.Parallel()
	if got := tuiAddress("/tmp/my-agent"); got != "dev-my-agent" {
		t.Fatalf("tuiAddress = %q, want dev-my-agent", got)
	}
}

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
	if run := chat.Flags().Lookup("run"); run == nil || run.DefValue != "tui-default" {
		t.Fatalf("run flag = %+v, want default tui-default", run)
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
