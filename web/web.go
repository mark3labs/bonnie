// Package web provides an optional, same-origin operator UI for BONNIE.
// Mount New at /web and /web/. The host must also mount the HTTP channel.
// Authentication is deployment-wide, not per-run access control.
package web

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	kit "github.com/mark3labs/kit/pkg/kit"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

//go:generate go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate

//go:embed assets/*
var assets embed.FS

// New returns a web UI that uses the HTTP channel for all run actions.
// Reads, assets, and SSE use the same authenticator as the channel. Actions
// also require a same-origin request and a CSRF token. No credentials are
// stored in browser storage. Supply a cookie or proxy authenticator for a
// browser deployment; this UI does not implement a bearer-token login.
// The caller owns the runner and journal. The handler does not close them.
func New(runner *runtime.Runner, journal runtime.Journal, httpChannel *httpchannel.Channel) http.Handler {
	if runner == nil || journal == nil || httpChannel == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Web UI is not configured", http.StatusServiceUnavailable)
		})
	}
	s := &server{journal: journal, runner: runner, api: httpChannel.Handler()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /web", s.page)
	mux.HandleFunc("GET /web/{$}", s.page)
	mux.HandleFunc("GET /web/live", s.live)
	local, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	} // The embedded directory is fixed at build time.
	mux.Handle("GET /web/assets/", http.StripPrefix("/web/assets/", http.FileServerFS(local)))
	mux.Handle("POST /web/actions/{action}", http.NewCrossOriginProtection().Handler(http.HandlerFunc(s.action)))
	protected := httpChannel.Protect(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		protected.ServeHTTP(w, r)
	})
}

type server struct {
	journal runtime.Journal
	runner  *runtime.Runner
	api     http.Handler
}
type runRow struct {
	ID    string
	State runtime.RunState
}
type traceRow struct {
	runtime.Record
	PayloadText string
	Preview     string
	Tools       []string
	Turn        int
}
type view struct {
	Runs                    []runRow
	ID, Filter, Token, View string
	Snapshot                httpchannel.SnapshotResponse
	Messages                []message
	Trace                   []traceRow
}
type message struct {
	Role, Text, Payload, HTML string
	Tools                     []toolActivity
}
type toolActivity struct{ Name, Kind, Summary, Detail string }

func (s *server) load(r *http.Request) (view, error) {
	v := view{ID: r.URL.Query().Get("run"), Filter: r.URL.Query().Get("filter"), View: r.URL.Query().Get("view")}
	if v.View == "" {
		v.View = "chat"
	}
	if v.View != "chat" && v.View != "runs" && v.View != "trace" {
		return v, errInvalidView
	}
	ids, err := s.journal.Runs(r.Context(), "")
	if err != nil {
		return v, err
	}
	sort.Strings(ids)
	for _, id := range ids {
		if runtime.IsReservedRun(id) {
			continue
		}
		state, err := s.journal.State(r.Context(), id)
		if err != nil {
			return v, err
		}
		v.Runs = append(v.Runs, runRow{id, state})
	}
	if v.ID == "" {
		return v, nil
	}
	if runtime.IsReservedRun(v.ID) {
		return v, runtime.ErrRunNotFound
	}
	recs, err := s.journal.Replay(r.Context(), v.ID)
	if err != nil {
		return v, err
	}
	// The channel snapshot selects the current branch without changing the journal.
	req := r.Clone(r.Context())
	req.URL = &url.URL{Path: "/bonnie/v1/runs/" + url.PathEscape(v.ID) + "/snapshot"}
	req.Method = http.MethodGet
	req.Body = nil
	reply := httptest.NewRecorder()
	s.api.ServeHTTP(reply, req)
	if reply.Code != http.StatusOK {
		return v, fmt.Errorf("bonnie: web: snapshot status %d", reply.Code)
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &v.Snapshot); err != nil {
		return v, err
	}
	chatToolNames := make(map[string]string)
	for _, msg := range v.Snapshot.Messages {
		m := message{Role: string(msg.Role)}
		for _, part := range msg.Content {
			if text, ok := part.(kit.LLMTextPart); ok {
				m.Text += text.Text
			}
		}
		payload, err := json.MarshalIndent(msg, "", "  ")
		if err != nil {
			return v, err
		}
		if m.Role == "assistant" {
			m.HTML, err = renderMarkdown(m.Text)
			if err != nil {
				return v, err
			}
		}
		m.Tools = chatTools(msg, chatToolNames)
		m.Payload = string(payload)
		v.Messages = append(v.Messages, m)
	}
	turn := 0
	toolNames := make(map[string]string)
	for _, rec := range recs {
		if rec.Kind == runtime.RecordTurn {
			turn++
		}
		preview, tools := traceDisplay(rec, toolNames)
		payload := pretty(rec.Payload)
		search := fmt.Sprintf("%d %s %s %s %s %s", rec.Seq, rec.Kind, rec.Role, rec.State, rec.Text, payload)
		if v.Filter != "" && !strings.Contains(strings.ToLower(search), strings.ToLower(v.Filter)) {
			continue
		}
		v.Trace = append(v.Trace, traceRow{Record: rec, PayloadText: payload, Preview: preview, Tools: tools, Turn: turn})
	}
	return v, nil
}

