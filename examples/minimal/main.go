// Command minimal starts one durable run and prints the answer.
//
// It is the smallest useful BONNIE program. The run is journalled to disk, so
// you can inspect it afterwards:
//
//	go run ./examples/minimal -text "What is 2 + 2?"
//	bonnie runs list --journal .bonnie
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	var (
		dir   = flag.String("journal", ".bonnie", "journal directory")
		runID = flag.String("run", "minimal-1", "run ID")
		model = flag.String("model", "", "model, for example anthropic/claude-sonnet-4-5")
		text  = flag.String("text", "In one sentence, what is BONNIE?", "the message to send")
	)
	flag.Parse()

	if err := run(*dir, *runID, *model, *text); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dir, runID, model, text string) error {
	// A file journal is what makes the run durable. Swap in
	// runtime.NewMemoryJournal() for a run that dies with the process.
	journal, err := runtime.OpenFileJournal(dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	var opts []kit.Option
	if model != "" {
		opts = append(opts, kit.WithModel(model))
	}

	runner := runtime.NewRunner(journal, runtime.KitAgent(opts...))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start also resumes: if runID is already in the journal, its conversation
	// is replayed before the new message.
	result, err := runner.Start(ctx, runID, runtime.Input{Text: text})
	if err != nil {
		return err
	}

	fmt.Printf("run   %s\nstate %s\n\n%s\n", result.ID, result.State, result.Response)
	return nil
}
