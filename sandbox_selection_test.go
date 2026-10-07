package bonnie

import (
	"context"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/sandbox"
)

// Selection must keep the configured provider, not create one by name.
func TestSandboxSelectionIdentity(t *testing.T) {
	t.Parallel()
	first := &selectionProvider{name: "first"}
	second := &selectionProvider{name: "second"}
	for _, tc := range []struct {
		name string
		want sandbox.Provider
	}{
		{"", first},
		{"first", first},
		{"second", second},
	} {
		t.Run("selected="+tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(WithSandboxes(first, second)).cfg
			c.sandboxName = tc.name
			got, err := c.selectSandbox()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("selected provider = %p, want %p", got, tc.want)
			}
		})
	}
}

// Invalid lists and conflicting options must fail before checking a backend.
func TestSandboxSelectionInvalid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts func(*selectionProvider) []Option
		want string
	}{
		{"empty", func(p *selectionProvider) []Option { return []Option{WithSandboxes()} }, "requires at least one provider"},
		{"nil", func(p *selectionProvider) []Option { return []Option{WithSandboxes(p, nil)} }, "must not be nil"},
		{"typed nil", func(p *selectionProvider) []Option {
			var absent *selectionProvider
			return []Option{WithSandboxes(p, absent)}
		}, "must not be nil"},
		{"duplicate", func(p *selectionProvider) []Option {
			return []Option{WithSandboxes(p, &selectionProvider{name: p.name})}
		}, "duplicate sandbox provider name"},
		{"empty name", func(p *selectionProvider) []Option {
			return []Option{WithSandboxes(p, &selectionProvider{})}
		}, "name must not be empty"},
		{"blank name", func(p *selectionProvider) []Option {
			return []Option{WithSandboxes(p, &selectionProvider{name: " \t\n"})}
		}, "name must not be empty"},
		{"single then list", func(p *selectionProvider) []Option {
			return []Option{WithSandbox(p), WithSandboxes(p)}
		}, "WithSandbox cannot be combined with WithSandboxes"},
		{"list then single", func(p *selectionProvider) []Option {
			return []Option{WithSandboxes(p), WithSandbox(p)}
		}, "WithSandbox cannot be combined with WithSandboxes"},
		{"repeated", func(p *selectionProvider) []Option {
			return []Option{WithSandboxes(p), WithSandboxes(p)}
		}, "WithSandboxes may only be set once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &selectionProvider{t: t, name: "valid"}
			c := New(tc.opts(p)...).cfg
			factory, err := c.agentFactory(t.Context(), "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("agentFactory error = %v, want %q", err, tc.want)
			}
			if factory != nil || p.availableCalls != 0 {
				t.Fatal("invalid configuration created a factory or checked availability")
			}
		})
	}
}

// Without a list, the operator can select only the default or explicit backend.
func TestSandboxSelectionPermittedOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "landlock", "local", "docker"} {
		t.Run("default/"+name, func(t *testing.T) {
			t.Parallel()
			c := New().cfg
			c.sandboxName = name
			p, err := c.selectSandbox()
			if name == "" || name == "landlock" {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := p.(*sandbox.LandlockProvider); !ok {
					t.Fatalf("default provider = %T, want Landlock", p)
				}
			} else if err == nil || !strings.Contains(err.Error(), "permitted backends: landlock") || p != nil {
				t.Fatalf("selection = %v, %v; want only landlock permitted", p, err)
			}
		})
	}
	for _, name := range []string{"", "custom", "landlock", "local"} {
		t.Run("single/"+name, func(t *testing.T) {
			t.Parallel()
			want := &selectionProvider{name: "custom"}
			c := New(WithSandbox(want)).cfg
			c.sandboxName = name
			p, err := c.selectSandbox()
			if name == "" || name == "custom" {
				if err != nil || p != want {
					t.Fatalf("selection = %v, %v; want configured provider", p, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "permitted backends: custom") || p != nil {
				t.Fatalf("selection = %v, %v; want only custom permitted", p, err)
			}
		})
	}
}

// Changes to the caller's slice must not change the permitted list.
func TestWithSandboxesCopiesSlice(t *testing.T) {
	t.Parallel()
	first := &selectionProvider{name: "first"}
	second := &selectionProvider{name: "second"}
	providers := []sandbox.Provider{first, second}
	option := WithSandboxes(providers...)
	providers[0] = nil // The copy must happen when the option is created.
	c := New(option).cfg
	providers[1] = first // Later changes must not replace a named provider either.
	for _, tc := range []struct {
		name string
		want sandbox.Provider
	}{{"", first}, {"second", second}} {
		c.sandboxName = tc.name
		got, err := c.selectSandbox()
		if err != nil || got != tc.want {
			t.Fatalf("selection %q = %v, %v; want %p", tc.name, got, err, tc.want)
		}
	}
}

// Only the selected provider is checked. Failure must not try another provider.
func TestSandboxSelectionAvailability(t *testing.T) {
	t.Parallel()
	for _, selected := range []string{"", "second", "unknown"} {
		for _, unavailable := range []bool{false, true} {
			name := selected + "/available"
			if unavailable {
				name = selected + "/unavailable"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				first := &selectionProvider{t: t, name: "first"}
				second := &selectionProvider{t: t, name: "second"}
				chosen, other := first, second
				if selected == "second" {
					chosen, other = second, first
				}
				// An unavailable unselected backend must not stop startup.
				other.availableErr = sandbox.ErrUnavailable
				if unavailable {
					chosen.availableErr = sandbox.ErrUnavailable
				}
				c := New(WithSandboxes(first, second)).cfg
				c.sandboxName = selected
				factory, err := c.agentFactory(t.Context(), "", nil)
				switch {
				case selected == "unknown":
					if err == nil || !strings.Contains(err.Error(), "permitted backends: first, second") || factory != nil {
						t.Fatalf("unknown selection = %v; want permitted list", err)
					}
					if chosen.availableCalls != 0 {
						t.Fatal("unknown selection checked availability")
					}
				case unavailable:
					if !errors.Is(err, sandbox.ErrUnavailable) || factory != nil {
						t.Fatalf("unavailable selection = %v; want ErrUnavailable and no factory", err)
					}
				case err != nil || factory == nil:
					t.Fatalf("available selection failed: %v", err)
				}
				if selected != "unknown" && chosen.availableCalls != 1 {
					t.Fatalf("selected Available calls = %d, want 1", chosen.availableCalls)
				}
				if other.availableCalls != 0 {
					t.Fatal("unselected provider checked or used as a fallback")
				}
			})
		}
	}
}

