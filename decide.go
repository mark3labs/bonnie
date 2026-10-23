package bonnie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// Decider sends one request to a decision model. [WithDecider] installs one
// in place of the provider call.
type Decider = runtime.Decider

// DecisionRequest is one request to the agent's decision model. See
// [kit.DecisionRequest] for the model, state and question fields.
type DecisionRequest struct {
	// Model is a decision model in "provider/model" format. Empty uses
	// the model of [WithDecisionModel].
	Model     string
	State     kit.DecisionState
	Questions []kit.DecisionQuestion
	// ProviderOptions are sent as-is on wires that accept them.
	ProviderOptions map[string]any
	// Key makes a call in a run idempotent. When the run already has a
	// successful decision with the same key and input, the stored result is
	// returned and the model is not asked again, also after a restart. Use
	// a key that names the decision, for example the ID of the tool call it
	// gates. A key used again with different input returns
	// [runtime.ErrDecisionKeyReused]. Empty asks the model each time.
	// [Agent.Decide] has no run, so it ignores Key.
	Key string
}

// WithDecisionModel sets the decision model the agent uses, in
// "provider/model" format, for example "openrouter/typesafe/jev-1.14" or
// "ollama/pplx-decider". See [kit.GetDecisionModels].
//
// A decision model answers typed questions with probabilities. It does not
// chat. The option makes [RunScope.Decide] available to tool factories, Kit
// setup callbacks and completion hooks, and makes [Agent.Decide] available
// to host code outside a run. [WithDecisionTool] also gives it to the model.
// A probability is not an authorization: enforce access rules in host code.
//
// [Agent.Run] refuses a model that Kit cannot call as a decision model. It
// does not check credentials.
func WithDecisionModel(model string) Option {
	return func(c *config) { c.decisionModel = model }
}

// WithDecider replaces the provider call with d. A test uses it to script
// answers, and a host uses it to reach a model Kit cannot call. Each call in
// a run is still journalled. When it is set, [Agent.Run] does not check the
// model of [WithDecisionModel] against the model database.
func WithDecider(d Decider) Option {
	return func(c *config) { c.decider = d }
}

// WithDecisionTool gives the model the "decide" tool, which asks the
// decision model typed questions about a text. It needs [WithDecisionModel]
// or [WithDecider], and it cannot be combined with [WithAgentFactory].
//
// Each call is journalled with the tool call's ID as its key, and the tool
// is replay-safe: a call interrupted by a crash returns the stored answer
// when the answer was journalled, and asks again when it was not.
func WithDecisionTool() Option {
	return func(c *config) { c.decisionTool = true }
}

// Decide asks the agent's decision model outside a run, for example in a
// schedule's Prepare function or in a channel that decides whether to
// answer. There is no run, so no journal record is written and Key is
// ignored. In a run, use [RunScope.Decide], which journals each call.
//
// It uses Kit's default settings: API keys from stored credentials or the
// environment, and no HTTP transport from [WithKit].
func (a *Agent) Decide(ctx context.Context, req DecisionRequest) (*kit.DecisionResult, error) {
	d := a.cfg.decider
	if d == nil {
		d = kit.Decide
	}
	kreq, err := a.cfg.decisionRequest(req)
	if err != nil {
		return nil, err
	}
	return d(ctx, kreq)
}

// decisionEnabled reports whether a run gets [RunScope.Decide].
func (c *config) decisionEnabled() bool {
	return c.decisionModel != "" || c.decider != nil
}

// decisionRequest fills the default model.
func (c *config) decisionRequest(req DecisionRequest) (kit.DecisionRequest, error) {
	if !c.decisionEnabled() {
		return kit.DecisionRequest{}, runtime.ErrNoDecider
	}
	if req.Model == "" {
		req.Model = c.decisionModel
	}
	return kit.DecisionRequest{Model: req.Model, State: req.State, Questions: req.Questions, ProviderOptions: req.ProviderOptions}, nil
}

// checkDecision refuses a decision configuration that cannot work, before
// the first run.
//
// TODO(kit): Kit exports no function that resolves a decision model without
// a request. The catalog alone is not enough: Kit accepts any model of some
// providers (local Ollama, TypeSafe). So the check sends an empty request:
// Kit resolves the model, then rejects the request before it sends anything.
// Only the model is checked: an error from the environment, such as a
// missing API key, shows on the first call.
func (c *config) checkDecision() error {
	if c.decisionTool && !c.decisionEnabled() {
		return fmt.Errorf("bonnie: WithDecisionTool needs WithDecisionModel or WithDecider")
	}
	if c.decisionModel == "" || c.decider != nil {
		return nil
	}
	if _, _, err := kit.ParseModelString(c.decisionModel); err != nil {
		return fmt.Errorf("bonnie: WithDecisionModel %q: %w", c.decisionModel, err)
	}
	_, err := kit.Decide(context.Background(), kit.DecisionRequest{Model: c.decisionModel})
	if errors.Is(err, kit.ErrNotDecisionModel) || errors.Is(err, kit.ErrUnknownDecisionWire) {
		return fmt.Errorf("bonnie: WithDecisionModel %q: %w (see kit.GetDecisionModels)", c.decisionModel, err)
	}
	return nil
}

// decideFunc asks the decision model in one execution and names the
// caller in the journal record.
type decideFunc func(ctx context.Context, req DecisionRequest, source string) (*kit.DecisionResult, error)

