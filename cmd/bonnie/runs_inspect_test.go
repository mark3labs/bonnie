package main

import (
	"strings"
	"testing"
)

// The inspector shares the local journal flag with the plain-output commands.
func TestRunsInspectCommand(t *testing.T) {
	t.Parallel()
	root := newRunsCmd()
	cmd, _, err := root.Find([]string{"inspect"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Use != "inspect [run-id]" {
		t.Fatalf("command use = %q", cmd.Use)
	}
	if cmd.Flags().Lookup("journal") == nil {
		t.Fatal("missing journal flag")
	}
	for _, args := range [][]string{nil, {"run-1"}} {
		if err := cmd.Args(cmd, args); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Args(cmd, []string{"one", "two"}); err == nil {
		t.Fatal("accepted two run IDs")
	}
}

// Opening an invalid journal must fail before Bubble Tea starts.
func TestRunsInspectJournalError(t *testing.T) {
	t.Parallel()
	err := execute(newRunsCmd(), "inspect", "--journal", "\x00")
	if err == nil || !strings.Contains(err.Error(), "open inspector journal") {
		t.Fatalf("error = %v", err)
	}
}
