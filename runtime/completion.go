package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// CompletionCandidate is a normal model response awaiting acceptance.
type CompletionCandidate struct {
	Response          string `json:"response"`
	ContinuationsUsed int    `json:"continuations_used"`
}

// CompletionFeedback accepts a candidate when ContinueWith is empty. Otherwise
// ContinueWith is the next model prompt; it is not a steer message.
type CompletionFeedback struct {
	ContinueWith string `json:"continue_with,omitempty"`
}

// CompletionHook checks a candidate before the runner publishes its response.
// A check interrupted before its feedback is durable can run again. Hooks must
// tolerate repeated calls and must observe ctx cancellation.
type CompletionHook func(ctx context.Context, candidate CompletionCandidate) (CompletionFeedback, error)

// CompletionAgent is an optional agent interface for completion checks.
// A Runner hook takes precedence over this interface.
type CompletionAgent interface {
	Complete(context.Context, CompletionCandidate) (CompletionFeedback, error)
}

// ErrContinuationLimit means a check requested more continuations than allowed.
var ErrContinuationLimit = errors.New("bonnie: completion continuation limit reached")

// RecordCompletion stores completion orchestration, not conversation history.
// Its JSON payload contains the candidate, feedback, phase, message watermark,
// prompt, and known usage. Phases are pending, check, continue, and accepted.
const RecordCompletion RecordKind = "completion"

// WithCompletionHook installs a completion check and its continuation limit.
// Empty feedback accepts a candidate, including at a zero limit. Negative
// limits are treated as zero. WithCompletionLimit can override this limit.
func WithCompletionHook(hook CompletionHook, limit int) RunnerOption {
	return func(r *Runner) { r.completionHook = hook; r.completionLimit = max(0, limit) }
}

// WithCompletionLimit sets the maximum number of model continuations for one
// logical turn. The default is zero. Suspension, cancellation, and process
// recovery do not reset the count. A completed or failed turn starts a new budget.
func WithCompletionLimit(limit int) RunnerOption {
	return func(r *Runner) { r.completionLimit = max(0, limit) }
}

// completionWriteError leaves the run recoverable when orchestration cannot
// commit. A failed state would close the budget and discard pending feedback.
type completionWriteError struct{ err error }

func (e *completionWriteError) Error() string { return e.err.Error() }
func (e *completionWriteError) Unwrap() error { return e.err }

type completionRecord struct {
	Candidate  CompletionCandidate `json:"candidate"`
	Feedback   CompletionFeedback  `json:"feedback"`
	Phase      string              `json:"phase"`
	MessageSeq int                 `json:"message_seq"`
	Prompt     string              `json:"prompt,omitempty"`
	Usage      *kit.LLMUsage       `json:"usage,omitempty"`
}

func (r *Runner) saveCompletion(ctx context.Context, s *Session, c *completionRecord) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("bonnie: encode completion: %w", err)
	}
	_, err = r.journal.Append(ctx, Record{RunID: s.runID, Kind: RecordCompletion, Timestamp: now(), Payload: payload})
	if err != nil {
		return &completionWriteError{err: fmt.Errorf("bonnie: save completion: %w", err)}
	}
	return nil
}

func (r *Runner) restoreCompletion(ctx context.Context, s *Session) (*completionRecord, error) {
	recs, err := r.journal.Replay(ctx, s.runID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: restore completion: %w", err)
	}
	var c *completionRecord
	for _, rec := range recs {
		if rec.Kind == RecordClear || (rec.Kind == RecordState && (rec.State == RunCompleted || rec.State == RunFailed || rec.State == RunRetired)) {
			c = nil
		}
		if rec.Kind == RecordResume && c != nil {
			c.Phase = "pending"
			c.Prompt = rec.Text
			c.MessageSeq = rec.Seq
			c.Feedback = CompletionFeedback{}
		}
		if rec.Kind != RecordCompletion {
			continue
		}
		var next completionRecord
		if err := json.Unmarshal(rec.Payload, &next); err != nil {
			return nil, fmt.Errorf("bonnie: decode completion: %w", err)
		}
		switch next.Phase {
		case "pending", "check", "continue", "accepted":
		default:
			return nil, fmt.Errorf("bonnie: invalid completion phase %q", next.Phase)
		}
		if next.Candidate.ContinuationsUsed < 0 {
			return nil, errors.New("bonnie: invalid completion count")
		}
		c = &next
	}
	return c, nil
}

