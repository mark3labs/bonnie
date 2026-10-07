package nats

import (
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Reject a missing worker identity before a JetStream connection is opened.
// Core NATS does not need the durable worker answer route.
func TestWorkerIDRequiredForJetStream(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{URL: "nats://127.0.0.1:4222", RootSubject: "bonnie"},
		{URL: "nats://127.0.0.1:4222", Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results", Stream: "INPUT"},
	} {
		if _, err := New(testRunner(fakemodel.New()), cfg); err == nil || !strings.Contains(err.Error(), "WorkerID is required") {
			t.Fatalf("New error = %v, want required WorkerID", err)
		}
	}
	if _, err := New(testRunner(fakemodel.New()), Config{URL: "nats://127.0.0.1:4222", Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}); err != nil {
		t.Fatalf("Core NATS: %v", err)
	}
}
