package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Login uses the command context and prints instructions without needing Kit CLI.
func TestAuthLogin(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	called := false
	actions := authActions{login: func(ctx context.Context, provider string, opts kit.LoginOptions) error {
		called = true
		if ctx != t.Context() || provider != "openai" || opts.CredentialManager != nil {
			t.Fatalf("login arguments: %v %s %+v", ctx, provider, opts)
		}
		return opts.OnAuthorization(ctx, kit.LoginInstructions{
			ProviderID: "openai", URL: "https://example.com/authorize", RedirectURI: "http://localhost:1455/auth/callback",
		})
	}}
	if err := executeCommand(t.Context(), newAuthCommand(actions), &out, "login", "openai"); err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(out.String(), "https://example.com/authorize") || !strings.Contains(out.String(), "Restart running agents") {
		t.Fatalf("login output: %q", out.String())
	}
}

// Failed login must not report that credentials were saved; errors remain testable.
func TestAuthLoginErrors(t *testing.T) {
	t.Parallel()
	for _, writer := range []io.Writer{io.Discard, failingWriter{}} {
		sentinel := errors.New("login failed")
		actions := authActions{login: func(ctx context.Context, _ string, opts kit.LoginOptions) error {
			if err := opts.OnAuthorization(ctx, kit.LoginInstructions{URL: "https://example.com"}); err != nil {
				return err
			}
			return sentinel
		}}
		err := executeCommand(t.Context(), newAuthCommand(actions), writer, "login", "openai")
		want := sentinel
		if _, ok := writer.(failingWriter); ok {
			want = errWrite
		}
		if !errors.Is(err, want) {
			t.Fatalf("error: %v, want %v", err, want)
		}
	}
	var out bytes.Buffer
	actions := authActions{login: func(context.Context, string, kit.LoginOptions) error { return context.DeadlineExceeded }}
	if err := executeCommand(t.Context(), newAuthCommand(actions), &out, "login", "openai"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error: %v", err)
	}
	if strings.Contains(out.String(), "saved") {
		t.Fatalf("false success: %q", out.String())
	}
	actions.login = func(context.Context, string, kit.LoginOptions) error { return nil }
	if err := executeCommand(t.Context(), newAuthCommand(actions), failingWriter{}, "login", "openai"); !errors.Is(err, errWrite) {
		t.Fatalf("success output error: %v", err)
	}
}

// Invalid arguments and cancellation must not read credentials or start OAuth.
func TestAuthValidationAndCancellation(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"login"}, {"login", "openai", "extra"}, {"login", "anthropic"},
		{"logout"}, {"status", "extra"},
	} {
		if err := executeCommand(t.Context(), newAuthCommand(authActions{}), io.Discard, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, args := range [][]string{{"login", "openai"}, {"status"}, {"logout", "openai"}} {
		if err := executeCommand(ctx, newAuthCommand(authActions{}), io.Discard, args...); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// Status uses an explicit safe projection, in stable order, with no token refresh.
func TestAuthStatus(t *testing.T) {
	t.Parallel()
	store := &kit.CredentialStore{
		OpenAI:    &kit.OpenAICredentials{Type: "oauth", AccessToken: "SECRET-ACCESS", RefreshToken: "SECRET-REFRESH", AccountID: "SECRET-ACCOUNT"},
		Anthropic: &kit.AnthropicCredentials{Type: "api_key", APIKey: "SECRET-KEY"},
		Copilot:   &kit.CopilotCredentials{Type: "oauth", GitHubToken: "SECRET-GITHUB", CopilotAccessToken: "SECRET-COPILOT"},
		Providers: map[string]*kit.ProviderCredentials{"groq": {Type: "api_key", APIKey: "SECRET-GROQ"}, "unused": nil},
	}
	actions := authActions{load: func() (*kit.CredentialStore, error) { return store, nil }}
	var out bytes.Buffer
	if err := executeCommand(t.Context(), newAuthCommand(actions), &out, "status"); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "anthropic\tapi_key\ncopilot\toauth\ngroq\tapi_key\nopenai\toauth\n"; got != want {
		t.Fatalf("status: %q, want %q", got, want)
	}
	if err := executeCommand(t.Context(), newAuthCommand(actions), failingWriter{}, "status"); !errors.Is(err, errWrite) {
		t.Fatalf("writer error: %v", err)
	}
	for _, empty := range []*kit.CredentialStore{nil, {}} {
		actions.load = func() (*kit.CredentialStore, error) { return empty, nil }
		out.Reset()
		if err := executeCommand(t.Context(), newAuthCommand(actions), &out, "status"); err != nil || out.String() != "No stored provider credentials.\n" {
			t.Fatalf("empty status: %q, %v", out.String(), err)
		}
		if err := executeCommand(t.Context(), newAuthCommand(actions), failingWriter{}, "status"); !errors.Is(err, errWrite) {
			t.Fatalf("empty output error: %v", err)
		}
	}
	actions.load = func() (*kit.CredentialStore, error) { return nil, io.ErrUnexpectedEOF }
	if err := executeCommand(t.Context(), newAuthCommand(actions), io.Discard, "status"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("load error: %v", err)
	}
}

// Logout changes only the requested stored provider and examines all errors.
func TestAuthLogout(t *testing.T) {
	t.Parallel()
	var removed string
	actions := authActions{remove: func(provider string) error { removed = provider; return nil }}
	var out bytes.Buffer
	if err := executeCommand(t.Context(), newAuthCommand(actions), &out, "logout", "openai"); err != nil {
		t.Fatal(err)
	}
	if removed != "openai" || !strings.Contains(out.String(), "running agents are unchanged") {
		t.Fatalf("logout: %q, %q", removed, out.String())
	}
	if err := executeCommand(t.Context(), newAuthCommand(actions), failingWriter{}, "logout", "openai"); !errors.Is(err, errWrite) {
		t.Fatalf("output error: %v", err)
	}
	actions.remove = func(string) error { return io.ErrUnexpectedEOF }
	if err := executeCommand(t.Context(), newAuthCommand(actions), io.Discard, "logout", "openai"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("remove error: %v", err)
	}
}

// The real public SDK must reject unsupported login before any browser interaction.
func TestAuthPublicSDKUnsupported(t *testing.T) {
	t.Parallel()
	err := executeCommand(t.Context(), NewAuthCommand(), io.Discard, "login", "copilot")
	if err == nil || !strings.Contains(err.Error(), "only openai") {
		t.Fatalf("unsupported login: %v", err)
	}
}
