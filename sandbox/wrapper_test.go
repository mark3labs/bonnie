package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// imagedStub is a provider that runs a named image, like Docker and
// microsandbox do.
type imagedStub struct{ plainStub }

func (imagedStub) Image() string { return "python:3.12-slim" }

// plainStub is a provider that runs no image, like Local and Landlock.
type plainStub struct{}

func (plainStub) Name() string                    { return "stub" }
func (plainStub) Available(context.Context) error { return nil }
func (plainStub) Open(context.Context, string) (Sandbox, error) {
	return nil, errors.New("stub does not open")
}

// A host asks the configured provider which image is in force. run.go wraps
// the backend in [EnvInjected] and then [Seeded] before the agent ever sees
// it, so a wrapper that drops [Imaged] answers for a provider the caller
// cannot reach. That is the defect [Seeded.WorkingDir] records, one
// interface over.
func TestWrappersForwardImaged(t *testing.T) {
	t.Parallel()
	const want = "python:3.12-slim"

	cases := []struct {
		name string
		wrap func(Provider) Provider
	}{
		{"Seeded", func(p Provider) Provider { return Seeded(p, t.TempDir()) }},
		{"EnvInjected", func(p Provider) Provider { return EnvInjected(p, map[string]string{"K": "v"}) }},
		{"EnvInjected(Seeded)", func(p Provider) Provider {
			return EnvInjected(Seeded(p, t.TempDir()), map[string]string{"K": "v"})
		}},
		// The order run.go actually applies: env first, then the seed.
		{"Seeded(EnvInjected)", func(p Provider) Provider {
			return Seeded(EnvInjected(p, map[string]string{"K": "v"}), t.TempDir())
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wrapped := c.wrap(imagedStub{})
			i, ok := wrapped.(Imaged)
			if !ok {
				t.Fatal("the wrapper dropped Imaged: a host would be told the backend runs no image")
			}
			if got := i.Image(); got != want {
				t.Fatalf("Image() = %q, want %q", got, want)
			}
		})
	}
}

// The mirror image of the test above: a wrapper must not INVENT the
// capability. [Imaged] is a report, so "" and "no such method" mean
// different things — an unnamed image against no image at all — and a
// wrapper that always answers turns Landlock, which runs no image, into one
// that claims a nameless one. This is how [newEnvSandbox] already treats
// [Deleter].
func TestWrappersDoNotInventImaged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		wrap func(Provider) Provider
	}{
		{"Seeded", func(p Provider) Provider { return Seeded(p, t.TempDir()) }},
		{"EnvInjected", func(p Provider) Provider { return EnvInjected(p, map[string]string{"K": "v"}) }},
		{"Seeded(EnvInjected)", func(p Provider) Provider {
			return Seeded(EnvInjected(p, map[string]string{"K": "v"}), t.TempDir())
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := c.wrap(plainStub{}).(Imaged); ok {
				t.Fatal("the wrapper advertised Imaged for a backend that runs no image")
			}
		})
	}
}

// A real backend, wrapped the way run.go wraps it, still reports its image.
func TestDockerImageSurvivesWrapping(t *testing.T) {
	t.Parallel()
	p := Seeded(EnvInjected(Docker(WithDockerImage("alpine:3.20")), map[string]string{"K": "v"}), t.TempDir())
	i, ok := p.(Imaged)
	if !ok {
		t.Fatal("a wrapped Docker provider does not report its image")
	}
	if got := i.Image(); got != "alpine:3.20" {
		t.Fatalf("Image() = %q, want alpine:3.20", got)
	}
}

// cliError keeps the two failure modes of [runCLI] apart. Collapsing them
// lost both halves: a CLI that never ran wrote no stderr to quote, so a
// missing binary was reported as "no output", and the real cause could not
// be reached with errors.Is.
func TestCliErrorSeparatesTheFailureModes(t *testing.T) {
	t.Parallel()

	t.Run("the CLI never ran", func(t *testing.T) {
		t.Parallel()
		// A binary that is not there: exactly what a host without docker
		// installed hands DeleteRun.
		_, stderr, code, err := runCLI(context.Background(), nil,
			"bonnie-no-such-binary-ever", "rm", "--force", "bonnie-x")
		if err == nil {
			t.Fatal("running a missing binary reported no error")
		}
		cerr := cliError("rm bonnie-x", firstLine(stderr), code, err)
		if cerr == nil {
			t.Fatal("cliError swallowed a CLI that never ran")
		}
		if !errors.Is(cerr, exec.ErrNotFound) {
			t.Errorf("the cause is unwrappable: %v", cerr)
		}
		if got := cerr.Error(); !strings.Contains(got, "bonnie-no-such-binary-ever") {
			t.Errorf("the message does not name the binary that is missing: %q", got)
		}
		if strings.Contains(cerr.Error(), "no output") {
			t.Errorf("a CLI that never ran was reported through its empty stderr: %q", cerr)
		}
	})

	t.Run("the CLI ran and refused", func(t *testing.T) {
		t.Parallel()
		cerr := cliError("rm bonnie-x", "No such container: bonnie-x", 1, nil)
		if cerr == nil {
			t.Fatal("cliError swallowed a non-zero exit")
		}
		if !strings.Contains(cerr.Error(), "No such container") {
			t.Errorf("stderr was dropped from the message: %q", cerr)
		}
	})

	t.Run("success is nil", func(t *testing.T) {
		t.Parallel()
		if cerr := cliError("rm bonnie-x", "", 0, nil); cerr != nil {
			t.Fatalf("a clean run reported %v", cerr)
		}
	})
}
