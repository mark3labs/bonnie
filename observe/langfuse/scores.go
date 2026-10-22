package langfuse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/mark3labs/bonnie/observe"
)

// ScoresPath is Langfuse's score endpoint, relative to the host.
const ScoresPath = "/api/public/scores"

// ApprovalScore is the name of the score a [Scorer] records.
const ApprovalScore = "approval"

// Scorer records each human approval as a BOOLEAN Langfuse score on the
// trace and observation of the turn that asked for it. Pass its Answer
// method to observe.WithAnswerHook. Answers that are not approvals are not
// recorded.
//
// A score is best effort: it is sent in the background, and a failure goes
// to the OpenTelemetry error handler (otel.Handle) and never to the run.
// Call [Scorer.Close] before the process exits to wait for the scores in
// flight.
type Scorer struct {
	cfg    Config
	client *http.Client
	wg     sync.WaitGroup
}

// NewScorer returns a Scorer for cfg.
func NewScorer(cfg Config) (*Scorer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Scorer{cfg: cfg, client: client}, nil
}

// score is the body of POST /api/public/scores (CreateScoreRequest).
type score struct {
	TraceID       string  `json:"traceId"`
	ObservationID string  `json:"observationId,omitempty"`
	Name          string  `json:"name"`
	Value         float64 `json:"value"`
	DataType      string  `json:"dataType"`
	Comment       string  `json:"comment,omitempty"`
	Environment   string  `json:"environment,omitempty"`
}

// Answer records a as a score when it is an approval. It returns at once.
func (s *Scorer) Answer(ctx context.Context, a observe.Answer) {
	if a.Approved == nil || !a.Waited.IsValid() {
		return
	}
	body := score{
		TraceID:       a.Waited.TraceID().String(),
		ObservationID: a.Waited.SpanID().String(),
		Name:          ApprovalScore,
		DataType:      "BOOLEAN",
		Comment:       a.Text,
		Environment:   s.cfg.Environment,
	}
	if *a.Approved {
		body.Value = 1
	}
	ctx = context.WithoutCancel(ctx)
	s.wg.Go(func() {
		if err := s.post(ctx, body); err != nil {
			otel.Handle(err)
		}
	})
}

func (s *Scorer) post(ctx context.Context, body score) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("bonnie: langfuse: encode score: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.host()+ScoresPath, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("bonnie: langfuse: score request: %w", err)
	}
	req.Header.Set("Authorization", s.cfg.authorization())
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("bonnie: langfuse: send score: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bonnie: langfuse: send score: %s", resp.Status)
	}
	return nil
}

// Close waits for the scores in flight, or until ctx ends.
func (s *Scorer) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("bonnie: langfuse: wait for scores: %w", ctx.Err())
	}
}
