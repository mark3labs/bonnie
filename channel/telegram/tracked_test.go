package telegram

import (
	"context"
	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestTrackedReceiptIdempotencyAndDeliveryFailure(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"ok":false}`)) }))
	t.Cleanup(api.Close)
	h := adapter(t, nil, "secret", "mybot")
	h.ch.api = api.URL
	a, err := h.ch.PrepareDispatch(context.Background(), "d1", "-100123/7")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.ch.PrepareDispatch(context.Background(), "d1", "-100123/7")
	if err != nil {
		t.Fatal(err)
	}
	if a.Address != b.Address || calls.Load() != 0 {
		t.Fatalf("receipts=%+v %+v calls=%d", a, b, calls.Load())
	}
	err = h.ch.DeliverDispatch(context.Background(), channel.DispatchReceipt{DispatchID: "d1", Address: a.Address}, &runtime.Run{Response: "done"})
	if err == nil {
		t.Fatal("rejected delivery returned nil")
	}
}
