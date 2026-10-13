package bonnie

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

func TestHTTPDisabledNoBindAndLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := false
	a := lifecycleAgent(t, WithHTTPListenerDisabled(), WithAddr("invalid address"), WithChannel(func(r *runtime.Runner) (Channel, error) {
		return &lifecycleChannel{name: "transport", start: func(context.Context) error { cancel(); return nil }, shutdown: func(ctx context.Context) error { stopped = true; _, err := r.Journal().Runs(ctx, ""); return err }}, nil
	}))
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("not stopped before journal close")
	}
}
func TestHTTPDisabledRejectsSurfaces(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	opts := []Option{WithListener(ln), WithWebUI(true), WithChannel(func(*runtime.Runner) (Channel, error) {
		return &lifecycleChannel{name: "webhook", routes: []channel.Route{{Method: http.MethodPost, Path: "/hook"}}, shutdown: func(context.Context) error { return nil }}, nil
	})}
	for i, opt := range opts {
		t.Run([]string{"listener", "web", "route"}[i], func(t *testing.T) {
			if err := lifecycleAgent(t, WithHTTPListenerDisabled(), opt).Run(t.Context()); err == nil || !strings.Contains(err.Error(), "conflicts") {
				t.Fatalf("got %v", err)
			}
		})
	}
}
