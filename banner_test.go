package bonnie

import (
	"bytes"
	"strings"
	"testing"
)

// The startup message must name the checked provider, not the implicit
// Landlock default when WithSandboxes or --sandbox selects another backend.
func TestBannerSelectedSandbox(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, selected, want string
		opts                 func(*selectionProvider, *selectionProvider) []Option
	}{
		{"list default", "", "first", func(first, second *selectionProvider) []Option { return []Option{WithSandboxes(first, second)} }},
		{"list flag", "microsandbox", "microsandbox", func(first, second *selectionProvider) []Option { return []Option{WithSandboxes(first, second)} }},
		{"single", "", "first", func(first, _ *selectionProvider) []Option { return []Option{WithSandbox(first)} }},
		{"implicit default", "", "landlock (default)", func(_, _ *selectionProvider) []Option { return nil }},
		{"explicit default", "landlock", "landlock", func(_, _ *selectionProvider) []Option { return nil }},
		{"host factory", "", "", func(_, _ *selectionProvider) []Option { return []Option{WithAgentFactory(stubFactory)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := &selectionProvider{t: t, name: "first"}
			second := &selectionProvider{t: t, name: "microsandbox"}
			c := New(tc.opts(first, second)...).cfg
			c.sandboxName = tc.selected
			if c.factory == nil && !c.sandboxSet && !c.sandboxesSet {
				// Test the default label without requiring host Landlock support.
				provider, err := c.selectSandbox()
				if err != nil {
					t.Fatal(err)
				}
				c.selectedSandbox = provider
			} else if _, err := c.agentFactory(t.Context(), "", nil); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			c.banner(&out, ":8081", "", "", false)
			want := "bonnie: sandbox " + tc.want + "\n"
			if tc.want == "" {
				want = "bonnie: agent supplied by the host\n"
				if strings.Contains(out.String(), "bonnie: sandbox") {
					t.Fatal("host factory message claims a sandbox")
				}
			}
			if !strings.Contains(out.String(), want) {
				t.Fatalf("startup message = %q, want %q", out.String(), want)
			}
			if second.availableCalls > 0 && tc.selected != "microsandbox" {
				t.Fatal("unselected provider was checked")
			}
		})
	}
}
