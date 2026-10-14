package sandbox

import (
	"context"
	"errors"
	"testing"
)

// Missing backend capabilities must remain errors, not false reports of loss
// or successful deletion, through either wrapper and both nesting orders.
func TestWrappersForwardUnsupportedCapabilities(t *testing.T) {
	t.Parallel()
	wraps := map[string]func(Provider) Provider{
		"env":      func(p Provider) Provider { return EnvInjected(p, map[string]string{"K": "v"}) },
		"seed":     func(p Provider) Provider { return Seeded(p, "unused") },
		"env-seed": func(p Provider) Provider { return EnvInjected(Seeded(p, "unused"), map[string]string{"K": "v"}) },
		"seed-env": func(p Provider) Provider { return Seeded(EnvInjected(p, map[string]string{"K": "v"}), "unused") },
	}
	for name, wrap := range wraps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := wrap(plainStub{}).(providerWrapper)
			if p.Name() != "stub" || p.Available(context.Background()) != nil {
				t.Fatal("provider identity or availability changed")
			}
			if err := p.SetNetworkPolicy(NetworkPolicy{}); !errors.Is(err, ErrPolicyUnsupported) {
				t.Fatalf("network policy error = %v", err)
			}
			if exists, err := p.SandboxExists(context.Background(), "run"); exists || err == nil || err.Error() != "bonnie: sandbox: the stub backend cannot report sandbox existence" {
				t.Fatalf("existence = %v, %v", exists, err)
			}
			if deleted, err := p.DeleteRun(context.Background(), "run"); deleted || err == nil || err.Error() != "bonnie: sandbox: the stub backend cannot delete sandboxes" {
				t.Fatalf("deletion = %v, %v", deleted, err)
			}
			if got := p.WorkingDir("run"); got != "" {
				t.Fatalf("working directory = %q", got)
			}
		})
	}
}