func pretty(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") == nil {
		return b.String()
	}
	return string(raw)
}
func token(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("bonnie_web_csrf"); err == nil && len(c.Value) == 64 {
		return c.Value
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	value := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "bonnie_web_csrf", Value: value, Path: "/web", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
	return value
}
func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	var b bytes.Buffer
	if err := renderComponent(r, c, &b); err != nil {
		http.Error(w, "Cannot render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
func (s *server) page(w http.ResponseWriter, r *http.Request) {
	v, err := s.load(r)
	if err != nil {
		readError(w, err)
		return
	}
	v.Token = token(w, r)
	if r.Header.Get("Datastar-Request") == "true" {
		w.Header().Set("Content-Type", "text/event-stream")
		if err := patch(w, r, application(v)); err != nil {
			return
		}
		return
	}
	render(w, r, page(v))
}

var errInvalidView = errors.New("bonnie: web: invalid view")

func readError(w http.ResponseWriter, err error) {
	if errors.Is(err, errInvalidView) {
		http.Error(w, "Invalid view", http.StatusBadRequest)
		return
	}
	if errors.Is(err, runtime.ErrRunNotFound) {
		http.Error(w, "Run not found", 404)
		return
	}
	http.Error(w, "Cannot read journal", 500)
}

func (s *server) action(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	c, err := r.Cookie("bonnie_web_csrf")
	if err != nil || len(c.Value) != 64 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostForm.Get("csrf"))) != 1 {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	action, id := r.PathValue("action"), r.PostForm.Get("run")
	path := "/bonnie/v1/runs"
	var body any
	switch action {
	case "ensure":
		address := r.PostForm.Get("address")
		if address == "" || strings.ContainsAny(address, "/\\\\") {
			http.Error(w, "Invalid address", 400)
			return
		}
		path = "/bonnie/v1/addresses/" + url.PathEscape("web-"+address)
		body = struct{}{}
	case "start":
		body = httpchannel.StartRequest{Text: r.PostForm.Get("text"), Title: r.PostForm.Get("title")}
	case "send":
		body = httpchannel.SendRequest{Text: r.PostForm.Get("text")}
	case "respond":
		answer := runtime.InputResponse{Text: r.PostForm.Get("text"), TurnID: r.PostForm.Get("turn")}
		if choice := r.PostForm.Get("approval"); choice != "" {
			if choice != "approve" && choice != "reject" {
				http.Error(w, "Invalid approval", 400)
				return
			}
			yes := choice == "approve"
			answer.Approved = &yes
		}
		body = httpchannel.RespondRequest{Responses: []runtime.InputResponse{answer}}
	case "cancel":
		body = map[string]string{"turn_id": r.PostForm.Get("turn")}
	case "reset":
		body = httpchannel.ResetRequest{Reason: r.PostForm.Get("reason")}
	case "clear", "compact":
		body = struct{}{}
	default:
		http.NotFound(w, r)
		return
	}
	if action != "start" && action != "ensure" {
		if id == "" || runtime.IsReservedRun(id) || strings.ContainsAny(id, "/\\") {
			http.Error(w, "Invalid run", 400)
			return
		}
		path += "/" + url.PathEscape(id)
		if action != "send" {
			path += "/" + action
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "Invalid action", 400)
		return
	}
	req := r.Clone(r.Context())
	req.URL = &url.URL{Path: path}
	req.Body = http.NoBody
	req.Body = io.NopCloser(bytes.NewReader(encoded))
	req.ContentLength = int64(len(encoded))
	req.Form = nil
	req.PostForm = nil
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	s.api.ServeHTTP(w, req)
}

// live subscribes before it reads the snapshot, so a concurrent change is not
// lost. Reconnects read durable state; transient text is not journal history.
func (s *server) live(w http.ResponseWriter, r *http.Request) {
	events, stop := s.runner.Events().SubscribeAll()
	defer stop()
	v, err := s.load(r)
	if err != nil {
		readError(w, err)
		return
	}
	c, err := r.Cookie("bonnie_web_csrf")
	if err != nil || len(c.Value) != 64 {
		http.Error(w, "Open the web page first", 400)
		return
	}
	v.Token = c.Value
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming is not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := patch(w, r, panel(v)); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var text string
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			if runtime.IsReservedRun(ev.RunID) {
				continue
			}
			if v.View != "runs" && ev.RunID != v.ID {
				continue
			}
			if ev.RunID == v.ID && v.View == "chat" {
				switch ev.Type {
				case string(kit.EventMessageUpdate):
					var update kit.MessageUpdateEvent
					if json.Unmarshal(ev.Data, &update) != nil || update.Chunk == "" {
						continue
					}
					text += update.Chunk
					if err := patch(w, r, streamingMessage(text)); err != nil {
						return
					}
					flusher.Flush()
					continue
				case string(kit.EventMessageStart), string(kit.EventMessageEnd), runtime.EventTurn,
					runtime.EventResponse, runtime.EventSuspend:
					text = ""
				case runtime.EventState:
					if ev.State != runtime.RunRunning {
						text = ""
					}
				}
			}
			// Kit events can precede the step commit. Runtime events follow durable
			// writes; step completion also refreshes tool activity and the trace.
			if !strings.HasPrefix(ev.Type, "run_") && ev.Type != string(kit.EventStepFinish) {
				continue
			}
			v, err = s.load(r)
			if err != nil {
				return
			}
			v.Token = c.Value
			if err := patch(w, r, panel(v)); err != nil {
				return
			}
			if text != "" {
				if err := patch(w, r, streamingMessage(text)); err != nil {
					return
				}
			}
			flusher.Flush()
		}
	}
}

