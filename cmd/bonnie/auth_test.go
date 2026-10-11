package main

import (
	"bytes"
	"strings"
	"testing"
)

// The CLI must expose the shared auth commands without starting a login.
// Run serially because other CLI tests replace os.Stdout.
func TestAuthCommandHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"auth", "--help"},
		{"auth", "login", "openai", "--help"},
		{"auth", "status", "--help"},
		{"auth", "logout", "openai", "--help"},
	} {
		root := newRootCmd()
		if args[0] == "auth" {
			path := args[:len(args)-1]
			if path[len(path)-1] == "openai" {
				path = path[:len(path)-1]
			}
			cmd, remaining, err := root.Find(path)
			if err != nil || len(remaining) != 0 || cmd.Name() != path[len(path)-1] {
				t.Fatalf("Find(%v) = %v, %v, %v", path, cmd, remaining, err)
			}
		}
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "auth") {
			t.Fatalf("%v: help does not contain auth: %s", args, &out)
		}
	}
}
