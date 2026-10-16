package bonnie

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tree activation must retain its file-backed freshness behavior when
// workspace discovery is not enabled.
func TestWorkspaceSkillsKitTreeCompatibility(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "tree.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(source, []byte("---\nname: tree\ndescription: Tree metadata.\n---\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ORIGINAL_TREE_BODY")
	model := fakemodel.New(fakemodel.Call("activate_skill", `{"name":"tree"}`), fakemodel.Say("done"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	k, err := kit.NewAgent(ctx, model.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
		o.Skills = []string{source}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	write("UPDATED_TREE_BODY")
	if _, err := k.Prompt(ctx, "Activate the authored tree skill."); err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	raw, err := json.Marshal(reqs[1].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "UPDATED_TREE_BODY") || strings.Contains(string(raw), "ORIGINAL_TREE_BODY") {
		t.Fatal("ordinary file-backed tree activation returns stale registered content instead of the updated file body")
	}
}
