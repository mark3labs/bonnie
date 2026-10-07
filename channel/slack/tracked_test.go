package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

func TestTrackedReceiptIdempotencyAndDeliveryFailure(t *testing.T) {
	t.Parallel()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Text == "Scheduled task" {
			_, _ = w.Write([]byte(`{"ok":true,"ts":"root-ts"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"rejected"}`))
	}))
	t.Cleanup(api.Close)
	r := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())
	ch := mustNew(t, r, Config{BotToken: "xoxb-test", SigningSecret: "secret", APIURL: api.URL})
	first, err := ch.PrepareDispatch(context.Background(), "d1", "C1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := ch.PrepareDispatch(context.Background(), "d1", "C1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Address != "C1/root-ts" || again.Address != first.Address {
		t.Fatalf("receipts = %+v, %+v", first, again)
	}
	if err := ch.DeliverDispatch(context.Background(), channel.DispatchReceipt{DispatchID: "d1", Address: "C1/root-ts"}, &runtime.Run{Response: "done"}); err == nil {
		t.Fatal("rejected delivery returned nil")
	}
}
