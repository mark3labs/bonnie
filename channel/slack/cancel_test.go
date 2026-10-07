package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// A verified slash command selects an exact thread. A duplicate must not
// affect new work, and a public channel without a thread is refused.
func TestCancelCommand(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	release, active := h.agent.HoldOpen()
	defer release()
	done := make(chan *runtime.Run, 1)
	go func() {
		run, err := h.ch.From("C1/123.456").Send(t.Context(), "work", channel.SendOptions{})
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	<-active
	post := func(body string, verified bool) *httptest.ResponseRecorder {
		ts := fmt.Sprint(time.Now().Unix())
		req := httptest.NewRequest(http.MethodPost, DefaultPath+"/cancel", strings.NewReader(body))
		mac := hmac.New(sha256.New, []byte(h.secret))
		_, _ = fmt.Fprintf(mac, "v0:%s:%s", ts, body)
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		if verified {
			req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		h.ch.handleCancelCommand(w, req, h.ch, nil)
		return w
	}
	body := "channel_id=C1&user_id=U7&trigger_id=one&text=123.456"
	if w := post(body, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", w.Code)
	}
	if w := post("channel_id=C1&user_id=U7&trigger_id=two", true); w.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous: %d", w.Code)
	}
	if w := post(body, true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "requested") {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	select {
	case run := <-done:
		if run == nil || run.State != runtime.RunCancelled {
			t.Fatalf("boundary: %+v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop turn")
	}
	if w := post(body, true); !strings.Contains(w.Body.String(), "already") {
		t.Fatalf("duplicate: %s", w.Body)
	}
}
