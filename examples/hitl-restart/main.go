// Command hitl-restart is the headline demonstration: a run parks for a human,
// the process exits, and a completely new process finishes the run.
//
// Run it in two phases. The first phase ends with os.Exit, so nothing survives
// but the journal on disk.
//
//	go run ./examples/hitl-restart -phase ask
//	go run ./examples/hitl-restart -phase answer -answer "eu-west-1"
//
// Between the two commands, the run sits in state "waiting" and holds no
// compute. Look at it with:
//
//	bonnie runs list --journal .bonnie --state waiting
//	bonnie runs show --journal .bonnie hitl-1
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

const systemPrompt = `You are a deployment assistant.
You must never guess a deployment region.
Before you do anything else, call the ask_human tool and ask the operator which
region to deploy to. After the operator answers, confirm the region in one
short sentence.`

func main() {
	var (
		phase  = flag.String("phase", "ask", `"ask" parks the run; "answer" resumes it`)
		dir    = flag.String("journal", ".bonnie", "journal directory")
		runID  = flag.String("run", "hitl-1", "run ID")
		model  = flag.String("model", "", "model, for example anthropic/claude-sonnet-4-5")
		answer = flag.String("answer", "eu-west-1", "the answer for the answer phase")
	)
	flag.Parse()

	if err := run(*phase, *dir, *runID, *model, *answer); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(phase, dir, runID, model, answer string) error {
	journal, err := runtime.OpenFileJournal(dir)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()

	var opts []kit.Option
	opts = append(opts, kit.WithSystemPrompt(systemPrompt))
	if model != "" {
		opts = append(opts, kit.WithModel(model))
	}
	runner := runtime.NewRunner(journal, runtime.KitAgent(opts...))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch phase {
	case "ask":
		return ask(ctx, runner, journal, runID)
	case "answer":
		return reply(ctx, runner, runID, answer)
	default:
		return fmt.Errorf("unknown phase %q: use ask or answer", phase)
	}
}

// ask starts the run and exits the process while the run waits.
func ask(ctx context.Context, runner *runtime.Runner, journal *runtime.FileJournal, runID string) error {
	result, err := runner.Start(ctx, runID, runtime.Input{
		Text: "Deploy the app.",
	})
	if err != nil {
		return err
	}
	if result.State != runtime.RunWaiting {
		return fmt.Errorf("the model did not ask anything; state is %q and the answer was %q",
			result.State, result.Response)
	}

	fmt.Printf("run %s is waiting\n\nthe agent asks: %s\n\n", result.ID, result.Suspend.Prompt)
	fmt.Printf("now run:\n  go run ./examples/hitl-restart -phase answer -answer \"eu-west-1\"\n")

	// Close the journal by hand: os.Exit does not run deferred functions.
	if err := journal.Close(); err != nil {
		return err
	}
	// Leave the process. Nothing is in memory any more. The next phase reads
	// only the files on disk.
	os.Exit(0)
	return nil
}

// reply resumes the run in a process that shares nothing but the journal.
func reply(ctx context.Context, runner *runtime.Runner, runID, answer string) error {
	result, err := runner.Resume(ctx, runID, []runtime.InputResponse{{Text: answer}})
	if errors.Is(err, runtime.ErrRunNotFound) {
		return fmt.Errorf("run %s is not in the journal: run the ask phase first", runID)
	}
	if errors.Is(err, runtime.ErrNotWaiting) {
		return fmt.Errorf("run %s is not waiting: run the ask phase first", runID)
	}
	if err != nil {
		return err
	}

	fmt.Printf("run   %s\nstate %s\n\n%s\n", result.ID, result.State, result.Response)
	return nil
}