// patch writes a Datastar element morph. templ escapes model text before it
// reaches the wire. Each HTML line has its own SSE data field.
func patch(w http.ResponseWriter, r *http.Request, component templ.Component) error {
	var b bytes.Buffer
	if err := renderComponent(r, component, &b); err != nil {
		return err
	}
	var event strings.Builder
	event.WriteString("event: datastar-patch-elements\ndata: mode outer\n")
	for line := range strings.SplitSeq(b.String(), "\n") {
		event.WriteString("data: elements " + line + "\n")
	}
	event.WriteString("\n")
	_, err := io.WriteString(w, event.String())
	return err
}

func liveURL(v view) string {
	return "/web/live?" + url.Values{"run": {v.ID}, "filter": {v.Filter}, "view": {v.View}}.Encode()
}
func runURL(id string) string { return "/web/?" + url.Values{"run": {id}}.Encode() }

func viewURL(name, id string) string {
	return "/web/?" + url.Values{"view": {name}, "run": {id}}.Encode()
}
func currentView(v view, name string) string {
	if v.View == name || (v.View == "" && name == "chat") {
		return "page"
	}
	return "false"
}

// shadcn-templ uses a process-wide class merger with mutable caches. Serialize
// component rendering, not requests or streams, to keep those caches safe.
var componentMu sync.Mutex

func renderComponent(r *http.Request, c templ.Component, b *bytes.Buffer) error {
	componentMu.Lock()
	defer componentMu.Unlock()
	return c.Render(r.Context(), b)
}
