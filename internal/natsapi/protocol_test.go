package natsapi

import (
	"net/http"
	"testing"
)

func TestValidation(t *testing.T) {
	for _, path := range []string{"http://evil/bonnie/v1/info", "//evil/bonnie/v1/info", "/bonnie/v1/../info", "/bonnie/v1/%2e%2e/info", "/bonnie/v1/info#x", "/bonnie/v1/%00", "/elsewhere", "/bonnie/v1/x\\y"} {
		if _, err := Path(path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	for _, path := range []string{"/bonnie/v1/info", "/bonnie/v1/addresses/a%2Fb", "/bonnie/v1/runs/id/stream?cursor=42"} {
		if _, err := Path(path); err != nil {
			t.Errorf("rejected %q", path)
		}
	}
	for _, name := range []string{"Host", "Connection", "X-Forwarded-For", "X-Identity", "Nats-Test", "Proxy-Authorization", "X-User"} {
		if Headers(http.Header{name: []string{"x"}}, []string{name}) == nil {
			t.Errorf("accepted %s", name)
		}
	}
	if Headers(http.Header{"Authorization": []string{"Bearer x\r\nx-user: y"}}, nil) == nil {
		t.Fatal("accepted newline")
	}
	if Headers(http.Header{"X-Operator-Token": []string{"x"}}, []string{"X-Operator-Token"}) != nil {
		t.Fatal("additional credential header rejected")
	}
	// W3C trace context crosses the transport, so a turn can join the
	// caller's trace.
	trace := http.Header{
		"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		"Tracestate":  {"vendor=x"},
	}
	if err := Headers(trace, nil); err != nil {
		t.Fatalf("trace context rejected: %v", err)
	}
	prefix := DefaultInboxPrefix
	for _, inbox := range []string{"tasks", "", prefix + ".*", prefix + ".short", prefix + "." + Token(16) + ".extra"} {
		if Inbox(inbox, prefix) {
			t.Errorf("accepted inbox %s", inbox)
		}
	}
	if !Inbox(prefix+"."+Token(16), prefix) {
		t.Fatal("valid inbox rejected")
	}
	if Decode([]byte(`{"version":1,"unknown":true}`), new(Request)) == nil || Decode([]byte(`{} {}`), new(Request)) == nil {
		t.Fatal("non-strict decoding")
	}
}
func TestLimitsAndStream(t *testing.T) {
	limits, err := (Limits{}).Defaults()
	if err != nil || limits.Window != 8 {
		t.Fatal(limits, err)
	}
	if _, err = (Limits{MaxRequests: -1}).Defaults(); err == nil {
		t.Fatal("negative limit")
	}
	u, _ := Path("/bonnie/v1/runs/r/stream?cursor=4")
	if !Stream(u, "GET") || Stream(u, "POST") {
		t.Fatal("stream classification")
	}
}
