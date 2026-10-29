package eval

import (
	"fmt"
	"slices"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// Turn is one ended turn of the run: what [T.Send] or [T.Respond] started.
// Its check methods are gates: a failed check fails the eval and the eval
// continues. They return the turn, so checks chain.
type Turn struct {
	e       *T
	run     *runtime.Run
	records []runtime.Record
	input   string
}

// ToolCall is one tool call of a turn, read from the journal.
type ToolCall struct {
	ID   string
	Name string
	// Input is the JSON input the model gave.
	Input string
	// Output is the text the tool returned, or the error text.
	Output string
	// IsError is true when the tool returned an error.
	IsError bool
	// Done is false when the journal has no result for the call, for
	// example because the call halted the turn.
	Done bool
}

// Run returns the run snapshot the turn ended with.
func (t *Turn) Run() *runtime.Run { return t.run }

// Reply returns the agent's final response of the turn.
func (t *Turn) Reply() string { return t.run.Response }

// Records returns the journal records the turn wrote, in order.
func (t *Turn) Records() []runtime.Record { return slices.Clone(t.records) }

// Messages returns the conversation messages the turn wrote, in order.
func (t *Turn) Messages() []kit.LLMMessage { return messages(t.records) }

// ToolCalls returns the tool calls of the turn, in the order the model made
// them.
func (t *Turn) ToolCalls() []ToolCall {
	var calls []ToolCall
	index := map[string]int{}
	for _, msg := range t.Messages() {
		for _, part := range msg.Content {
			switch p := part.(type) {
			case kit.LLMToolCallPart:
				index[p.ToolCallID] = len(calls)
				calls = append(calls, ToolCall{ID: p.ToolCallID, Name: p.ToolName, Input: p.Input})
			case kit.LLMToolResultPart:
				i, ok := index[p.ToolCallID]
				if !ok {
					continue
				}
				calls[i].Done = true
				switch out := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					calls[i].Output = out.Text
				case kit.LLMToolResultOutputContentError:
					calls[i].IsError = true
					if out.Error != nil {
						calls[i].Output = out.Error.Error()
					}
				}
			}
		}
	}
	return calls
}

// Steps returns the number of agent steps the turn completed.
func (t *Turn) Steps() int {
	n := 0
	for _, rec := range t.records {
		if rec.Kind == runtime.RecordStep {
			n++
		}
	}
	return n
}

// Usage returns the token usage of the turn, or zero when the agent
// reported none.
func (t *Turn) Usage() kit.LLMUsage {
	if t.run.Usage == nil {
		return kit.LLMUsage{}
	}
	return *t.run.Usage
}

// Decisions returns the decision records the turn wrote: each call the agent
// made to its own decision model, from a tool, a hook or the decide tool.
// Judge calls are not in the journal.
func (t *Turn) Decisions() []runtime.DecisionRecord {
	var out []runtime.DecisionRecord
	for _, rec := range t.records {
		if rec.Kind != runtime.RecordDecision {
			continue
		}
		d, err := runtime.DecodeDecisionRecord(rec)
		if err != nil {
			t.e.tb.Errorf("bonnie: eval: %v", err)
			continue
		}
		out = append(out, d)
	}
	return out
}

// Succeeded checks that the turn completed with no error.
func (t *Turn) Succeeded() *Turn {
	t.e.tb.Helper()
	if t.run.State != runtime.RunCompleted || t.run.Err != nil {
		t.e.tb.Errorf("bonnie: eval: turn did not succeed: state %s, error %v", t.run.State, t.run.Err)
	}
	return t
}

// Suspended checks that the turn waits for a human answer. Answer it with
// [T.Respond].
func (t *Turn) Suspended() *Turn {
	t.e.tb.Helper()
	if t.run.State != runtime.RunWaiting || t.run.Suspend == nil {
		t.e.tb.Errorf("bonnie: eval: turn is not suspended: state %s", t.run.State)
	}
	return t
}

// CalledTool checks that the turn called the tool name at least once.
func (t *Turn) CalledTool(name string) *Turn {
	t.e.tb.Helper()
	if len(t.calls(name)) == 0 {
		t.e.tb.Errorf("bonnie: eval: tool %q was not called; calls: %s", name, t.callNames())
	}
	return t
}