// A custom agent factory cannot silently ignore the permitted sandbox list.
func TestWithSandboxesAgentFactoryConflict(t *testing.T) {
	t.Parallel()
	for _, factoryFirst := range []bool{false, true} {
		name := "list first"
		if factoryFirst {
			name = "factory first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &selectionProvider{t: t, name: "custom"}
			opts := []Option{WithSandboxes(p), WithAgentFactory(stubFactory)}
			if factoryFirst {
				opts[0], opts[1] = opts[1], opts[0]
			}
			factory, err := New(opts...).cfg.agentFactory(t.Context(), "", nil)
			if err == nil || !strings.Contains(err.Error(), "WithAgentFactory") || !strings.Contains(err.Error(), "WithSandboxes") {
				t.Fatalf("conflict error = %v, want both option names", err)
			}
			if factory != nil || p.availableCalls != 0 {
				t.Fatal("conflicting options created a factory or checked availability")
			}
		})
	}
}

// Local flag sets allow parallel tests without changing process flags.
func TestSandboxServeFlags(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("sandbox", flag.ContinueOnError)
			defaults := map[string]string{"sandbox": "", "addr": "", "model": ""}
			original := make(map[string]*flag.Flag)
			if existing {
				for name := range defaults {
					defaults[name] = "host-" + name
					fs.String(name, defaults[name], "host flag")
					original[name] = fs.Lookup(name)
				}
			}
			registerServeFlags(fs)
			registerServeFlags(fs)
			for name, want := range defaults {
				f := fs.Lookup(name)
				if f == nil || f.Value.String() != want || f.DefValue != want {
					t.Fatalf("flag %q defaults changed: %v", name, f)
				}
				if existing && (f != original[name] || f.Usage != "host flag") {
					t.Fatalf("host flag %q was replaced", name)
				}
			}
			if err := fs.Parse([]string{"--sandbox=second", "--addr=127.0.0.1:4321", "--model=provider/model"}); err != nil {
				t.Fatal(err)
			}
			registerServeFlags(fs)
			for name, want := range map[string]string{"sandbox": "second", "addr": "127.0.0.1:4321", "model": "provider/model"} {
				if got := fs.Lookup(name).Value.String(); got != want {
					t.Fatalf("parsed flag %q = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// Open is not needed to select a backend or build the agent factory.
type selectionProvider struct {
	t              *testing.T
	name           string
	availableErr   error
	availableCalls int
}

func (p *selectionProvider) Name() string { return p.name }

func (p *selectionProvider) Available(context.Context) error {
	p.availableCalls++
	return p.availableErr
}

func (p *selectionProvider) Open(context.Context, string) (sandbox.Sandbox, error) {
	p.t.Fatal("Open must not be called during sandbox selection")
	return nil, sandbox.ErrUnavailable
}
