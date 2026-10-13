// Package natsapi implements the private version-one HTTP framing protocol.
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
	"unicode"
)

const Version = 1
const DefaultInboxPrefix = "_INBOX.bonnieapi"

// Limits bound independently admitted requests and per-response memory.
type Limits struct {
	MaxRequests  int
	MaxStreams   int
	MaxBody      int
	MaxFrame     int
	ChunkSize    int
	Window       int
	Lease        time.Duration
	Renew        time.Duration
	AckTimeout   time.Duration
	SetupTimeout time.Duration
}

func (l Limits) Defaults() (Limits, error) {
	if l.MaxRequests == 0 {
		l.MaxRequests = 64
	}
	if l.MaxStreams == 0 {
		l.MaxStreams = 16
	}
	if l.MaxBody == 0 {
		l.MaxBody = 1 << 20
	}
	if l.MaxFrame == 0 {
		l.MaxFrame = 64 << 10
	}
	if l.ChunkSize == 0 {
		l.ChunkSize = 32 << 10
	}
	if l.Window == 0 {
		l.Window = 8
	}
	if l.Lease == 0 {
		l.Lease = 30 * time.Second
	}
	if l.Renew == 0 {
		l.Renew = 10 * time.Second
	}
	if l.AckTimeout == 0 {
		l.AckTimeout = 10 * time.Second
	}
	if l.SetupTimeout == 0 {
		l.SetupTimeout = 10 * time.Second
	}
	if l.MaxRequests < 1 || l.MaxStreams < 1 || l.MaxBody < 1 || l.MaxBody > 1<<20 || l.MaxFrame < 1024 || l.MaxFrame > 64<<10 || l.ChunkSize < 1 || l.ChunkSize > 32<<10 || l.Window < 1 || l.Window > 8 || l.Lease <= 0 || l.Renew <= 0 || l.Renew*2 >= l.Lease || l.AckTimeout <= 0 || l.SetupTimeout <= 0 {
		return l, errors.New("nats-api: invalid limits")
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
	Secret    string      `json:"secret"`
}

type Frame struct {
	Version  int         `json:"version"`
	ID       string      `json:"request_id"`
	Sequence uint64      `json:"sequence"`
	Kind     string      `json:"kind"`
	Status   int         `json:"status,omitempty"`
	Headers  http.Header `json:"headers,omitempty"`
	Data     []byte      `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	LeaseMS  int64       `json:"lease_ms,omitempty"`
	RenewMS  int64       `json:"renew_ms,omitempty"`
	Window   int         `json:"window,omitempty"`
}

type Control struct {
	Version  int    `json:"version"`
	ID       string `json:"request_id"`
	Inbox    string `json:"inbox"`
	Secret   string `json:"secret"`
	Sequence uint64 `json:"sequence"`
	Kind     string `json:"kind"`
	Ack      uint64 `json:"ack,omitempty"`
}

func Decode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errors.New("invalid_request")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid_request")
	}
	return nil
}

func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func IsToken(s string, n int) bool {
	if len(s) != 2*n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func Subject(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	if slices.Contains(strings.Split(s, "."), "") {
		return false
	}
	return !strings.ContainsAny(s, "*> \t\r\n") && !strings.ContainsFunc(s, unicode.IsControl)
}
func Inbox(s, prefix string) bool {
	return strings.HasPrefix(s, prefix+".") && IsToken(strings.TrimPrefix(s, prefix+"."), 16)
}
func Path(s string) (*url.URL, error) {
	if len(s) > 8192 || !strings.HasPrefix(s, "/bonnie/v1/") || strings.ContainsAny(s, "#\\") || strings.ContainsFunc(s, unicode.IsControl) {
		return nil, errors.New("invalid_request")
	}
	u, err := url.ParseRequestURI(s)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.ContainsFunc(u.Path, unicode.IsControl) {
		return nil, errors.New("invalid_request")
	}
	for p := range strings.SplitSeq(u.Path, "/") {
		if p == "." || p == ".." {
			return nil, errors.New("invalid_request")
		}
	}
	return u, nil
}
func Stream(u *url.URL, method string) bool {
	p := strings.Split(u.Path, "/")
	return method == http.MethodGet && len(p) == 6 && p[1] == "bonnie" && p[2] == "v1" && p[3] == "runs" && p[4] != "" && p[5] == "stream"
}

var responseHeaders = map[string]bool{"Content-Type": true, "Cache-Control": true, "Allow": true, "Www-Authenticate": true, "Retry-After": true}

func ResponseHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for k, v := range h {
		k = http.CanonicalHeaderKey(k)
		if responseHeaders[k] {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}
func Headers(h http.Header, extra []string) error {
	allowed := map[string]bool{"Authorization": true, "Content-Type": true, "Accept": true}
	for _, k := range extra {
		k = http.CanonicalHeaderKey(k)
		if denied(k) || !headerName(k) {
			return errors.New("invalid_request")
		}
		allowed[k] = true
	}
	size, count := 0, 0
	for k, vs := range h {
		canonical := http.CanonicalHeaderKey(k)
		if !headerName(k) || denied(canonical) || !allowed[canonical] {
			return errors.New("invalid_request")
		}
		for _, v := range vs {
			count++
			size += len(k) + len(v)
			if strings.ContainsFunc(v, unicode.IsControl) {
				return errors.New("invalid_request")
			}
		}
	}
	if count > 64 || size > 16<<10 {
		return errors.New("too_large")
	}
	return nil
}
func headerName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			return false
		}
	}
	return true
}
func denied(s string) bool {
	s = strings.ToLower(s)
	switch s {
	case "host", "connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade", "forwarded", "via", "cookie", "content-length":
		return true
	}
	return strings.HasPrefix(s, "proxy-") || strings.HasPrefix(s, "x-forwarded-") || strings.HasPrefix(s, "x-identity") || strings.HasPrefix(s, "x-user") || strings.HasPrefix(s, "x-auth") || strings.HasPrefix(s, "nats-")
}
func Validate(r Request, maxBody int, extra []string) error {
	if r.Version != Version {
		return errors.New("unsupported_version")
	}
	if !IsToken(r.ID, 16) || !IsToken(r.Secret, 32) || r.TimeoutMS < 0 || r.TimeoutMS > int64((24*time.Hour)/time.Millisecond) {
		return errors.New("invalid_request")
	}
	switch r.Method {
	case "GET", "HEAD", "POST", "DELETE":
	default:
		return errors.New("invalid_request")
	}
	if _, err := Path(r.Path); err != nil {
		return err
	}
	if len(r.Body) > maxBody {
		return errors.New("too_large")
	}
	return Headers(r.Headers, extra)
}
