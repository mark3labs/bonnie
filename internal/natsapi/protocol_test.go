package natsapi

import (
	"net/http"
	"testing"
)

func TestPathAndHeaders(t *testing.T) {
	for _, p := range []string{"https://elsewhere/bonnie/v1/info", "//host/bonnie/v1/info", "/bonnie/v1/../info", "/bonnie/v1/%2e%2e/info", "/bonnie/v1/info#secret", "/bonnie/v1/%00", "/bonnie/v1/%zz"} {
		if _, err := Path(p, 8192); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	for _, p := range []string{"/bonnie/v1/info", "/bonnie/v1/addresses/a%2Fb", "/bonnie/v1/runs/a/stream?cursor=3"} {
		if _, err := Path(p, 8192); err != nil {
			t.Errorf("rejected %q", p)
		}
	}
	l, _ := (Limits{}).Defaults()
	for _, k := range []string{"Host", "Connection", "X-Forwarded-For", "X-Identity", "Nats-Reply", "Proxy-Authorization", "X-Auth-User", "Bad Header"} {
		if err := Headers(http.Header{k: {"value"}}, []string{k}, false, l); err == nil {
			t.Errorf("accepted header %q", k)
		}
	}
	if err := Headers(http.Header{"Authorization": {"Bearer a\r\nb"}}, nil, false, l); err == nil {
		t.Fatal("accepted CRLF")
	}
	if err := Headers(http.Header{"X-Api-Key": {"credential"}}, []string{"X-Api-Key"}, false, l); err != nil {
		t.Fatal(err)
	}
}
func TestInboxAndStrictDecode(t *testing.T) {
	for _, s := range []string{"_INBOX.bonnieapi.*", "agent.api", "_INBOX.bonnieapi.", "_INBOX.bonnieapi.aaa", "_INBOX.bonnieapi.0123456789abcdef0123456789abcdef.more"} {
		if Inbox(s, DefaultInbox) {
			t.Errorf("accepted inbox %q", s)
		}
	}
	if !Inbox("_INBOX.bonnieapi.0123456789abcdef0123456789abcdef", DefaultInbox) {
		t.Fatal("valid inbox rejected")
	}
	for _, data := range []string{`{"version":1,"unknown":1}`, `{"version":1} {}`, `not json`} {
		if Decode([]byte(data), 1024, new(Request)) == nil {
			t.Errorf("accepted %s", data)
		}
	}
}
