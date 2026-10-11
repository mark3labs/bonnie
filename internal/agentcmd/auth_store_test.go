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
