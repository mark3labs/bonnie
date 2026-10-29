package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// llmJudgeSystem is the grading instruction of [T.JudgeLLM].
const llmJudgeSystem = `You grade the work of an AI agent against one criterion.
Read the state, then decide how well it meets the criterion.
Reply with one JSON object and nothing else:
{"score": <number from 0 to 1>, "reason": "<one sentence>"}
1 means that the criterion is fully met. 0 means that it is not met.`

// JudgeLLM asks a language model how well the latest turn meets criterion,
// and returns its score with the model's reason. The options are those of
// [T.Judge].
//
// Prefer [T.Judge]. A decision model returns a probability. A language
// model writes a number that the eval parses, which costs more and is not
// calibrated. Use JudgeLLM when no decision model fits the question, or when
// the written reason helps.
//
// The model is the one of [Model], then [WithLLMJudge], then [EnvLLMJudge].
// With [WithLLMJudgeFunc], no model is needed. A judgment with no model, or
// a reply that is not a score, fails the eval.
func (e *T) JudgeLLM(criterion string, opts ...JudgeOption) *Score {
	e.tb.Helper()
	s := e.newScore(KindLLM, criterion)
	var c judgeConfig
	for _, opt := range opts {
		opt(&c)
	}
	model := c.model
	if model == "" {
		model = e.cfg.llmJudge
	}
	ask := e.cfg.llmFunc
	if ask == nil {
		if model == "" {
			s.fail(fmt.Errorf("%w: use eval.WithLLMJudge or set %s", ErrNoJudge, EnvLLMJudge))
			return s
		}
		ask = modelJudge(model)
	}
	s.model = model
	state, err := e.judgeState(c)
	if err != nil {
		s.fail(err)
		return s
	}
	text := state.Text
	if state.JSON != nil {
		b, err := json.MarshalIndent(state.JSON, "", "  ")
		if err != nil {
			s.fail(fmt.Errorf("encode state: %w", err))
			return s
		}
		text = strings.TrimSpace(text + "\n" + string(b))
	}
	reply, err := ask(e.tb.Context(), llmJudgeSystem, "Criterion: "+criterion+"\n\nState:\n"+text)
	if err != nil {
		s.fail(fmt.Errorf("llm judge: %w", err))
		return s
	}
	v, reason, err := parseLLMScore(reply)
	if err != nil {
		s.fail(fmt.Errorf("llm judge: %w", err))
		return s
	}
	s.value, s.reason = v, reason
	return s
}

// parseLLMScore reads the JSON object of a judge reply. It accepts text
// around the object, because models often add a code fence.
func parseLLMScore(reply string) (float64, string, error) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return 0, "", fmt.Errorf("reply has no JSON object: %s", short(reply))
	}
	var out struct {
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &out); err != nil {
		return 0, "", fmt.Errorf("decode reply %s: %w", short(reply), err)
	}
	if out.Score == nil {
		return 0, "", fmt.Errorf("reply has no score: %s", short(reply))
	}
	if v := *out.Score; math.IsNaN(v) || v < 0 || v > 1 {
		return 0, "", fmt.Errorf("score %v is not from 0 to 1", v)
	}
	return *out.Score, out.Reason, nil
}

// modelJudge calls model through Kit's provider for one completion. It uses
// API keys from stored credentials or the environment.
func modelJudge(model string) LLMJudgeFunc {
	return func(ctx context.Context, system, prompt string) (string, error) {
		p, err := kit.CreateProvider(ctx, &kit.ProviderConfig{ModelString: model})
		if err != nil {
			return "", err
		}
		if p.Closer != nil {
			defer func() { _ = p.Closer.Close() }()
		}
		if p.Model == nil {
			return "", errors.New("provider returned no model")
		}
		temperature := 0.0
		resp, err := p.Model.Generate(ctx, kit.LLMCall{
			Prompt: []kit.LLMMessage{
				{Role: kit.LLMRoleSystem, Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: system}}},
				{Role: kit.LLMRoleUser, Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: prompt}}},
			},
			Temperature: &temperature,
		})
		if err != nil {
			return "", err
		}
		return resp.Content.Text(), nil
	}
}
