package web

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

// Check the component output, not only its name in the template source.
// Stable IDs, labels, native validation, and morph attributes are required
// for the same form to work with and without the action script.
func TestRenderedComponents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		suspend            *runtime.SuspendRequest
		control, tag, slot string
	}{
		{name: "conversation"},
		{name: "start"},
		{name: "runs"},
		{name: "trace"},
		{"approval", &runtime.SuspendRequest{TurnID: "turn-a", Kind: runtime.SuspendApproval}, "respond-approval", "select", "native-select"},
		{"choice", &runtime.SuspendRequest{TurnID: "turn-b", Options: []string{"<First>", "Second"}}, "respond-choice", "select", "native-select"},
		{"answer", &runtime.SuspendRequest{TurnID: "turn-c"}, "respond-answer", "textarea", "textarea"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := view{ID: "chat", Token: "csrf-token", Filter: "<filter>", Runs: []runRow{{ID: "chat", State: runtime.RunWaiting}}, Snapshot: httpchannel.SnapshotResponse{Suspend: tc.suspend}, Trace: []traceRow{{Seq: 1, EntryID: "entry-a", ParentID: "parent-a", Text: "<full text>", PayloadText: `{"key":"<payload>"}`, Tools: []string{"search"}}, {Seq: 2, Turn: 1, Preview: "second"}, {Seq: 3, Turn: 1, Preview: "third"}}}
			v.View = "chat"
			if tc.name == "start" {
				v.ID = ""
			}
			if tc.name == "runs" || tc.name == "trace" {
				v.View = tc.name
			}
			var output bytes.Buffer
			r := httptest.NewRequest("GET", "/web/?run=chat", nil)
			if err := renderComponent(r, page(v), &output); err != nil {
				t.Fatal(err)
			}
			doc, err := html.Parse(strings.NewReader(output.String()))
			if err != nil {
				t.Fatal(err)
			}
			var nodes []*html.Node
			var walk func(*html.Node)
			walk = func(n *html.Node) {
				if n.Type == html.ElementNode {
					nodes = append(nodes, n)
				}
				for child := n.FirstChild; child != nil; child = child.NextSibling {
					walk(child)
				}
			}
			walk(doc)
			attr := func(n *html.Node, key string) (string, bool) {
				for _, a := range n.Attr {
					if a.Key == key {
						return a.Val, true
					}
				}
				return "", false
			}
			find := func(key, value string) *html.Node {
				t.Helper()
				for _, n := range nodes {
					if got, ok := attr(n, key); ok && got == value {
						return n
					}
				}
				t.Fatalf("missing %s=%q", key, value)
				return nil
			}
			check := func(n *html.Node, key, value string) {
				t.Helper()
				if got, ok := attr(n, key); !ok || got != value {
					t.Errorf("%s: %s=%q, want %q", n.Data, key, got, value)
				}
			}
			ids := map[string]bool{}
			for _, n := range nodes {
				if id, ok := attr(n, "id"); ok {
					if ids[id] {
						t.Errorf("duplicate ID %s", id)
					}
					ids[id] = true
				}
			}
			find("data-slot", "button")
			switch tc.name {
			case "runs":
				for _, slot := range []string{"card", "card-header", "card-title", "card-description", "card-content", "table", "table-header", "table-body", "badge", "separator"} {
					find("data-slot", slot)
				}
				check(find("data-slot", "separator"), "aria-orientation", "horizontal")
				check(find("data-slot", "table-head"), "scope", "col")
				// State tags take their color from data-state, not from the variant.
				check(find("data-state", string(runtime.RunWaiting)), "data-slot", "badge")
			case "trace":
				check(find("id", "trace-filter"), "value", v.Filter)
				check(find("id", "trace-filter"), "data-ignore-morph", "")
				find("for", "trace-filter")
				check(find("id", "trace-1"), "data-preserve-attr", "open")
				check(find("data-slot", "table"), "aria-label", "Journal records")
				var headings []string
				for _, n := range nodes {
					if n.Data == "th" && n.Parent.Parent.Data == "thead" {
						check(n, "scope", "col")
						check(n, "data-slot", "table-head")
						headings = append(headings, strings.TrimSpace(n.FirstChild.Data))
					}
				}
				if strings.Join(headings, ",") != "Sequence,Time (UTC),Kind,Role,State,Summary" {
					t.Errorf("headings = %v", headings)
				}
				for _, id := range []string{"trace-turn-0", "trace-turn-1"} {
					n := find("id", id)
					if _, ok := attr(n, "data-stripe"); ok {
						t.Error("turn separator has a stripe")
					}
					check(n.FirstChild, "colspan", "6")
					check(n.FirstChild, "scope", "row")
				}
				if !strings.Contains(output.String(), "Initialization") || strings.Contains(output.String(), "Turn 0") {
					t.Error("initial records need an Initialization heading")
				}
				for i, id := range []string{"trace-row-1", "trace-row-2", "trace-row-3"} {
					n := find("id", id)
					check(n, "data-stripe", []string{"1", "0", "1"}[i])
					cells := 0
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.ElementNode {
							if c.Data != "td" {
								t.Errorf("record row child = %s", c.Data)
							}
							cells++
						}
					}
					if cells != 6 {
						t.Errorf("record cells = %d, want 6", cells)
					}
				}
				detail := find("id", "trace-1")
				if detail.Data != "details" || detail.Parent.Data != "td" || detail.Parent.Parent != find("id", "trace-row-1") || detail.FirstChild.Data != "summary" {
					t.Error("disclosure must be in the summary cell")
				}
				for _, text := range []string{"entry-a", "parent-a", "&lt;full text&gt;", "&lt;payload&gt;", "search"} {
					if !strings.Contains(output.String(), text) {
						t.Errorf("missing full record content %q", text)
					}
				}
			default:
				id := "send-text"
				if tc.name == "start" {
					id = "start-text"
				}
				n := find("id", id)
				find("for", id)
				check(n, "required", "")
				check(n, "data-ignore-morph", "")
				check(find("name", "csrf"), "value", v.Token)
			}
			if tc.suspend != nil {
				find("id", "respond-"+tc.suspend.TurnID)
				check(find("name", "turn"), "value", tc.suspend.TurnID)
				control := find("id", tc.control)
				if control.Data != tc.tag {
					t.Errorf("control = %s, want %s", control.Data, tc.tag)
				}
				check(control, "data-slot", tc.slot)
				check(control, "required", "")
				check(control, "data-ignore-morph", "")
				find("for", tc.control)
				if tc.tag == "select" {
					first := control.FirstChild
					for first != nil && first.Type != html.ElementNode {
						first = first.NextSibling
					}
					if first == nil || first.Data != "option" {
						t.Fatal("no native option")
					}
					want := "reject"
					if tc.name == "choice" {
						want = "<First>"
					}
					check(first, "value", want)
				}
				if tc.name == "approval" {
					check(find("id", "respond-note"), "data-ignore-morph", "")
					find("for", "respond-note")
				}
			}
		})
	}
}

// The bundled Datastar get action retries a clean SSE EOF only with
// retry: 'always'. Check the rendered attribute, including HTML escaping.
func TestLiveConnectionRetriesCleanEOF(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	w := request(h, "GET", "/web/", nil, nil)
	doc, err := html.Parse(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	var visit func(*html.Node)
	var expression string
	visit = func(n *html.Node) {
		var id, init string
		for _, attr := range n.Attr {
			switch attr.Key {
			case "id":
				id = attr.Val
			case "data-init":
				init = attr.Val
			}
		}
		if id == "live-connection" {
			expression = init
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	want := `@get("/web/live?filter=&run=&view=chat", {openWhenHidden: true, retry: 'always'})`
	if expression != want {
		t.Fatalf("live data-init = %q, want %q", expression, want)
	}
}