// runDecide returns the decision function of one execution, or nil. The
// Kit's own Decide applies its HTTP transport and provider overrides.
func (c *config) runDecide(k *kit.Kit, s *runtime.Session) decideFunc {
	if !c.decisionEnabled() {
		return nil
	}
	d := c.decider
	if d == nil {
		d = k.Decide
	}
	return func(ctx context.Context, req DecisionRequest, source string) (*kit.DecisionResult, error) {
		kreq, err := c.decisionRequest(req)
		if err != nil {
			return nil, err
		}
		return s.Decide(ctx, d, runtime.DecisionCall{Request: kreq, Key: req.Key, Source: source})
	}
}

// hostDecide is [RunScope.Decide] for decide, or nil.
func hostDecide(decide decideFunc) func(context.Context, DecisionRequest) (*kit.DecisionResult, error) {
	if decide == nil {
		return nil
	}
	return func(ctx context.Context, req DecisionRequest) (*kit.DecisionResult, error) {
		return decide(ctx, req, runtime.DecisionSourceHost)
	}
}

// DecideToolName is the name of the tool [WithDecisionTool] adds.
const DecideToolName = "decide"

// Limits of the decide tool. They bound what one model call can spend.
const (
	decideMaxState     = 32 << 10
	decideMaxQuestions = 16
)

type decideOption struct {
	Name        string `json:"name" description:"The answer the model returns when it selects this option."`
	Description string `json:"description,omitempty" description:"What the option means."`
}

type decideQuestion struct {
	ID           string         `json:"id" description:"Unique ID of the question. The answer has the same ID."`
	Type         string         `json:"type" description:"boolean (answered with P(true)), choice (one of options) or score (a position on levels)."`
	Instructions string         `json:"instructions" description:"The question."`
	Options      []decideOption `json:"options,omitempty" description:"For a choice question: 2 to 255 options, in order."`
	Levels       []string       `json:"levels,omitempty" description:"For a score question: 2 to 10 levels, lowest first."`
}

type decideInput struct {
	State     string           `json:"state" description:"The text the questions are about."`
	Questions []decideQuestion `json:"questions" description:"1 to 16 typed questions."`
}

// decideTool returns the model-facing decide tool of one execution.
func decideTool(decide decideFunc) kit.Tool {
	return kit.NewTool(DecideToolName,
		"Ask a decision model typed questions about a text. It returns calibrated probabilities, not prose: use it to classify, route or score many items cheaply. A probability is not permission to act.",
		func(ctx context.Context, in decideInput) (kit.ToolOutput, error) {
			req, err := in.request()
			if err != nil {
				return kit.ErrorResult(err.Error()), nil
			}
			req.Key = "tool:" + kit.ToolCallIDFromContext(ctx)
			res, err := decide(ctx, req, runtime.DecisionSourceTool)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return kit.ToolOutput{}, err
				}
				return kit.ErrorResult(decideErrorText(err)), nil
			}
			out, err := json.Marshal(struct {
				Answers []kit.DecisionAnswer `json:"answers"`
			}{res.Answers})
			if err != nil {
				return kit.ToolOutput{}, fmt.Errorf("bonnie: encode decision: %w", err)
			}
			return kit.TextResult(string(out)), nil
		})
}

func (in decideInput) request() (DecisionRequest, error) {
	switch {
	case strings.TrimSpace(in.State) == "":
		return DecisionRequest{}, errors.New("state is empty")
	case len(in.State) > decideMaxState:
		return DecisionRequest{}, fmt.Errorf("state is %d bytes; the limit is %d", len(in.State), decideMaxState)
	case len(in.Questions) == 0 || len(in.Questions) > decideMaxQuestions:
		return DecisionRequest{}, fmt.Errorf("give 1 to %d questions, not %d", decideMaxQuestions, len(in.Questions))
	}
	req := DecisionRequest{State: kit.DecisionState{Text: in.State}}
	for _, q := range in.Questions {
		var kq kit.DecisionQuestion
		switch kit.DecisionQuestionType(q.Type) {
		case kit.DecisionBoolean:
			kq = kit.BooleanQuestion(q.ID, q.Instructions)
		case kit.DecisionChoice:
			opts := make([]kit.DecisionOption, len(q.Options))
			for i, o := range q.Options {
				opts[i] = kit.DecisionOption{Name: o.Name, Description: o.Description}
			}
			kq = kit.ChoiceQuestion(q.ID, q.Instructions, opts...)
		case kit.DecisionScore:
			kq = kit.ScoreQuestion(q.ID, q.Instructions, q.Levels...)
		default:
			return DecisionRequest{}, fmt.Errorf("question %q: type %q is not boolean, choice or score", q.ID, q.Type)
		}
		req.Questions = append(req.Questions, kq)
	}
	return req, nil
}

// decideErrorText tells the model what failed without the provider's
// response body, which can hold sensitive data.
func decideErrorText(err error) string {
	if apiErr, ok := errors.AsType[*kit.DecisionAPIError](err); ok {
		return fmt.Sprintf("decision model unavailable (HTTP %d)", apiErr.StatusCode)
	}
	for _, target := range []error{kit.ErrInvalidDecisionRequest, kit.ErrInvalidDecisionResponse, kit.ErrDecisionRefused, runtime.ErrDecisionKeyReused} {
		if errors.Is(err, target) {
			return err.Error()
		}
	}
	return "decision failed"
}
