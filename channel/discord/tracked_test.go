package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

func TestTrackedReceiptIdempotencyAndDeliveryFailure(t *testing.T) {
	t.Parallel()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/messages") {
			http.Error(w, "rejected", http.StatusBadRequest)
			return
		}
	}))
	t.Cleanup(api.Close)
	h := adapter(t, nil)
	h.ch.api = api.URL
	first, err := h.ch.PrepareDispatch(context.Background(), "d1", "C77")
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.ch.PrepareDispatch(context.Background(), "d1", "C77")
	if err != nil {
		t.Fatal(err)
	}
	if first.Address != "C77" || again.Address != first.Address {
		t.Fatalf("receipts = %+v, %+v", first, again)
	}
	if err := h.ch.DeliverDispatch(context.Background(), channel.DispatchReceipt{DispatchID: "d1", Address: "C77"}, &runtime.Run{Response: "done"}); err == nil {
		t.Fatal("rejected delivery returned nil")
	}
}
