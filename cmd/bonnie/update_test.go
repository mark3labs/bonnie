package main

import (
	"bytes"
	"strings"
	"testing"
)

// The CLI must expose the shared update commands without updating storage.
// Run serially because other CLI tests replace os.Stdout.
func TestUpdateCommandHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"update", "--help"}, {"update", "models", "--help"}} {
		root := newRootCmd()
		if args[0] == "update" {
			path := args[:len(args)-1]
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
		if !strings.Contains(out.String(), "update") {
			t.Fatalf("%v: help does not contain update: %s", args, &out)
		}
	}
}
