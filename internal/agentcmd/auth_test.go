package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
		{"login"}, {"login", "openai", "extra"}, {"login", "openai", "--region", "us-east-1"},
		{"logout"}, {"status", "extra"},
	} {
		if err := executeCommand(t.Context(), newAuthCommand(authActions{}), io.Discard, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, args := range [][]string{{"login", "openai"}, {"login", "groq", "--api-key-stdin"}, {"status"}, {"logout", "openai"}} {
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

// The real public SDK must reject Copilot and unknown providers before it
// reads a key or starts a browser login. Kit's model database names Copilot
// "github-copilot"; both spellings must reach the same refusal.
func TestAuthPublicSDKUnsupported(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"copilot", "github-copilot"} {
		err := executeCommand(t.Context(), NewAuthCommand(), io.Discard, "login", provider)
		if err == nil || !strings.Contains(err.Error(), "OAuth only") {
			t.Fatalf("%s login: %v", provider, err)
		}
	}
	err := executeCommand(t.Context(), NewAuthCommand(), io.Discard, "login", "no-such-provider")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown login: %v", err)
	}
}

// fakeProviders answers like Kit's model database for the IDs the tests use.
// "bedrock" is an alias whose ID is "amazon-bedrock", as in Kit.
func fakeProviders(name string) *kit.ProviderInfo {
	switch name {
	case "anthropic":
		return &kit.ProviderInfo{ID: "anthropic", Name: "Anthropic"}
	case "openai":
		return &kit.ProviderInfo{ID: "openai", Name: "OpenAI"}
	case "groq":
		return &kit.ProviderInfo{ID: "groq", Name: "Groq"}
	case "amazon-bedrock", "bedrock":
		return &kit.ProviderInfo{ID: "amazon-bedrock", Name: "Amazon Bedrock"}
	case "copilot", "github-copilot":
		return &kit.ProviderInfo{ID: "github-copilot", Name: "GitHub Copilot"}
	}
	return nil
}

type storedKey struct{ provider, key, region string }

// keyActions records each stored key. The OAuth login fails the test: an
// API-key login must never start a browser flow, also for openai.
func keyActions(t *testing.T, stored *[]storedKey, prompted string) authActions {
	return authActions{
		login: func(context.Context, string, kit.LoginOptions) error {
			t.Error("API-key login started OAuth")
			return nil
		},
		provider: fakeProviders,
		setKey: func(provider, key, region string) error {
			*stored = append(*stored, storedKey{provider, key, region})
			return nil
		},
		prompt: func(_ *cobra.Command, title string) (string, error) {
			if prompted == "" {
				return "", errNoKeyInput
			}
			if !strings.HasSuffix(title, "API key") {
				t.Errorf("prompt title: %q", title)
			}
			return prompted, nil
		},
	}
}

// A key from standard input or the hidden prompt is trimmed and stored under
// Kit's provider ID. The key never reaches the output.
func TestAuthLoginAPIKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   []string
		stdin  string
		prompt string
		want   storedKey
	}{
		{"stdin", []string{"login", "groq", "--api-key-stdin"}, "  gsk-secret\n", "", storedKey{"groq", "gsk-secret", ""}},
		{"prompt", []string{"login", "Anthropic"}, "", " sk-ant-secret ", storedKey{"anthropic", "sk-ant-secret", ""}},
		{"openai key", []string{"login", "openai", "--api-key-stdin"}, "sk-secret", "", storedKey{"openai", "sk-secret", ""}},
		{"bedrock alias and region", []string{"login", "bedrock", "--api-key-stdin", "--region", "eu-central-1"}, "br-secret", "", storedKey{"amazon-bedrock", "br-secret", "eu-central-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stored []storedKey
			cmd := newAuthCommand(keyActions(t, &stored, tc.prompt))
			cmd.SetIn(strings.NewReader(tc.stdin))
			var out bytes.Buffer
			if err := executeCommand(t.Context(), cmd, &out, tc.args...); err != nil {
				t.Fatal(err)
			}
			if len(stored) != 1 || stored[0] != tc.want {
				t.Fatalf("stored %+v, want %+v", stored, tc.want)
			}
			if !strings.Contains(out.String(), "API key saved") || strings.Contains(out.String(), "secret") {
				t.Fatalf("output: %q", out.String())
			}
		})
	}
}

// A refused API-key login stores nothing and does not report success.
func TestAuthLoginAPIKeyErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stdin, prompt, want string
		args                      []string
	}{
		{"unknown provider", "k", "", "unknown provider", []string{"login", "nope", "--api-key-stdin"}},
		{"copilot", "k", "", "OAuth only", []string{"login", "copilot", "--api-key-stdin"}},
		{"region on other provider", "k", "", "amazon-bedrock only", []string{"login", "groq", "--api-key-stdin", "--region", "us-east-1"}},
		{"empty stdin", " \n", "", "empty", []string{"login", "groq", "--api-key-stdin"}},
		{"empty prompt", "", "  ", "empty", []string{"login", "groq"}},
		{"no terminal", "piped-key", "", "--api-key-stdin", []string{"login", "groq"}},
		{"too long", strings.Repeat("k", maxAPIKeyBytes+1), "", "longer than", []string{"login", "groq", "--api-key-stdin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stored []storedKey
			cmd := newAuthCommand(keyActions(t, &stored, tc.prompt))
			cmd.SetIn(strings.NewReader(tc.stdin))
			var out bytes.Buffer
			err := executeCommand(t.Context(), cmd, &out, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error: %v, want %q", err, tc.want)
			}
			if len(stored) != 0 || strings.Contains(out.String(), "saved") {
				t.Fatalf("stored %+v, output %q", stored, out.String())
			}
		})
	}

	var stored []storedKey
	actions := keyActions(t, &stored, "")
	actions.setKey = func(string, string, string) error { return io.ErrUnexpectedEOF }
	cmd := newAuthCommand(actions)
	cmd.SetIn(strings.NewReader("k"))
	var out bytes.Buffer
	if err := executeCommand(t.Context(), cmd, &out, "login", "groq", "--api-key-stdin"); !errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(out.String(), "saved") {
		t.Fatalf("store error: %v, output %q", err, out.String())
	}
	cmd = newAuthCommand(keyActions(t, &stored, ""))
	cmd.SetIn(strings.NewReader("k"))
	if err := executeCommand(t.Context(), cmd, failingWriter{}, "login", "groq", "--api-key-stdin"); !errors.Is(err, errWrite) {
		t.Fatalf("output error: %v", err)
	}
}
