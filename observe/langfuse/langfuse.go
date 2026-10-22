// Package langfuse sends BONNIE traces to Langfuse.
//
// Langfuse ingests OpenTelemetry directly (OTLP over HTTP at
// /api/public/otel/v1/traces), so the adapter is an exporter, not a client
// SDK. [NewExporter] configures OTLP for Langfuse and maps BONNIE's spans to
// Langfuse's data model:
//
//   - invoke_agent spans become agent observations, chat spans become
//     generations with model and usage, execute_tool spans become tool
//     observations;
//   - the run ID becomes the Langfuse session, on every span, so that
//     filtering by session works on each observation;
//   - the run title becomes the trace name;
//   - gen_ai.input.messages and gen_ai.output.messages become the
//     observation's input and output, when the capture policy recorded them.
//
// Langfuse only accepts OTLP over HTTP. It shows data in real time only when
// the exporter sends x-langfuse-ingestion-version: 4, which NewExporter
// does.
//
// A [Scorer] records each human approval as a Langfuse score, through the
// public REST API, which can show data up to about ten minutes late.
package langfuse

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// DefaultHost is Langfuse Cloud's EU region. Other regions are
// https://us.cloud.langfuse.com, https://jp.cloud.langfuse.com, and
// https://hipaa.cloud.langfuse.com. A self-hosted Langfuse needs v3.22.0 or
// later for OpenTelemetry ingestion.
const DefaultHost = "https://cloud.langfuse.com"

// TracesPath is Langfuse's OTLP trace endpoint, relative to the host.
const TracesPath = "/api/public/otel/v1/traces"

// ErrMissingKeys is returned when a Config has no public or secret key.
var ErrMissingKeys = errors.New("bonnie: langfuse: public key and secret key are required")

// Config is a Langfuse project.
type Config struct {
	// Host is the Langfuse base URL. Empty means [DefaultHost].
	Host string
	// PublicKey and SecretKey are the project's API keys. They
	// authenticate with HTTP Basic auth.
	PublicKey string
	SecretKey string
	// Environment sets langfuse.environment on every span: a lowercase
	// name such as "production". Empty leaves it unset.
	Environment string
	// Release sets langfuse.release on every span. Empty leaves it unset.
	Release string
	// HTTPClient sends score requests. Nil uses a client with a 10 second
	// timeout. Spans use the OTLP exporter's own client.
	HTTPClient *http.Client
}

// ConfigFromEnv reads the variables the Langfuse SDKs use:
// LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY, LANGFUSE_BASE_URL (or the older
// LANGFUSE_HOST), LANGFUSE_TRACING_ENVIRONMENT, and LANGFUSE_RELEASE.
func ConfigFromEnv() Config {
	host := os.Getenv("LANGFUSE_BASE_URL")
	if host == "" {
		host = os.Getenv("LANGFUSE_HOST")
	}
	return Config{
		Host:        host,
		PublicKey:   os.Getenv("LANGFUSE_PUBLIC_KEY"),
		SecretKey:   os.Getenv("LANGFUSE_SECRET_KEY"),
		Environment: os.Getenv("LANGFUSE_TRACING_ENVIRONMENT"),
		Release:     os.Getenv("LANGFUSE_RELEASE"),
	}
}

func (c Config) host() string {
	if c.Host == "" {
		return DefaultHost
	}
	return strings.TrimRight(c.Host, "/")
}

func (c Config) authorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.PublicKey+":"+c.SecretKey))
}

func (c Config) validate() error {
	if c.PublicKey == "" || c.SecretKey == "" {
		return ErrMissingKeys
	}
	return nil
}

// NewExporter returns an OTLP exporter that sends spans to Langfuse, mapped
// to its data model. Wrap it with observe.NewExporter to filter what this
// destination receives.
func NewExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	otlp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.host()+TracesPath),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization":                cfg.authorization(),
			"x-langfuse-ingestion-version": "4",
		}),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
	)
	if err != nil {
		return nil, fmt.Errorf("bonnie: langfuse: exporter: %w", err)
	}
	return &exporter{next: otlp, cfg: cfg}, nil
}

// NewTracerProvider returns a TracerProvider that batches spans to Langfuse.
// service names the agent in the trace's resource. opts are applied after
// BONNIE's own, so an option such as a sampler or a second exporter can be
// added. The caller must call Shutdown before the process exits, or the
// last batch is lost.
func NewTracerProvider(ctx context.Context, cfg Config, service string, opts ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	exp, err := NewExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", service)))
	if err != nil {
		return nil, fmt.Errorf("bonnie: langfuse: resource: %w", err)
	}
	base := []sdktrace.TracerProviderOption{
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
	}
	return sdktrace.NewTracerProvider(append(base, opts...)...), nil
}
