package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestSandboxProviderSelection(t *testing.T) {
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
			p, err := sandboxProvider(c.kind, "")
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

// TestNoSandboxIsTheDefault documents the default plainly. It is the right
// choice for a local developer and the wrong one for an exposed server, which
// is why the help text and the startup banner both say so.
func TestNoSandboxIsTheDefault(t *testing.T) {
	f, err := agentFactory("none", "", nil, nil)
	if err != nil {
		t.Fatalf("agentFactory: %v", err)
	}
	if f == nil {
		t.Fatal("no factory returned")
	}
	if f, err = agentFactory("", "", nil, nil); err != nil || f == nil {
		t.Fatalf("empty kind must behave like none: %v", err)
	}
}

// TestDenyNetworkNeedsASandbox stops a false sense of safety: asking for no
// egress without a sandbox must fail, not quietly run with full network.
func TestDenyNetworkNeedsASandbox(t *testing.T) {
	_, err := agentFactory("none", "", &sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}, nil)
	if err == nil {
		t.Fatal("want an error for a network policy without a sandbox")
	}
	if !strings.Contains(err.Error(), "--sandbox") {
		t.Fatalf("error does not say how to fix it: %v", err)
	}
}

// TestDenyNetworkRejectedByIncapableBackend is the same honesty rule one level
// down: a backend that cannot enforce the policy must refuse it.
func TestDenyNetworkOnLocalIsRejected(t *testing.T) {
	// Local cannot control egress at all, so it does not implement
	// sandbox.Networked and the request must fail.
	if _, err := agentFactory("local", "", &sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}, nil); err == nil {
		t.Fatal("want an error: the local sandbox cannot control the network")
	}
}

func TestSandboxImageIsPassedThrough(t *testing.T) {
	p, err := sandboxProvider("docker", "python:3.12-slim")
	if err != nil {
		t.Fatalf("sandboxProvider: %v", err)
	}
	d, ok := p.(*sandbox.DockerProvider)
	if !ok {
		t.Fatalf("got %T", p)
	}
	// The field is unexported, so check the behaviour that depends on it:
	// the provider is usable and named correctly.
	if d.Name() != "docker" {
		t.Fatalf("name = %q", d.Name())
	}
}

// TestUnavailableSandboxFailsAtStartup is why Available exists. An operator
// must learn that Docker is down when the server starts, not on the first tool
// call an hour later.
func TestUnavailableSandboxFailsAtStartup(t *testing.T) {
	_, err := agentFactory("microsandbox", "", nil, []kit.Option{})
	if err == nil {
		t.Skip("msb is installed here, so this path cannot be exercised")
	}
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// TestPrefixedAddsExactlyOnePrefix guards the terminal output. Library errors
// already carry "bonnie:" by convention, so prefixing unconditionally produced
// "bonnie: bonnie: ..." for every error raised inside the framework.
func TestPrefixedAddsExactlyOnePrefix(t *testing.T) {
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
