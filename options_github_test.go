package bonnie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/runtime"
)

// fakeGitHubAPI records the installation a token was minted for: the
// number a hand-off posts with, and the one GITHUB_INSTALLATION_ID has to
// reach.
type fakeGitHubAPI struct {
	mu            sync.Mutex
	installations []string
	server        *httptest.Server
}

func newFakeGitHubAPI(t *testing.T) *fakeGitHubAPI {
	t.Helper()
	f := &fakeGitHubAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.installations = append(f.installations, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_test-token"})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHubAPI) minted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.installations...)
}

// GITHUB_INSTALLATION_ID is the installation a hand-off posts with, and
// the error a hand-off without one returns names that variable. The
// variable therefore has to be read: an operator who sets what the error
// asks for must get a working hand-off, not the same error again.
func TestGitHubInstallationIDComesFromTheEnvironment(t *testing.T) {
	fake := newFakeGitHubAPI(t)
	t.Setenv("GITHUB_APP_ID", "1234")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testRSAPrivateKey(t))
	t.Setenv("GITHUB_WEBHOOK_SECRET", "s3cret")
	t.Setenv("GITHUB_API_URL", fake.server.URL)
	t.Setenv("GITHUB_INSTALLATION_ID", "424242")

	runner := runtime.NewRunner(runtime.NewMemoryJournal(), digestFactory)
	build := resolve(WithGitHub(github.Config{BotName: "my-agent"})).channels
	ch, err := build[0](runner)
	if err != nil {
		t.Fatalf("the channel did not mount: %v", err)
	}
	to, ok := ch.(channel.Receiver)
	if !ok {
		t.Fatal("the GitHub channel is not a Receiver")
	}

	err = to.Receive(context.Background(),
		github.Target{Owner: "octo", Repo: "repo", Number: 42}, "summarise this", channel.SendOptions{})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(fake.minted()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := fake.minted()
	if len(got) == 0 {
		t.Fatal("the hand-off minted no installation token")
	}
	if got[0] != "424242" {
		t.Fatalf("the token was minted for installation %s, want the one in the environment", got[0])
	}
}

// A GITHUB_INSTALLATION_ID that is not a number is a typo, and a typo that
// silently becomes zero would surface much later as a hand-off asking for
// a variable the operator can see is set.
func TestGitHubInstallationIDMustBeANumber(t *testing.T) {
	t.Setenv("GITHUB_APP_ID", "1234")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testRSAPrivateKey(t))
	t.Setenv("GITHUB_WEBHOOK_SECRET", "s3cret")
	t.Setenv("GITHUB_INSTALLATION_ID", "not-a-number")

	runner := runtime.NewRunner(runtime.NewMemoryJournal(), stubFactory)
	build := resolve(WithGitHub(github.Config{BotName: "my-agent"})).channels
	_, err := build[0](runner)
	if err == nil {
		t.Fatal("the channel mounted with an unparseable installation ID")
	}
	if !strings.Contains(err.Error(), "GITHUB_INSTALLATION_ID") {
		t.Fatalf("error = %v, want the variable named", err)
	}
}

// An authored value outranks the environment, as it does for every other
// channel setting.
func TestGitHubInstallationIDPrefersTheAuthoredValue(t *testing.T) {
	fake := newFakeGitHubAPI(t)
	t.Setenv("GITHUB_APP_ID", "1234")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testRSAPrivateKey(t))
	t.Setenv("GITHUB_WEBHOOK_SECRET", "s3cret")
	t.Setenv("GITHUB_API_URL", fake.server.URL)
	t.Setenv("GITHUB_INSTALLATION_ID", "424242")

	runner := runtime.NewRunner(runtime.NewMemoryJournal(), digestFactory)
	build := resolve(WithGitHub(github.Config{BotName: "my-agent", InstallationID: 99})).channels
	ch, err := build[0](runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.(channel.Receiver).Receive(context.Background(),
		github.Target{Owner: "octo", Repo: "repo", Number: 42}, "hi", channel.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(fake.minted()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fake.minted(); len(got) == 0 || got[0] != "99" {
		t.Fatalf("minted for %v, want the authored 99", got)
	}
}
