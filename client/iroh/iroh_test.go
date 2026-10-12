package iroh

import (
	"context"
	"testing"
	"time"

	goiroh "github.com/tmc/go-iroh/iroh"
)

func TestValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewHTTPClient(nil, "invalid"); err == nil {
		t.Fatal("nil endpoint accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, err := goiroh.Bind(ctx, goiroh.WithoutRelayTransports(), goiroh.WithoutNetReport())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ep.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	if _, err := NewHTTPClient(ep, "invalid"); err == nil {
		t.Fatal("invalid ticket accepted")
	}
}