// CalledToolWith checks that the turn called the tool name with an input
// that m matches.
func (t *Turn) CalledToolWith(name string, m Matcher) *Turn {
	t.e.tb.Helper()
	calls := t.calls(name)
	for _, c := range calls {
		if ok, _ := m.Match(c.Input); ok {
			return t
		}
	}
	if len(calls) == 0 {
		t.e.tb.Errorf("bonnie: eval: tool %q was not called; calls: %s", name, t.callNames())
		return t
	}
	_, why := m.Match(calls[len(calls)-1].Input)
	t.e.tb.Errorf("bonnie: eval: no input of tool %q matches: %s", name, why)
	return t
}

// NotCalledTool checks that the turn did not call the tool name.
func (t *Turn) NotCalledTool(name string) *Turn {
	t.e.tb.Helper()
	if n := len(t.calls(name)); n > 0 {
		t.e.tb.Errorf("bonnie: eval: tool %q was called %d times", name, n)
	}
	return t
}

// NoFailedTools checks that no tool call of the turn returned an error.
func (t *Turn) NoFailedTools() *Turn {
	t.e.tb.Helper()
	for _, c := range t.ToolCalls() {
		if c.IsError {
			t.e.tb.Errorf("bonnie: eval: tool %q failed: %s", c.Name, c.Output)
		}
	}
	return t
}

// MaxSteps checks that the turn completed n agent steps or fewer.
func (t *Turn) MaxSteps(n int) *Turn {
	t.e.tb.Helper()
	if got := t.Steps(); got > n {
		t.e.tb.Errorf("bonnie: eval: turn took %d steps, more than %d", got, n)
	}
	return t
}

// Decided returns a score from the agent's own latest decision on the
// question id in this turn: P(true) for a boolean question, and the
// position on the scale from 0 to 1 for a score question. Use it to check a
// gate the agent applies, for example
// turn.Decided("destructive").AtMost(0.2).Gate(). A turn without the
// decision, or a choice question, gives a failed score.
func (t *Turn) Decided(id string) *Score {
	t.e.tb.Helper()
	s := t.e.newScore(KindDecision, id)
	decisions := t.Decisions()
	for _, d := range slices.Backward(decisions) {
		for _, a := range d.Answers {
			if a.ID != id {
				continue
			}
			q, _ := question(d.Questions, id)
			if q.Type == kit.DecisionChoice {
				s.fail(fmt.Errorf("decision %q is a choice question; read it with Turn.Decisions", id))
				return s
			}
			v, err := normalize(q, a, "")
			if err != nil {
				s.fail(err)
				return s
			}
			s.value, s.model = v, d.Model
			return s
		}
	}
	s.fail(fmt.Errorf("the turn has no decision on %q", id))
	return s
}

func question(qs []kit.DecisionQuestion, id string) (kit.DecisionQuestion, bool) {
	for _, q := range qs {
		if q.ID == id {
			return q, true
		}
	}
	return kit.DecisionQuestion{}, false
}

func (t *Turn) calls(name string) []ToolCall {
	var out []ToolCall
	for _, c := range t.ToolCalls() {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func (t *Turn) callNames() string {
	var names []string
	for _, c := range t.ToolCalls() {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// transcript renders the message records of recs as text.
func transcript(recs []runtime.Record) string {
	var b strings.Builder
	for _, msg := range messages(recs) {
		var parts []string
		for _, part := range msg.Content {
			switch p := part.(type) {
			case kit.LLMTextPart:
				if strings.TrimSpace(p.Text) != "" {
					parts = append(parts, p.Text)
				}
			case kit.LLMToolCallPart:
				parts = append(parts, fmt.Sprintf("[call %s %s]", p.ToolName, p.Input))
			case kit.LLMToolResultPart:
				switch out := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					parts = append(parts, fmt.Sprintf("[result %s]", out.Text))
				case kit.LLMToolResultOutputContentError:
					if out.Error != nil {
						parts = append(parts, fmt.Sprintf("[error %s]", out.Error.Error()))
					}
				}
			}
		}
		if len(parts) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s: %s", msg.Role, strings.Join(parts, "\n"))
	}
	return b.String()
}
