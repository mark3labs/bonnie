package bonnie

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/mark3labs/bonnie/sandbox"
)

// selectSandbox resolves the permitted list before any backend is configured
// or checked. Selection never substitutes a new provider or tries a fallback.
func (c *config) selectSandbox() (sandbox.Provider, error) {
	if c.sandboxesSet && c.sandboxSet {
		return nil, fmt.Errorf("bonnie: WithSandbox cannot be combined with WithSandboxes")
	}
	if c.sandboxesDuplicate {
		return nil, fmt.Errorf("bonnie: WithSandboxes may only be set once")
	}
	providers := c.sandboxes
	if !c.sandboxesSet {
		if c.sandboxSet {
			providers = []sandbox.Provider{c.sandbox}
		} else {
			providers = []sandbox.Provider{c.defaultSandbox()}
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("bonnie: WithSandboxes requires at least one provider")
	}

	names := make([]string, 0, len(providers))
	seen := make(map[string]bool, len(providers))
	selected := providers[0]
	found := c.sandboxName == ""
	for _, provider := range providers {
		if nilProvider(provider) {
			return nil, fmt.Errorf("bonnie: sandbox provider must not be nil")
		}
		name := provider.Name()
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("bonnie: sandbox provider name must not be empty")
		}
		if seen[name] {
			return nil, fmt.Errorf("bonnie: duplicate sandbox provider name %q", name)
		}
		seen[name] = true
		names = append(names, name)
		if name == c.sandboxName {
			selected = provider
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("bonnie: unknown sandbox %q; permitted backends: %s", c.sandboxName, strings.Join(names, ", "))
	}
	return selected, nil
}

// A Provider interface can hold a typed nil; do not call Name on one.
func nilProvider(provider sandbox.Provider) bool {
	if provider == nil {
		return true
	}
	v := reflect.ValueOf(provider)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
