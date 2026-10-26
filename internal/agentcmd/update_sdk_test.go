package agentcmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

const modelCatalog = `{"bonnie-update-test":{"models":{"unique-model":{"id":"unique-model","name":"Fixture"}}}}`

// A fresh process must see the shared catalog; an initialized registry does not reload.
func TestUpdateLookupProcess(t *testing.T) {
	if os.Getenv("BONNIE_UPDATE_LOOKUP") != "1" {
		return
	}
	found := kit.LookupModel("bonnie-update-test", "unique-model") != nil
	if fmt.Sprint(found) != os.Getenv("BONNIE_UPDATE_FOUND") {
		t.Fatalf("found = %v", found)
	}
}

func TestUpdatePublicSDK(t *testing.T) {
	// Environment changes and SDK cache operations must remain serial.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	lookup := func(want bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUpdateLookupProcess$")
		cmd.Env = append(os.Environ(), "BONNIE_UPDATE_LOOKUP=1", fmt.Sprintf("BONNIE_UPDATE_FOUND=%v", want))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lookup: %v: %s", err, out)
		}
	}
	run := func(ctx context.Context, wantError bool, args ...string) string {
		t.Helper()
		cmd := NewUpdateCommand()
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"models"}, args...))
		err := cmd.ExecuteContext(ctx)
		if (err != nil) != wantError {
			t.Fatalf("%v: %v", args, err)
		}
		if wantError && out.Len() != 0 {
			t.Fatalf("success after error: %s", &out)
		}
		return out.String()
	}
	var requests atomic.Int32
	var validated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("If-None-Match") == `"fixture"` {
			validated.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"fixture"`)
		_, _ = fmt.Fprint(w, modelCatalog)
	}))
	defer server.Close()
	t.Setenv("KIT_MODELS_URL", server.URL)
	if out := run(t.Context(), false); !strings.Contains(out, "1 providers, 1 models") {
		t.Fatal(out)
	}
	lookup(true)
	if out := run(t.Context(), false); !strings.Contains(out, "already up to date") {
		t.Fatal(out)
	}
	if requests.Load() != 2 || !validated.Load() {
		t.Fatalf("requests=%d validated=%v", requests.Load(), validated.Load())
	}
	lookup(true)
	run(t.Context(), false, server.URL)
	file := filepath.Join(t.TempDir(), "models with spaces.json")
	if err := os.WriteFile(file, []byte(modelCatalog), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t.Context(), false, file)
	lookup(true)
	if err := os.WriteFile(file, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t.Context(), true, file)
	lookup(true)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer bad.Close()
	run(t.Context(), true, bad.URL)
	lookup(true)
	started := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	run(ctx, true, slow.URL)
	lookup(true)
	for range 2 {
		if out := run(t.Context(), false, "embedded"); !strings.Contains(out, "reset to embedded") {
			t.Fatal(out)
		}
		lookup(false)
	}
}
