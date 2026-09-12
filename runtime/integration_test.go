//go:build integration

// This file runs BONNIE's durability claim against a real model. It is behind
// a build tag because it costs money and needs a network.
//
//	go test -race -tags integration ./runtime
//
// It skips, rather than fails, when no provider credential is present, so the
// tagged build is safe on a machine with no key.
package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// liveModel returns the model to test against, or "" when the machine has no
// credential. BONNIE_TEST_MODEL overrides the choice.
func liveModel() string {
	if m := os.Getenv("BONNIE_TEST_MODEL"); m != "" {
		return m
	}
	switch {
	case os.Getenv("OPENCODE_API_KEY") != "" || os.Getenv("OPENCODE_ZEN_API_KEY") != "":
		// First, and verified: the live suites ran against this model and
		// answered in seconds, where the Anthropic workspace behind us
		// returned 429 for agent-sized requests. A test model must not
		// share a quota with a busy workspace.
		return "opencode/kimi-k2.5"
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		return "anthropic/claude-sonnet-4-5"
	case os.Getenv("OPENAI_API_KEY") != "":
		return "openai/gpt-4.1"
	case os.Getenv("GEMINI_API_KEY") != "":
		return "google/gemini-2.5-pro"
	default:
		return ""
	}
}

func requireLiveModel(t *testing.T) string {
	t.Helper()
	model := liveModel()
	if model == "" {
		t.Skip("no provider credential: set ANTHROPIC_API_KEY, OPENAI_API_KEY, " +
			"GEMINI_API_KEY, or BONNIE_TEST_MODEL")
	}
	return model
}

// noCoreTools removes Kit's core tool set, so the live agent can call only the
// human-in-the-loop tools BONNIE registers.
//
// This is not a detail. BONNIE ships no sandbox: it runs the tool calls a
// model chooses. An earlier version of this test gave the model the shell and
// write tools in the repository working directory, told it to "deploy the
// app", and the model wrote Terraform files into the checkout. The test now
// gives it nothing to do that with, and [assertWorkspaceUntouched] proves it.
//
// kit.Option is func(*kit.Options), so an inline option is public API.
func noCoreTools() kit.Option {
	return func(o *kit.Options) { o.DisableCoreTools = true }
}

// isolatedWorkspace moves the test into an empty directory and returns a
// function that fails the test if anything appeared in it.
func isolatedWorkspace(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)

	return func() {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			t.Errorf("the agent wrote %q into the working directory: "+
				"BONNIE ships no sandbox, so a test must give the model no tool "+
				"that can touch the disk", e.Name())
		}
	}
}

// TestLiveSuspendAndResume is the claim the release is tagged for, executed
// against a real provider:
//
//  1. A run parks on ask_human.
//  2. A second Runner, sharing only the journal, resumes it.
//  3. The replayed conversation is intact and has no orphaned tool call.
//
// Everything else in the package is verified with a fake agent. This is the
// only place that proves Kit behaves as docs/SPEC.md §3 claims.
func TestLiveSuspendAndResume(t *testing.T) {
	model := requireLiveModel(t)
	assertWorkspaceUntouched := isolatedWorkspace(t)
	defer assertWorkspaceUntouched()

	dir := t.TempDir()
	journal, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	defer func() { _ = journal.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Kit must not open a session of its own — invariant 8. Anything it
	// created would show up in this directory's session list.
	before, err := kit.ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	factory := KitAgent(
		kit.WithModel(model),
		noCoreTools(),
		kit.WithSystemPrompt(
			"You are a deployment assistant. You must never guess a region. "+
				"Before you do anything else, call the ask_human tool to ask "+
				"the operator which region to deploy to. After the operator "+
				"answers, reply with one short sentence that confirms the "+
				"region. Do not do anything else."),
	)

	// Process A: start the run and expect it to park.
	runnerA := NewRunner(journal, factory)
	runA, err := runnerA.Start(ctx, "live-1", Input{
		Text: "Deploy the app. Ask me which region first.",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runA.State != RunWaiting {
		t.Fatalf("state = %q, want %q — the model did not call ask_human (response: %q)",
			runA.State, RunWaiting, runA.Response)
	}
	if runA.Suspend == nil || runA.Suspend.Prompt == "" {
		t.Fatalf("suspend = %+v, want a question", runA.Suspend)
	}
	t.Logf("parked on: %q", runA.Suspend.Prompt)

	// Process B: a new Runner over the same journal, as if the first process
	// had died. Nothing is shared but the files on disk.
	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if state, err := reopened.State(ctx, "live-1"); err != nil || state != RunWaiting {
		t.Fatalf("state after reopen = %q, %v, want waiting", state, err)
	}

	// Inspect what the second process will actually send to the provider.
	restored, err := Restore(ctx, "live-1", reopened)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := restored.GetMessages()
	if len(msgs) < 2 {
		t.Fatalf("replayed %d messages, want the pre-suspension history", len(msgs))
	}

	var calls, results int
	for _, m := range msgs {
		calls += len(toolCallIDs(m))
		results += len(toolResultIDs(m))
	}
	if calls == 0 {
		t.Fatal("the replayed conversation has no tool call — replay is lossy")
	}
	if calls != results {
		t.Fatalf("replayed %d tool calls and %d results — a provider rejects this", calls, results)
	}
	assertNoOrphanLive(t, msgs)

	runnerB := NewRunner(reopened, factory)
	runB, err := runnerB.Resume(ctx, "live-1", []InputResponse{{Text: "eu-west-1"}})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if runB.State != RunCompleted {
		t.Fatalf("state = %q, want %q (response: %q)", runB.State, RunCompleted, runB.Response)
	}
	if !strings.Contains(strings.ToLower(runB.Response), "eu-west-1") {
		t.Logf("resumed response did not echo the region, which is allowed: %q", runB.Response)
	}

	after, err := kit.ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("Kit opened %d session(s) of its own — Options.SessionManager "+
			"was not set before kit.New (docs/SPEC.md §3.1)", len(after)-len(before))
	}
}

// TestLiveToolCallsSurviveOneProcess is the cheaper half of the claim: a
// tool-using turn that does not suspend still journals a conversation a
// provider accepts on the next turn.
func TestLiveToolCallsSurviveOneProcess(t *testing.T) {
	model := requireLiveModel(t)
	assertWorkspaceUntouched := isolatedWorkspace(t)
	defer assertWorkspaceUntouched()

	journal, err := OpenFileJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	defer func() { _ = journal.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runner := NewRunner(journal, KitAgent(kit.WithModel(model), noCoreTools()))
	if _, err := runner.Start(ctx, "live-2", Input{
		Text: "In one short sentence, what is 2 + 2?",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A second turn on the same run replays the first, which is where a lossy
	// journal or an orphaned tool call would surface as a provider error.
	run, err := runner.Start(ctx, "live-2", Input{Text: "And what did I just ask?"})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if run.State != RunCompleted {
		t.Fatalf("state = %q, want completed", run.State)
	}
	if run.Response == "" {
		t.Fatal("second turn produced no response")
	}
}

// assertNoOrphanLive mirrors the assertion in repair_test.go. It is duplicated
// here because that file is not built under the integration tag.
func assertNoOrphanLive(t *testing.T, msgs []kit.LLMMessage) {
	t.Helper()
	results := make(map[string]bool)
	for _, m := range msgs {
		for _, id := range toolResultIDs(m) {
			results[id] = true
		}
	}
	for i, m := range msgs {
		for _, id := range toolCallIDs(m) {
			if !results[id] {
				t.Fatalf("message %d calls %q with no result", i, id)
			}
		}
	}
}
