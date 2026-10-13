// Package natsapi defines the private versioned Core NATS HTTP wire contract.
package natsapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const Version = 1
const DefaultInbox = "_INBOX.bonnieapi"

// Limits bound all transport-owned buffers and waits. Zero fields use defaults.
type Limits struct {
	BodyBytes        int
	PathBytes        int
	HeaderBytes      int
	HeaderCount      int
	FrameBytes       int
	ChunkBytes       int
	Window           int
	OrdinaryRequests int
	StreamRequests   int
	ClientRequests   int
	Lease            time.Duration
	Heartbeat        time.Duration
	Startup          time.Duration
	AckTimeout       time.Duration
}

func (l Limits) Defaults() (Limits, error) {
	ints := []*int{&l.BodyBytes, &l.PathBytes, &l.HeaderBytes, &l.HeaderCount, &l.FrameBytes, &l.ChunkBytes, &l.Window, &l.OrdinaryRequests, &l.StreamRequests, &l.ClientRequests}
	defaults := []int{1 << 20, 8 << 10, 16 << 10, 64, 64 << 10, 32 << 10, 8, 64, 16, 64}
	for i, p := range ints {
		if *p < 0 {
			return l, errors.New("nats-api: negative limit")
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	durations := []*time.Duration{&l.Lease, &l.Heartbeat, &l.Startup, &l.AckTimeout}
	for i, p := range durations {
		if *p < 0 {
			return l, errors.New("nats-api: negative duration")
		}
		if *p == 0 {
			*p = []time.Duration{30 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second}[i]
		}
	}
	if l.Heartbeat*2 >= l.Lease || l.ChunkBytes > 32<<10 || l.Window > 8 || l.FrameBytes < 1024 {
		return l, errors.New("nats-api: incompatible limits")
	}
	return l, nil
}

type Request struct {
	Version   int         `json:"version"`
	ID        string      `json:"request_id"`
	Method    string      `json:"method"`
	Path      string      `json:"path"`
	Headers   http.Header `json:"headers,omitempty"`
	Body      []byte      `json:"body,omitempty"`
	TimeoutMS int64       `json:"timeout_ms,omitempty"`
	Secret    string      `json:"control_secret"`
}

type Frame struct {
	Version int         `json:"version"`
	ID      string      `json:"request_id"`
	Seq     uint64      `json:"sequence"`
	Kind    string      `json:"kind"`
	Status  int         `json:"status,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Data    []byte      `json:"data,omitempty"`
	Code    string      `json:"code,omitempty"`
	LeaseMS int64       `json:"lease_ms,omitempty"`
	Window  int         `json:"window,omitempty"`
}

type Control struct {
	Version int    `json:"version"`
	ID      string `json:"request_id"`
	Inbox   string `json:"reply_inbox"`
	Secret  string `json:"secret"`
	Seq     uint64 `json:"sequence"`
	Kind    string `json:"kind"`
	Ack     uint64 `json:"acknowledged_sequence,omitempty"`
}

func Token(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func ValidToken(s string, n int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == n && s == strings.ToLower(s)
}
func Subject(s string) bool {
	if s == "" || strings.ContainsAny(s, "* >\t\r\n") {
		return false
	}
	return !slices.Contains(strings.Split(s, "."), "")
}
func Inbox(s, prefix string) bool {
	return Subject(prefix) && strings.HasPrefix(s, prefix+".") && ValidToken(strings.TrimPrefix(s, prefix+"."), 16)
}

func Decode(data []byte, max int, v any) error {
	if len(data) > max {
		return errors.New("too_large")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid_request")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid_request")
	}
	return nil
}

func Path(s string, max int) (*url.URL, error) {
	bad := func() (*url.URL, error) { return nil, errors.New("invalid_request") }
	if len(s) > max || !strings.HasPrefix(s, "/bonnie/v1/") || strings.Contains(s, "#") {
		return bad()
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return bad()
		}
	}
	u, err := url.ParseRequestURI(s)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return bad()
	}
	for part := range strings.SplitSeq(u.Path, "/") {
		if part == "." || part == ".." {
			return bad()
		}
	}
	for _, r := range u.Path {
		if r < 32 || r == 127 || r == '\\' {
			return bad()
		}
	}
	return u, nil
}

var responseHeaders = map[string]bool{"Content-Type": true, "Cache-Control": true, "Allow": true, "Www-Authenticate": true, "Retry-After": true}
var denied = map[string]bool{"Host": true, "Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Forwarded": true}

func Headers(h http.Header, extra []string, response bool, l Limits) error {
	allowed := map[string]bool{"Authorization": true, "Content-Type": true, "Accept": true}
	if response {
		allowed = responseHeaders
	} else {
		for _, k := range extra {
			allowed[http.CanonicalHeaderKey(k)] = true
		}
	}
	count, size := 0, 0
	for k, vs := range h {
		canonical := http.CanonicalHeaderKey(k)
		lower := strings.ToLower(k)
		if !allowed[canonical] || denied[canonical] || strings.HasPrefix(lower, "proxy-") || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "nats-") || strings.Contains(lower, "identity") || strings.HasPrefix(lower, "x-auth-") {
			return errors.New("invalid_request")
		}
		for _, c := range k {
			letterOrDigit := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
			if !letterOrDigit && !strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
				return errors.New("invalid_request")
			}
		}
		count += len(vs)
		size += len(k)
		for _, v := range vs {
			if strings.ContainsAny(v, "\r\n") {
				return errors.New("invalid_request")
			}
			size += len(v)
		}
	}
	if count > l.HeaderCount || size > l.HeaderBytes {
		return errors.New("too_large")
	}
	return nil
}
func ResponseHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for k, v := range h {
		if responseHeaders[http.CanonicalHeaderKey(k)] {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	return out
}
func (r Request) Validate(l Limits, extra []string) error {
	if r.Version != Version {
		return errors.New("unsupported_version")
	}
	if !ValidToken(r.ID, 16) || !ValidToken(r.Secret, 32) || r.TimeoutMS < 0 || r.TimeoutMS > int64((24*time.Hour)/time.Millisecond) {
		return errors.New("invalid_request")
	}
	switch r.Method {
	case "GET", "HEAD", "POST", "DELETE":
	default:
		return errors.New("invalid_request")
	}
	if _, err := Path(r.Path, l.PathBytes); err != nil {
		return err
	}
	if len(r.Body) > l.BodyBytes {
		return errors.New("too_large")
	}
	return Headers(r.Headers, extra, false, l)
}
func Stream(r Request) bool {
	u, err := Path(r.Path, 1<<20)
	if err != nil || r.Method != "GET" {
		return false
	}
	p := strings.Split(strings.TrimPrefix(u.Path, "/bonnie/v1/"), "/")
	return len(p) == 3 && p[0] == "runs" && p[1] != "" && p[2] == "stream"
}
