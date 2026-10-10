package nats

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

func TestAgentJSONIdentityFields(t *testing.T) {
	t.Parallel()
	for _, value := range []any{Answer{AgentID: "alpha"}, Result{AgentID: "alpha"}} {
		data, err := json.Marshal(value)
		if err != nil || !strings.Contains(string(data), `"agent_id":"alpha"`) || strings.Contains(string(data), `"worker_id"`) {
			t.Fatalf("JSON identity = %s, %v", data, err)
		}
	}
}

// Reject a missing agent identity before a JetStream connection is opened.
// Core NATS does not need the durable agent answer route.
func TestAgentIDRequiredForJetStream(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{URL: "nats://127.0.0.1:4222", RootSubject: "bonnie"},
		{URL: "nats://127.0.0.1:4222", Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results", Stream: "INPUT"},
	} {
		if _, err := New(testRunner(fakemodel.New()), cfg); err == nil || !strings.Contains(err.Error(), "AgentID is required") {
			t.Fatalf("New error = %v, want required AgentID", err)
		}
	}
	if _, err := New(testRunner(fakemodel.New()), Config{URL: "nats://127.0.0.1:4222", Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}); err != nil {
		t.Fatalf("Core NATS: %v", err)
	}
}
