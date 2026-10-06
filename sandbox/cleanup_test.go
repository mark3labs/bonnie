package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Cleanup must reject shared mode before a pass, even with no open runs.
func TestRunCleanupValidation(t *testing.T) {
	t.Parallel()
	local := Local()
	landlock := Landlock()
	for _, p := range []Provider{local, landlock, Docker(), Microsandbox()} {
		if err := p.(RunCleanupValidator).ValidateRunCleanup(); err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
	}
	for _, p := range []interface {
		RunCleanupValidator
		UseSharedWorkspace(string) error
	}{local, landlock} {
		if err := p.UseSharedWorkspace(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if err := p.ValidateRunCleanup(); err == nil || !strings.Contains(err.Error(), "shared workspace") {
			t.Fatalf("shared cleanup validation = %v", err)
		}
	}
	if err := Local(WithLocalSharedWorkspace()).ValidateRunCleanup(); err == nil {
		t.Fatal("shared option accepted cleanup")
	}
}

type cleanupDeleterStub struct{ plainStub }

func (cleanupDeleterStub) DeleteRun(context.Context, string) (bool, error) { return false, nil }

type cleanupValidatorStub struct {
	cleanupDeleterStub
	err error
}

func (p cleanupValidatorStub) ValidateRunCleanup() error { return p.err }

// Wrappers must not hide an unsupported backend or a shared workspace mode.
// A third-party deleter without a validator remains supported.
func TestWrappersValidateRunCleanup(t *testing.T) {
	t.Parallel()
	wraps := map[string]func(Provider) Provider{
		"seed": func(p Provider) Provider { return Seeded(p, "unused") },
		"env":  func(p Provider) Provider { return EnvInjected(p, map[string]string{"K": "v"}) },
		"seed-env": func(p Provider) Provider {
			return Seeded(EnvInjected(p, map[string]string{"K": "v"}), "unused")
		},
		"env-seed": func(p Provider) Provider {
			return EnvInjected(Seeded(p, "unused"), map[string]string{"K": "v"})
		},
	}
	for name, wrap := range wraps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			failure := errors.New("cleanup mode rejected")
			landlock := Landlock()
			if err := landlock.UseSharedWorkspace(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			for _, p := range []Provider{plainStub{}, Local(WithLocalSharedWorkspace()), landlock} {
				if err := wrap(p).(RunCleanupValidator).ValidateRunCleanup(); err == nil {
					t.Fatalf("wrapped %s accepted cleanup", p.Name())
				}
			}
			if err := wrap(cleanupValidatorStub{err: failure}).(RunCleanupValidator).ValidateRunCleanup(); !errors.Is(err, failure) {
				t.Fatalf("validator error = %v, want %v", err, failure)
			}
			for _, p := range []Provider{cleanupDeleterStub{}, Local(), Landlock(), Docker(), Microsandbox()} {
				if err := wrap(p).(RunCleanupValidator).ValidateRunCleanup(); err != nil {
					t.Fatalf("wrapped %s: %v", p.Name(), err)
				}
			}
		})
	}
}
