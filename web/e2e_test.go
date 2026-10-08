package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// Use the public Kit provider seam, but do not read host configuration or
// discover tools, context, skills, extensions, or agents from the machine.
func webScriptedFactory(model *fakemodel.Model) runtime.AgentFactory {
	return runtime.KitAgent(func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}, model.Option())
}

func webScriptedHandler(j *runtime.SQLiteJournal, model *fakemodel.Model) http.Handler {
	runner := runtime.NewRunner(j, webScriptedFactory(model))
	channel := httpchannel.New(runner)
	mux := http.NewServeMux()
	ui := New(runner, j, channel)
	mux.Handle("/web", ui)
	mux.Handle("/web/", ui)
	mux.Handle("/bonnie/", channel.Handler())
	return mux
}

// A single test message waits for cancellation without a model request.
type browserStopAgent struct{ runtime.Agent }

func (a *browserStopAgent) PromptResult(ctx context.Context, text string) (*kit.TurnResult, error) {
	if text == "stop browser turn" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.Agent.PromptResult(ctx, text)
}

// This is a real browser against New, the HTTP channel, SQLite, Runner, and
// Kit. Node's built-in WebSocket speaks CDP; no npm install or model key is
// needed. A missing browser or Node with no WebSocket skips only this test.
func TestBrowserEndToEnd(t *testing.T) {
	t.Parallel()
	browser := ""
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			browser = path
			break
		}
	}
	if browser == "" {
		t.Skip("Chromium is not installed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, node, "-e", "if(typeof WebSocket !== 'function')process.exit(1)").Run(); err != nil {
		t.Skip("Node with built-in WebSocket is required")
	}
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	model := fakemodel.New(fakemodel.Say("## First reply\n\nA **helpful** answer.\n\n- One\n- Two\n\n```go\nfmt.Println(\"hello\")\n```"), fakemodel.Say("Followup received."), fakemodel.Say("Fresh context."), fakemodel.Call("request_approval", `{"action":"publish the report"}`), fakemodel.Say("Approved report."))
	factory := webScriptedFactory(model)
	runner := runtime.NewRunner(j, func(ctx context.Context, session *runtime.Session) (runtime.Agent, error) {
		agent, err := factory(ctx, session)
		if err != nil {
			return nil, err
		}
		return &browserStopAgent{Agent: agent}, nil
	})
	channel := httpchannel.New(runner)
	mux := http.NewServeMux()
	mux.Handle("/web/", New(runner, j, channel))
	mux.Handle("/bonnie/", channel.Handler())
	server := httptest.NewServer(mux)
	defer server.Close()
	profile := filepath.Join(dir, "chromium")
	log, err := os.Create(filepath.Join(dir, "chromium.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	}()
	cmd := exec.CommandContext(ctx, browser, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "--user-data-dir="+profile, "about:blank")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var port string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err == nil {
			port = strings.Split(string(data), "\n")[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if port == "" {
		data, _ := os.ReadFile(log.Name())
		t.Fatalf("Chromium did not start: %s", data)
	}
	output, err := exec.CommandContext(ctx, node, "-e", browserE2E, "http://127.0.0.1:"+port, server.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("browser E2E: %v\n%s", err, output)
	}
	t.Log(string(output))
	requests := model.Requests()
	if len(requests) != 5 {
		t.Fatalf("model calls = %d, want 5", len(requests))
	}
	if !strings.Contains(requests[1].Text(), "First reply") || !strings.Contains(requests[1].Text(), "browser followup") {
		t.Fatal("followup did not replay the conversation")
	}
	if strings.Contains(requests[2].Text(), "browser followup") || strings.Contains(requests[2].Text(), "First reply") {
		t.Fatal("clear did not remove old model context")
	}
	if !strings.Contains(requests[4].Text(), "approved") {
		t.Fatalf("approval did not reach model: %s", requests[4].Text())
	}
	ids, err := j.Runs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	retired := false
	visible := 0
	for _, id := range ids {
		if runtime.IsReservedRun(id) {
			continue
		}
		visible++
		state, err := j.State(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if state == runtime.RunRetired {
			retired = true
		}
	}
	if visible != 2 || !retired {
		t.Fatalf("want two visible runs and one retired run: runs=%v, retired=%v", ids, retired)
	}
}

// TestWebDemo is opt-in and runs until the test process is stopped. It never
// uses a live provider. Both variables are required; the address must be a
// loopback IP. The journal path is durable, not a test temporary directory.
func TestWebDemo(t *testing.T) {
	addr, root := os.Getenv("BONNIE_WEB_DEMO_ADDR"), os.Getenv("BONNIE_WEB_DEMO_JOURNAL")
	if addr == "" || root == "" {
		t.Skip("set BONNIE_WEB_DEMO_ADDR and BONNIE_WEB_DEMO_JOURNAL")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("BONNIE_WEB_DEMO_ADDR must use a loopback IP and port")
	}
	j, err := runtime.OpenSQLiteJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	replies := make([]fakemodel.Reply, 10000)
	for i := range replies {
		replies[i] = fakemodel.Say(fmt.Sprintf("## Demo reply %d\n\nThis is a **scripted offline answer**, not a live model.\n\n- Send a followup to test conversation history.\n- Open **Message content** or a journal record to inspect the data.\n- Use the trace filter to find records.\n- **Clear** removes model context but keeps the journal.\n- **Reset** retires the run and keeps its history.\n\n```go\nfmt.Println(\"Hello from BONNIE\")\n```\n\nYour messages are stored in the SQLite journal.", i+1))
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: webScriptedHandler(j, fakemodel.New(replies...)), ReadHeaderTimeout: 5 * time.Second}
	t.Logf("Offline web demo: http://%s/web/ (journal: %s; 10000 scripted replies per process)", addr, root)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

const browserE2E = `
(async () => {
 const [debug, base] = process.argv.slice(1);
 const targets = await (await fetch(debug + '/json/list')).json();
 const ws = new WebSocket(targets.find(t => t.type === 'page').webSocketDebuggerUrl);
 await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });
 let seq = 0, streams = 0;
 const pending = new Map(), errors = [], external = [];
 ws.onmessage = event => {
  const m = JSON.parse(event.data);
  if (m.id) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.reject(m.error) : p.resolve(m.result); }
  if (m.method === 'Network.requestWillBeSent' && /^https?:/.test(m.params.request.url) && !m.params.request.url.startsWith(base + '/')) external.push(m.params.request.url);
  if (m.method === 'Runtime.exceptionThrown') errors.push(m.params.exceptionDetails.text + ' ' + JSON.stringify(m.params.exceptionDetails.exception));
  if (m.method === 'Network.responseReceived' && m.params.response.mimeType === 'text/event-stream') streams++;
 };
 const call = (method, params = {}) => new Promise((resolve, reject) => { const id = ++seq; pending.set(id, {resolve, reject}); ws.send(JSON.stringify({id, method, params})); });
 const evaluate = async expression => {
  const r = await call('Runtime.evaluate', {expression, returnByValue: true, awaitPromise: true});
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails));
  return r.result.value;
 };
 const wait = async (expression, label) => {
  const deadline = Date.now() + 12000;
  while (Date.now() < deadline) { if (await evaluate(expression)) return; await new Promise(r => setTimeout(r, 50)); }
  throw new Error('Timed out: ' + label + '\n' + await evaluate('document.body.innerText'));
 };
 const submit = async (action, text) => evaluate('(() => { const f = document.querySelector(' + JSON.stringify('form[action="/web/actions/' + action + '"]') + '); ' + (text === undefined ? '' : 'f.querySelector("[name=text]").value=' + JSON.stringify(text) + ';') + ' f.requestSubmit(); })()');
 try {
  await call('Runtime.enable'); await call('Page.enable'); await call('Network.enable');
  await call('Page.addScriptToEvaluateOnNewDocument', {source: "window.patchCount=0;document.addEventListener('datastar-fetch',e=>{if(e.detail.type==='datastar-patch-elements')window.patchCount++});"});
  await call('Page.navigate', {url: base + '/web/'});
  await wait('document.querySelector("#connection-status")?.dataset.state === "connected"', 'initial live connection');
  if (!await evaluate("(() => { const text=document.querySelector('#start-text'); return !text.checkValidity() && text.labels[0].textContent.trim()==='Message' && !document.querySelector('#start-title') && !document.querySelector('.record') && !document.querySelector('.runs') && getComputedStyle(document.querySelector('.chat')).display==='flex' && getComputedStyle(document.querySelector('.composer')).borderTopLeftRadius==='0px' && getComputedStyle(document.querySelector('.composer .cn-label')).position==='absolute' && [...document.querySelectorAll('.brand-logo, .welcome-logo')].length===2 && [...document.querySelectorAll('.brand-logo, .welcome-logo')].every(i=>i.complete && i.naturalWidth>0 && i.alt===''); })()")) throw new Error('Entry composer, logo, or chat layout failed');
  await submit('start', 'browser first message');
  await wait('document.querySelector(".markdown h2")?.textContent === "First reply"', 'new conversation and Markdown');
  await wait('document.querySelector("#connection-status").dataset.state === "connected"', 'run SSE');
  if (!await evaluate('!!document.querySelector(".markdown strong") && document.querySelectorAll(".markdown li").length===2 && !!document.querySelector(".markdown pre code")')) throw new Error('Markdown was not rendered');
  if (!await evaluate("document.querySelector('#send-text').labels[0].textContent.trim()==='Message' && !document.querySelector('.record') && !document.querySelector('.controls')")) throw new Error('Chat is not separate');
  if (!await evaluate("(() => { const user=document.querySelector('.message[data-role=user]'), assistant=document.querySelector('.message[data-role=assistant]'), composer=document.querySelector('.composer'); return document.querySelector('.chat-heading h1').textContent.trim()==='# '+new URL(location.href).searchParams.get('run') && getComputedStyle(user).borderLeftWidth==='3px' && getComputedStyle(user).borderLeftColor===getComputedStyle(composer).borderLeftColor && getComputedStyle(assistant).borderBottomWidth==='0px' && getComputedStyle(composer).backgroundColor===getComputedStyle(user).backgroundColor && getComputedStyle(document.body).fontFamily.includes('monospace') && [...document.querySelectorAll('body *')].every(e=>{const c=getComputedStyle(e);return c.borderTopLeftRadius==='0px' && c.borderBottomRightRadius==='0px' && c.boxShadow==='none' && !c.backgroundImage.includes('gradient') && c.backdropFilter==='none';}) && document.querySelector('.composer-state').textContent.includes('completed'); })()")) throw new Error('Chat structure failed');
  for (const [theme, bg, brand, user, surface] of [
   ['light','rgb(248, 247, 249)','rgb(168, 32, 101)','rgb(0, 106, 158)','rgb(255, 255, 255)'],
   ['dark','rgb(48, 48, 48)','rgb(255, 95, 175)','rgb(0, 175, 255)','rgb(38, 38, 38)']
  ]) {
   await call('Emulation.setEmulatedMedia', {features:[{name:'prefers-color-scheme',value:theme}]});
   // Wait until the browser applies the selected color scheme.
   await wait('(() => { const body=getComputedStyle(document.body), button=getComputedStyle(document.querySelector("#send-form button")), user=getComputedStyle(document.querySelector(".message[data-role=user] .message-role")), composer=getComputedStyle(document.querySelector(".composer")); return body.backgroundColor==='+JSON.stringify(bg)+' && button.backgroundColor==='+JSON.stringify(brand)+' && user.color==='+JSON.stringify(user)+' && composer.backgroundColor==='+JSON.stringify(surface)+'; })()', 'chat palette: '+theme);
  }
  await call('Emulation.setEmulatedMedia', {features:[]});
  await call('Emulation.setDeviceMetricsOverride', {width:390,height:844,deviceScaleFactor:1,mobile:false});
  if (!await evaluate('document.documentElement.scrollWidth<=390 && document.querySelector(".composer").getBoundingClientRect().bottom<=844')) throw new Error('Chat mobile overflow');
  await call('Emulation.clearDeviceMetricsOverride');
  const runURL = await evaluate('location.href');
  const navigateView = async view => {
   await evaluate('document.querySelector(' + JSON.stringify('.view-nav a[href*="view=' + view + '"]') + ').click()');
   await wait('document.body?.dataset.view===' + JSON.stringify(view) + ' && document.querySelector("#connection-status").dataset.state==="connected"', view + ' navigation');
  };
  await evaluate('document.querySelector(".message details summary").click()');
  const patches = await evaluate('window.patchCount');
  await evaluate('const t=document.querySelector("#send-text");t.value="browser followup";t.focus()');
  await call('Input.dispatchKeyEvent', {type:'keyDown', key:'Enter', code:'Enter', modifiers:2, windowsVirtualKeyCode:13});
  await call('Input.dispatchKeyEvent', {type:'keyUp', key:'Enter', code:'Enter', modifiers:2, windowsVirtualKeyCode:13});
  await wait('document.querySelector("#action-status").textContent === "Action completed."', 'send action');
  await evaluate('const input=document.querySelector("#send-text");input.value="unfinished draft";input.focus();input.setSelectionRange(3,8)');
  await wait('document.querySelector("#live-panel").innerText.includes("Followup received.") && window.patchCount > ' + patches, 'followup through actual SSE');
  if (await evaluate('location.href') !== runURL) throw new Error('followup navigated instead of using SSE');
  if (!await evaluate('document.querySelector(".message details").open && document.querySelector("#send-text").value==="unfinished draft" && document.querySelector("#send-text").selectionStart===3 && document.querySelector("#send-text").selectionEnd===8')) throw new Error('live patch lost draft or disclosures');
  await navigateView('trace');
  if (!await evaluate('!document.querySelector("#send-text") && !document.querySelector(".runs") && document.querySelectorAll(".record").length>0')) throw new Error('Trace is not separate');
  if (!await evaluate("(() => {    const table=document.querySelector('.trace-table');    const headings=[...table.querySelectorAll('thead th')];    const rows=[...table.querySelectorAll('.trace-row')];    const odd=rows.find(r=>r.dataset.stripe==='1'), even=rows.find(r=>r.dataset.stripe==='0');    return headings.map(h=>h.textContent.trim()).join(',')==='Sequence,Time (UTC),Kind,Role,State,Summary' && headings.every(h=>h.scope==='col') && rows.every(r=>r.cells.length===6 && r.cells[5].querySelector('details.record')) && odd && even && getComputedStyle(odd).backgroundColor!==getComputedStyle(even).backgroundColor && [...table.querySelectorAll('.trace-turn')].every(r=>!r.hasAttribute('data-stripe') && r.cells[0].colSpan===6) && table.textContent.includes('Initialization');   })()")) throw new Error('Trace table headings, stripes, or turn groups failed');
  await evaluate('document.querySelector(".record summary").focus()');
  await call('Input.dispatchKeyEvent', {type:'keyDown', key:'Enter', code:'Enter', text:'\r', windowsVirtualKeyCode:13});
  await call('Input.dispatchKeyEvent', {type:'keyUp', key:'Enter', code:'Enter', windowsVirtualKeyCode:13});
  if (!await evaluate('document.querySelector(".record").open')) throw new Error('Trace keyboard disclosure failed');
  await call('Emulation.setDeviceMetricsOverride', {width:390,height:844,deviceScaleFactor:1,mobile:false});
  if (!await evaluate("(() => { const c=document.querySelector('.trace-table').parentElement; return getComputedStyle(c).overflowX==='auto' && c.scrollWidth>c.clientWidth && document.documentElement.scrollWidth<=390; })()")) throw new Error('Trace narrow-screen overflow failed');
  await call('Emulation.clearDeviceMetricsOverride');
  await evaluate('const f=document.querySelector("#trace-filter");f.value="browser followup";f.form.requestSubmit()');
  await wait('location.search.includes("filter=browser") && document.querySelectorAll(".record").length>0', 'trace filter');
  if (!await evaluate('[...document.querySelectorAll(".record")].every(r=>r.innerText.includes("browser followup") || r.textContent.includes("browser followup"))')) throw new Error('trace filter mismatch');
  await call('Page.navigate', {url: runURL});
  await wait('document.querySelector("#send-text") && document.querySelector("#connection-status").dataset.state==="connected"', 'unfiltered run');
  await navigateView('runs');
  if (!await evaluate('document.querySelector("[data-slot=table-head]").scope==="col" && !document.querySelector("#send-text") && !document.querySelector(".record")')) throw new Error('Runs is not separate');
  await evaluate('window.confirm=()=>true');
  await submit('clear');
  await wait('document.querySelector("#action-status").textContent==="Action completed."', 'clear action');
  await navigateView('chat');
  await wait('document.querySelector("#live-panel").innerText.includes("No messages in the current branch.")', 'clear context');
  await navigateView('trace');
  if (!await evaluate('document.querySelectorAll(".record").length>0 && document.querySelector("#live-panel").textContent.includes("browser first message")')) throw new Error('clear lost journal history');
  await navigateView('chat');
  await submit('send', 'after clear');
  await wait('document.querySelector(".markdown")?.textContent.includes("Fresh context.")', 'send after clear');
  await navigateView('runs');
  await evaluate('window.confirm=()=>true');
  await submit('reset');
  await wait('document.querySelector("#live-panel").innerText.includes("retired")', 'reset');
  await navigateView('trace');
  if (!await evaluate('document.querySelector("#live-panel").textContent.includes("browser first message")')) throw new Error('reset lost history');
  await evaluate('document.querySelector("#new-chat").click()');
  await wait('!!document.querySelector("#start-text") && document.querySelector("#connection-status").dataset.state==="connected"', 'new chat');
  await submit('start', 'please publish');
  await wait('!!document.querySelector(".waiting select[name=approval]")', 'real Kit approval halt');
  if (!await evaluate("(() => { const s=document.querySelector('#respond-approval'); return s.value==='reject' && s.required && s.labels[0].textContent.trim()==='Decision' && getComputedStyle(s).appearance==='none' && getComputedStyle(document.querySelector('.cn-native-select-icon')).pointerEvents==='none'; })()")) throw new Error('Native select defaults, label, or styles failed');
  if (!await evaluate('document.querySelectorAll(".tool-activity").length>=2 && [...document.querySelectorAll(".tool-activity")].every(d=>!d.open) && document.querySelector(".tool-activity summary").textContent.includes("request_approval") && document.querySelector(".tool-activity summary").textContent.includes("publish the report")')) throw new Error('Tool summaries failed');
  await evaluate('document.querySelector(".tool-activity summary").click()');
  const toolID=await evaluate('document.querySelector(".tool-activity").id');
  await evaluate('document.querySelector(".waiting select").value="approve";document.querySelector(".waiting [name=text]").value="browser approval note"');
  await submit('respond');
  await wait('document.querySelector("#live-panel").innerText.includes("Approved report.") && !document.querySelector(".waiting")', 'approval resume');
  if (!await evaluate('document.getElementById('+JSON.stringify(toolID)+')?.open')) throw new Error('Tool expansion lost on resume');
  await submit('send', 'stop browser turn');
  await wait('!!document.querySelector("#stop-form")', 'active turn Stop');
  if (!await evaluate('document.querySelector("#send-form button").disabled && document.querySelector(".composer-state").textContent.includes("running")')) throw new Error('Active controls failed');
  await submit('cancel');
  await wait('document.querySelector(".composer-state").textContent.includes("cancelled") && !document.querySelector("#stop-form") && !document.querySelector("#send-form button").disabled', 'Stop through channel and SSE');
  if (streams < 3) throw new Error('No real SSE responses observed');
  if (external.length) throw new Error('External asset requests: ' + external.join('\n'));
  if (errors.length) throw new Error('Browser errors: ' + errors.join('\n'));
  console.log('PASS: new run, Markdown, followup, live SSE, drafts, disclosures, trace filter, clear, reset, approval, tool details, Ctrl+Enter, Stop; SSE responses=' + streams);
 } finally { ws.close(); }
})().catch(error => { console.error(error); process.exit(1); });
`