// completedAfter recognizes only a final text assistant message on the active
// branch after the pending watermark. Tool-call messages do not prove that a
// model turn finished. Usage in this crash window is not available from messages.
func completedAfter(s *Session, seq int) (string, bool) {
	if s.LastMessageSeq() <= seq {
		return "", false
	}
	msgs := s.GetMessages()
	if len(msgs) == 0 {
		return "", false
	}
	msg := msgs[len(msgs)-1]
	if msg.Role != "assistant" {
		return "", false
	}
	for _, part := range msg.Content {
		if _, ok := part.(kit.LLMToolCallPart); ok {
			return "", false
		}
	}
	text := messageText(msg)
	return text, text != ""
}

func addCompletionUsage(a, b *kit.LLMUsage) *kit.LLMUsage {
	if b == nil {
		return a
	}
	if a == nil {
		a = &kit.LLMUsage{}
	}
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.CacheCreationTokens += b.CacheCreationTokens
	a.CacheReadTokens += b.CacheReadTokens
	return a
}

// completionTurn runs the model/check loop. Suspension returns to the existing
// turn classifier. Resume replaces its pending prompt but keeps the budget.
func (r *Runner) completionTurn(ctx context.Context, s *Session, agent Agent, prompt string, files []kit.LLMFilePart, c *completionRecord, resumed bool) (*kit.TurnResult, error) {
	hook := r.completionHook
	if hook == nil {
		if capable, ok := agent.(CompletionAgent); ok {
			hook = capable.Complete
		}
	}
	if c == nil && hook == nil {
		return promptCompletion(ctx, agent, prompt, files)
	}
	book := context.WithoutCancel(ctx)
	if c != nil && !resumed {
		// Recovery owns its stored prompt, not new Start input or attachments.
		files = nil
	}
	if c == nil {
		c = &completionRecord{Phase: "pending", Prompt: prompt, MessageSeq: s.LastMessageSeq()}
	}
	if resumed {
		c.Phase = "pending"
		c.Prompt = prompt
		c.MessageSeq = s.LastMessageSeq()
		c.Feedback = CompletionFeedback{}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch c.Phase {
		case "pending":
			text, finished := completedAfter(s, c.MessageSeq)
			if !finished {
				if err := r.saveCompletion(book, s, c); err != nil {
					return nil, err
				}
				res, err := promptCompletion(ctx, agent, c.Prompt, files)
				files = nil
				if err != nil {
					return res, err
				}
				if res == nil {
					return nil, errors.New("bonnie: agent returned no turn result")
				}
				c.Usage = addCompletionUsage(c.Usage, res.TotalUsage)
				if _, suspended := suspensionFrom(res); suspended {
					c.MessageSeq = s.LastMessageSeq()
					if err := r.saveCompletion(book, s, c); err != nil {
						return nil, err
					}
					res.TotalUsage = c.Usage
					return res, nil
				}
				text = res.Response
			}
			c.Candidate.Response = text
			c.MessageSeq = s.LastMessageSeq()
			c.Phase = "check"
			if err := r.saveCompletion(book, s, c); err != nil {
				return nil, err
			}
		case "check":
			if hook == nil {
				return nil, errors.New("bonnie: completion check unavailable during recovery")
			}
			feedback, err := hook(ctx, c.Candidate)
			if err != nil {
				return nil, fmt.Errorf("bonnie: completion check: %w", err)
			}
			c.Feedback = feedback
			c.Phase = "accepted"
			if feedback.ContinueWith != "" {
				c.Phase = "continue"
			}
			if err := r.saveCompletion(book, s, c); err != nil {
				return nil, err
			}
		case "continue":
			if c.Candidate.ContinuationsUsed >= r.completionLimit {
				return nil, ErrContinuationLimit
			}
			c.Candidate.ContinuationsUsed++
			c.Prompt = c.Feedback.ContinueWith
			c.MessageSeq = s.LastMessageSeq()
			c.Phase = "pending"
			if err := r.saveCompletion(book, s, c); err != nil {
				return nil, err
			}
		case "accepted":
			return &kit.TurnResult{Response: c.Candidate.Response, TotalUsage: c.Usage}, nil
		}
	}
}

func promptCompletion(ctx context.Context, agent Agent, prompt string, files []kit.LLMFilePart) (*kit.TurnResult, error) {
	if len(files) == 0 {
		return agent.PromptResult(ctx, prompt)
	}
	capable, ok := agent.(FileAgent)
	if !ok {
		return nil, fmt.Errorf("%w: %T", ErrFilesUnsupported, agent)
	}
	return capable.PromptResultWithFiles(ctx, prompt, files)
}
