package langfuse_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/observe"
	"github.com/mark3labs/bonnie/observe/langfuse"
)

func mapped(t *testing.T, cfg langfuse.Config, attrs ...attribute.KeyValue) map[attribute.Key]attribute.Value {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	_, span := tp.Tracer("test").Start(context.Background(), "span")
	span.SetAttributes(attrs...)
	span.End()
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range cfg.Map(sr.Ended()[0]).Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestMapChatSpanToGeneration(t *testing.T) {
	t.Parallel()
	got := mapped(t, langfuse.Config{Environment: "staging", Release: "v1"},
		observe.KeyOperationName.String(observe.OperationChat),
		observe.KeyConversationID.String("run-1"),
		observe.KeyRunTitle.String("Deploy request"),
		observe.KeyInputMessages.String(`[{"role":"user"}]`),
		observe.KeyOutputMessages.String(`[{"role":"assistant"}]`),
		observe.KeyUsageInputTokens.Int64(10),
		observe.KeyUsageOutputTokens.Int64(4),
		observe.KeyUsageCacheReadTokens.Int64(7),
	)
	want := map[attribute.Key]string{
		langfuse.KeyObservationType:   "generation",
		langfuse.KeySessionID:         "run-1",
		langfuse.KeyTraceName:         "Deploy request",
		langfuse.KeyObservationInput:  `[{"role":"user"}]`,
		langfuse.KeyObservationOutput: `[{"role":"assistant"}]`,
		langfuse.KeyEnvironment:       "staging",
		langfuse.KeyRelease:           "v1",
	}
	for k, v := range want {
		if got[k].AsString() != v {
			t.Fatalf("%s = %q, want %q", k, got[k].AsString(), v)
		}
	}
	var usage map[string]int64
	if err := json.Unmarshal([]byte(got[langfuse.KeyUsageDetails].AsString()), &usage); err != nil {
		t.Fatalf("usage_details: %v", err)
	}
	if usage["input"] != 10 || usage["output"] != 4 || usage["cache_read_input_tokens"] != 7 {
		t.Fatalf("usage_details = %v", usage)
	}
	// Content moves; it is not sent twice.
	if _, ok := got[observe.KeyInputMessages]; ok {
		t.Fatal("gen_ai.input.messages was sent beside langfuse.observation.input")
	}
}

func TestMapObservationTypes(t *testing.T) {
	t.Parallel()
	for op, typ := range map[string]string{
		observe.OperationInvokeAgent: "agent",
		observe.OperationExecuteTool: "tool",
	} {
		got := mapped(t, langfuse.Config{}, observe.KeyOperationName.String(op), observe.KeyAgentName.String("ops"))
		if got[langfuse.KeyObservationType].AsString() != typ {
			t.Fatalf("%s maps to %q, want %q", op, got[langfuse.KeyObservationType].AsString(), typ)
		}
		if got[langfuse.KeyTraceName].AsString() != "ops" {
			t.Fatalf("trace name = %q, want the agent name when the run has no title", got[langfuse.KeyTraceName].AsString())
		}
		if _, ok := got[langfuse.KeyUsageDetails]; ok {
			t.Fatalf("%s span has usage_details; only generations carry usage", op)
		}
	}
	// A span BONNIE did not make keeps its own type inference.
	if _, ok := mapped(t, langfuse.Config{})[langfuse.KeyObservationType]; ok {
		t.Fatal("a foreign span got an observation type")
	}
}

func TestNewExporterRequiresKeys(t *testing.T) {
	t.Parallel()
	if _, err := langfuse.NewExporter(context.Background(), langfuse.Config{PublicKey: "pk"}); !errors.Is(err, langfuse.ErrMissingKeys) {
		t.Fatalf("err = %v, want ErrMissingKeys", err)
	}
	if _, err := langfuse.NewScorer(langfuse.Config{SecretKey: "sk"}); !errors.Is(err, langfuse.ErrMissingKeys) {
		t.Fatalf("err = %v, want ErrMissingKeys", err)
	}
}

// captured is one request a fake Langfuse received.
type captured struct {
	path, auth, version, contentType, encoding string
	body                                       []byte
}

func fakeLangfuse(t *testing.T) (*httptest.Server, func() []captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{
			path: r.URL.Path, auth: r.Header.Get("Authorization"),
			version: r.Header.Get("x-langfuse-ingestion-version"), contentType: r.Header.Get("Content-Type"),
			encoding: r.Header.Get("Content-Encoding"), body: body,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		return append([]captured(nil), got...)
	}
}

func TestTracerProviderSendsOTLPToLangfuse(t *testing.T) {
	t.Parallel()
	srv, requests := fakeLangfuse(t)
	ctx := context.Background()
	tp, err := langfuse.NewTracerProvider(ctx, langfuse.Config{Host: srv.URL + "/", PublicKey: "pk-lf", SecretKey: "sk-lf"}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("test").Start(ctx, "invoke_agent ops",
		trace.WithAttributes(observe.KeyOperationName.String(observe.OperationInvokeAgent), observe.KeyConversationID.String("run-9")))
	span.End()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	got := requests()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	r := got[0]
	if r.path != langfuse.TracesPath {
		t.Fatalf("path = %q", r.path)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf:sk-lf")); r.auth != want {
		t.Fatalf("Authorization = %q, want %q", r.auth, want)
	}
	if r.version != "4" {
		t.Fatalf("x-langfuse-ingestion-version = %q, want 4 for real-time ingestion", r.version)
	}
	if r.contentType != "application/x-protobuf" || r.encoding != "gzip" {
		t.Fatalf("content type %q encoding %q", r.contentType, r.encoding)
	}
	zr, err := gzip.NewReader(bytes.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	// The protobuf body holds the mapped attributes as plain strings.
	for _, want := range []string{"langfuse.observation.type", "agent", "langfuse.session.id", "run-9", "service.name", "ops"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("OTLP body has no %q", want)
		}
	}
}

func TestScorerRecordsApprovals(t *testing.T) {
	t.Parallel()
	srv, requests := fakeLangfuse(t)
	scorer, err := langfuse.NewScorer(langfuse.Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	waited := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
	})
	no := false
	scorer.Answer(context.Background(), observe.Answer{RunID: "r", Waited: waited, Approved: &no, Text: "too risky"})
	// Not an approval: nothing to score.
	scorer.Answer(context.Background(), observe.Answer{RunID: "r", Waited: waited, Text: "eu-west-1"})
	if err := scorer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := requests()
	if len(got) != 1 || got[0].path != langfuse.ScoresPath {
		t.Fatalf("requests = %+v, want one score", got)
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"traceId":       waited.TraceID().String(),
		"observationId": waited.SpanID().String(),
		"name":          langfuse.ApprovalScore,
		"value":         float64(0),
		"dataType":      "BOOLEAN",
		"comment":       "too risky",
		"environment":   "prod",
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("%s = %v, want %v (body %v)", k, body[k], v, body)
		}
	}
	if !strings.HasPrefix(got[0].auth, "Basic ") {
		t.Fatalf("Authorization = %q", got[0].auth)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk")
	t.Setenv("LANGFUSE_HOST", "https://old.example")
	t.Setenv("LANGFUSE_BASE_URL", "https://us.cloud.langfuse.com")
	t.Setenv("LANGFUSE_TRACING_ENVIRONMENT", "dev")
	t.Setenv("LANGFUSE_RELEASE", "v2")
	cfg := langfuse.ConfigFromEnv()
	if cfg.PublicKey != "pk" || cfg.SecretKey != "sk" || cfg.Environment != "dev" || cfg.Release != "v2" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Host != "https://us.cloud.langfuse.com" {
		t.Fatalf("host = %q, want LANGFUSE_BASE_URL to win over LANGFUSE_HOST", cfg.Host)
	}
}
