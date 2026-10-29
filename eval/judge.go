package eval

import (
	"context"
	"errors"
	"fmt"
	"math"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Question is one typed question for the decision judge. Build it with
// [Bool], [Rubric] or [Choice]. Its ID labels the score.
type Question struct {
	q kit.DecisionQuestion
	// expected is the option a choice question wants. It is never sent to
	// the model.
	expected string
}

// ID returns the question's ID.
func (q Question) ID() string { return q.q.ID }

// Bool returns a yes-or-no question. The score is the model's P(true): an
// estimate, not a calibrated confidence.
func Bool(id, instructions string) Question {
	return Question{q: kit.BooleanQuestion(id, instructions)}
}

// Rubric returns a question answered with a position on levels, worst
// first. It needs two levels or more. The score is the expected position
// divided by the highest position, so it is from 0 to 1.
func Rubric(id, instructions string, levels ...string) Question {
	return Question{q: kit.ScoreQuestion(id, instructions, levels...)}
}

// Choice returns a question answered with one of options. The score is the
// probability of the expected option, or 1 or 0 when the model gives no
// distribution. expected is not sent to the model.
func Choice(id, instructions, expected string, options ...kit.DecisionOption) Question {
	return Question{q: kit.ChoiceQuestion(id, instructions, options...), expected: expected}
}

// JudgeOption changes one judgment.
type JudgeOption func(*judgeConfig)

type judgeConfig struct {
	state     *kit.DecisionState
	reference string
	model     string
}

// On sets what the judge grades in place of the default. A string is sent as
// text, and any other value as JSON. The default is the input and the reply
// of the latest turn, as JSON with the keys "input" and "response".
func On(value any) JudgeOption {
	return func(c *judgeConfig) {
		if s, ok := value.(string); ok {
			c.state = &kit.DecisionState{Text: s}
			return
		}
		c.state = &kit.DecisionState{JSON: value}
	}
}

// Reference adds a reference answer to the default state, under the key
// "reference". It has no effect with [On].
func Reference(answer string) JudgeOption {
	return func(c *judgeConfig) { c.reference = answer }
}

// Model sets the judge model of one judgment.
func Model(model string) JudgeOption {
	return func(c *judgeConfig) { c.model = model }
}

// Judge asks the decision judge one question about the latest turn and
// returns its score. See [T.JudgeAll].
func (e *T) Judge(q Question, opts ...JudgeOption) *Score {
	e.tb.Helper()
	return e.JudgeAll([]Question{q}, opts...)[q.ID()]
}

// JudgeAll asks the decision judge each question in one request, about the
// same state, and returns one score for each question, by ID. One request
// for many questions costs less than one for each.
//
// The model is the one of [Model], then [WithJudge], then [EnvJudge]. With
// [WithDecider], no model is needed. A judgment with no model, or one the
// model does not answer, fails the eval.
func (e *T) JudgeAll(qs []Question, opts ...JudgeOption) map[string]*Score {
	e.tb.Helper()
	scores := make(map[string]*Score, len(qs))
	for _, q := range qs {
		if _, dup := scores[q.ID()]; dup {
			e.tb.Errorf("bonnie: eval: question ID %q is used twice", q.ID())
			continue
		}
		scores[q.ID()] = e.newScore(KindJudge, q.ID())
	}
	fail := func(err error) map[string]*Score {
		e.tb.Helper()
		for _, s := range scores {
			s.fail(err)
		}
		return scores
	}
	var c judgeConfig
	for _, opt := range opts {
		opt(&c)
	}
	model := c.model
	if model == "" {
		model = e.cfg.judge
	}
	decide := e.cfg.decider
	if decide == nil {
		if model == "" {
			return fail(fmt.Errorf("%w: use eval.WithJudge or set %s", ErrNoJudge, EnvJudge))
		}
		decide = kit.Decide
	}
	state, err := e.judgeState(c)
	if err != nil {
		return fail(err)
	}
	req := kit.DecisionRequest{Model: model, State: state}
	for _, q := range qs {
		req.Questions = append(req.Questions, q.q)
	}
	res, err := decide(e.tb.Context(), req)
	if err != nil {
		return fail(fmt.Errorf("judge: %w", err))
	}
	for _, q := range qs {
		s := scores[q.ID()]
		s.model = res.Model
		if s.model == "" {
			s.model = model
		}
		a, ok := res.Answer(q.ID())
		if !ok {
			s.fail(fmt.Errorf("judge: %w %q", kit.ErrNoDecisionAnswer, q.ID()))
			continue
		}
		v, err := normalize(q.q, a, q.expected)
		if err != nil {
			s.fail(fmt.Errorf("judge: %w", err))
			continue
		}
		s.value = v
	}
	return scores
}

// judgeState returns what a judgment grades.
func (e *T) judgeState(c judgeConfig) (kit.DecisionState, error) {
	if c.state != nil {
		return *c.state, nil
	}
	last := e.Last()
	if last == nil {
		return kit.DecisionState{}, errors.New("no turn to judge: call Send first, or give eval.On")
	}
	v := map[string]string{"input": last.input, "response": last.Reply()}
	if c.reference != "" {
		v["reference"] = c.reference
	}
	return kit.DecisionState{JSON: v}, nil
}

// normalize turns an answer into a score from 0 to 1.
func normalize(q kit.DecisionQuestion, a kit.DecisionAnswer, expected string) (float64, error) {
	if a.Refused {
		return 0, fmt.Errorf("%w %q", kit.ErrDecisionRefused, q.ID)
	}
	var v float64
	switch q.Type {
	case kit.DecisionBoolean:
		v = a.Probability
	case kit.DecisionScore:
		if len(q.Levels) < 2 {
			return 0, fmt.Errorf("question %q has fewer than 2 levels", q.ID)
		}
		v = a.Score / float64(len(q.Levels)-1)
	case kit.DecisionChoice:
		if expected == "" {
			return 0, fmt.Errorf("choice question %q has no expected option", q.ID)
		}
		if p, ok := a.Prob(expected); ok {
			v = p
		} else if a.Choice == expected {
			v = 1
		}
	default:
		return 0, fmt.Errorf("question %q has type %q", q.ID, q.Type)
	}
	if math.IsNaN(v) || v < 0 || v > 1 {
		return 0, fmt.Errorf("answer to %q gives score %v, not from 0 to 1", q.ID, v)
	}
	return v, nil
}

// LLMJudgeFunc sends one prompt to a language model and returns its text
// reply. system holds the grading instructions.
type LLMJudgeFunc func(ctx context.Context, system, prompt string) (string, error)
