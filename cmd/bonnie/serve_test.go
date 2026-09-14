package main

import (
	"context"
	"errors"
	"testing"

	"github.com/mark3labs/bonnie/sandbox"
)

func TestSandboxProviderSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind    string
		want    string
		wantErr bool
	}{
		{kind: "docker", want: "docker"},
		{kind: "microsandbox", want: "microsandbox"},
		{kind: "msb", want: "microsandbox"},
		{kind: "local", want: "local"},
		{kind: "frobnicate", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			t.Parallel()
			p, err := sandboxProvider(context.Background(), c.kind, "")
			if c.wantErr {
				if err == nil {
					t.Fatal("want an error for an unknown sandbox")
				}
				return
			}
			if err != nil {
				t.Fatalf("sandboxProvider: %v", err)
			}
			if p.Name() != c.want {
				t.Fatalf("got %q, want %q", p.Name(), c.want)
			}
		})
	}
}

// TestSandboxImageReachesEveryBackendThatRunsOne is invariant 13 at the
// sandbox flag. The image used to reach docker and microsandbox and stop
// there: `--sandbox auto --sandbox-image python:3.12-slim` built its
// candidates with no image at all, so an operator who named an image got
// alpine and no warning. The old test asserted only the provider's name, so
// it passed either way.
func TestSandboxImageReachesEveryBackendThatRunsOne(t *testing.T) {
	t.Parallel()
	const image = "python:3.12-slim"

	for _, kind := range []string{"docker", "microsandbox", "msb"} {
		p, err := sandboxProvider(context.Background(), kind, image)
		if err != nil {
			t.Fatalf("sandboxProvider(%s): %v", kind, err)
		}
		imaged, ok := p.(sandbox.Imaged)
		if !ok {
			t.Fatalf("%s provider (%T) reports no image", kind, p)
		}
		if got := imaged.Image(); got != image {
			t.Fatalf("%s runs %q, want the image that was asked for, %q", kind, got, image)
		}
	}

	// auto picks a backend at run time, and whichever it picks must carry
	// the image. The pick needs a live backend, so a host with neither is a
	// skip, not a failure.
	p, err := sandboxProvider(context.Background(), "auto", image)
	if errors.Is(err, sandbox.ErrUnavailable) {
		t.Skip("no sandbox backend is available here")
	}
	if err != nil {
		t.Fatalf("sandboxProvider(auto): %v", err)
	}
	imaged, ok := p.(sandbox.Imaged)
	if !ok {
		t.Fatalf("auto picked %T, which reports no image", p)
	}
	if got := imaged.Image(); got != image {
		t.Fatalf("auto picked %s running %q, want %q", p.Name(), got, image)
	}
}

// TestLocalSandboxRefusesAnImage is the other half of invariant 13: a
// backend that cannot honor a setting says so. The local backend runs no
// image, so accepting one would tell an operator their tools run in
// python:3.12-slim while they run on the host.
func TestLocalSandboxRefusesAnImage(t *testing.T) {
	t.Parallel()
	if _, err := sandboxProvider(context.Background(), "local", "python:3.12-slim"); err == nil {
		t.Fatal("the local backend accepted an image it cannot run")
	}
	if _, err := sandboxProvider(context.Background(), "local", ""); err != nil {
		t.Fatalf("the local backend refused an empty image: %v", err)
	}
}

// TestServeHasNoTree pins what `bonnie serve` is now: the generic host. A tree
// is served by running it, so serve takes neither an instructions file nor a
// workspace, and the process's own directory stays the agent's root — the
// historical behaviour of serve with no --agent.
func TestServeHasNoTree(t *testing.T) {
	t.Parallel()
	opts, err := serveOptions(context.Background(), serveOpts{addr: ":0", journal: t.TempDir()})
	if err != nil {
		t.Fatalf("serveOptions: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("serveOptions returned nothing")
	}

	// The flags that read a tree are gone. A user who passes them should get
	// cobra's unknown-flag error, not a silently ignored value.
	cmd := newServeCmd()
	for _, gone := range []string{"agent", "config"} {
		if f := cmd.Flags().Lookup(gone); f != nil {
			t.Fatalf("serve still carries --%s; a tree is served by running it", gone)
		}
	}
}

// TestPrefixedAddsExactlyOnePrefix guards the terminal output. Library errors
// already carry "bonnie:" by convention, so prefixing unconditionally produced
// "bonnie: bonnie: ..." for every error raised inside the framework.
func TestPrefixedAddsExactlyOnePrefix(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"bonnie: sandbox: boom", "bonnie: sandbox: boom"},
		{"unknown sandbox \"frob\"", "bonnie: unknown sandbox \"frob\""},
	}
	for _, c := range cases {
		if got := prefixed(errors.New(c.in)); got != c.want {
			t.Errorf("prefixed(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
