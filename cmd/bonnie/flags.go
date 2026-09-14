package main

import (
	"github.com/spf13/pflag"
)

// defaultJournalDir is where a run's records live when nobody says
// otherwise. It is one constant because four commands must agree on it: a
// `bonnie runs list` that reads a different directory from the `bonnie serve`
// that wrote it reports an empty store and looks like data loss.
const defaultJournalDir = ".bonnie"

// addJournalFlag mounts --journal on a command. Every operator command that
// opens the journal uses it, so the flag name, its default, and its help stay
// one decision instead of four copies.
func addJournalFlag(f *pflag.FlagSet, dir *string) {
	f.StringVar(dir, "journal", defaultJournalDir, "journal directory")
}
