package agentcmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A second command reads the shared Kit store, without showing secrets. Logout
// removes only its provider. Serial execution permits an isolated XDG location.
func TestAuthSharedCredentialStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cm, err := kit.NewCredentialManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.SetOpenAIOAuthCredentials(&kit.OpenAICredentials{
		Type: "oauth", AccessToken: "secret-access", RefreshToken: "secret-refresh", AccountID: "secret-account",
	}); err != nil {
		t.Fatal(err)
	}
	if err := kit.SetProviderAPIKey("groq", "secret-key"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := executeCommand(t.Context(), NewAuthCommand(), &out, "status"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "groq\tapi_key\nopenai\toauth\n" || strings.Contains(out.String(), "secret") {
		t.Fatalf("status output: %q", out.String())
	}
	info, err := os.Stat(cm.GetCredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions: %v", info.Mode())
	}
	out.Reset()
	if err := executeCommand(t.Context(), NewAuthCommand(), &out, "logout", "openai"); err != nil {
		t.Fatal(err)
	}
	second, err := kit.NewCredentialManager()
	if err != nil {
		t.Fatal(err)
	}
	store, err := second.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if store.OpenAI != nil || store.Providers["groq"] == nil || store.Providers["groq"].APIKey != "secret-key" {
		t.Fatal("logout did not preserve other provider credentials")
	}
}

// An API key stored by login is the key Kit reads back, in the same shared
// file. A Bedrock key keeps its region when it is rotated without --region,
// as Kit's own login does. Kit validates an Anthropic key's format, so the
// fake key has the real prefix and length. Serial execution permits an isolated XDG location.
func TestAuthLoginAPIKeyStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	login := func(stdin string, args ...string) {
		t.Helper()
		cmd := NewAuthCommand()
		cmd.SetIn(strings.NewReader(stdin))
		var out bytes.Buffer
		if err := executeCommand(t.Context(), cmd, &out, append([]string{"login"}, args...)...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out.String(), "secret") {
			t.Fatalf("output shows the key: %q", out.String())
		}
	}
	login("gsk-secret\n", "groq", "--api-key-stdin")
	login("sk-ant-secret-0123456789\n", "anthropic", "--api-key-stdin")
	login("br-secret-1", "amazon-bedrock", "--api-key-stdin", "--region", "eu-central-1")
	login("br-secret-2", "bedrock", "--api-key-stdin")

	if got := kit.GetProviderAPIKey("groq"); got != "gsk-secret" {
		t.Fatalf("groq key: %q", got)
	}
	if got := kit.GetAnthropicAPIKey(); got != "sk-ant-secret-0123456789" {
		t.Fatalf("anthropic key: %q", got)
	}
	cm, err := kit.NewCredentialManager()
	if err != nil {
		t.Fatal(err)
	}
	key, region, err := cm.GetProviderAPIKeyWithRegion("amazon-bedrock")
	if err != nil || key != "br-secret-2" || region != "eu-central-1" {
		t.Fatalf("bedrock: %q %q %v", key, region, err)
	}
	var out bytes.Buffer
	if err := executeCommand(t.Context(), NewAuthCommand(), &out, "status"); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "amazon-bedrock\tapi_key\nanthropic\tapi_key\ngroq\tapi_key\n"; got != want {
		t.Fatalf("status: %q, want %q", got, want)
	}
}
