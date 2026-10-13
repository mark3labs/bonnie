package bonnie

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

func TestNoHTTPListenerLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, stopped := false, false
	a := lifecycleAgent(t, WithHTTPListenerDisabled(), WithAddr("invalid address: should never bind"), WithChannel(func(r *runtime.Runner) (Channel, error) {
		return &lifecycleChannel{name: "non-tcp", start: func(context.Context) error { started = true; cancel(); return nil }, shutdown: func(context.Context) error {
			stopped = true
			_, err := r.Journal().Replay(context.Background(), "absent")
			if errors.Is(err, runtime.ErrRunNotFound) {
				return nil
			}
			return err
		}}, nil
	}))
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !started || !stopped {
		t.Fatal("lifecycle skipped")
	}
}
func TestNoHTTPListenerRejectsIncompatibleSurfaces(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	for _, opt := range []Option{WithListener(ln), WithWebUI(true), WithChannel(func(*runtime.Runner) (Channel, error) {
		return &lifecycleChannel{name: "webhook", routes: []channel.Route{{Method: "POST", Path: "/webhook"}}, start: func(context.Context) error { return nil }, shutdown: func(context.Context) error { return nil }}, nil
	})} {
		err := lifecycleAgent(t, WithHTTPListenerDisabled(), opt).Run(t.Context())
		if err == nil || !strings.Contains(err.Error(), "disabled HTTP listener conflicts") {
			t.Fatal(err)
		}
	}
}
