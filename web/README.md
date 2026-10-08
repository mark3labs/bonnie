# Web chat interface

`web.New(runner, journal, httpChannel)` returns an optional `http.Handler`.
Mount it at `/web` and `/web/`. Mount the HTTP channel at its normal API paths.
The host owns the runner and journal. The UI does not close either resource.

The default `/web/` page is chat, not a management console. The chat uses a compact, terminal-style layout, up to 1100px wide. The transcript
scrolls above a full-width composer. Chat text is monospaced. User messages have
a subtle surface and a blue left rule. Assistant Markdown has no surrounding box.
The compact session heading shows `#` and the run ID, or `New chat`. The current
snapshot API has no title metadata, so the UI does not infer a title. It does not
show model, token, or cost estimates. Send the first message
to create a run through the existing start action. No title is required. In an
open chat, the same composer sends a followup. **New chat** opens an empty chat.

The navigation keeps the selected run:

- **Chat**: `/web/?view=chat&run=ID` (or `/web/?run=ID`).
- **Runs**: `/web/?view=runs&run=ID`. Select a run to see its controls and links
  to chat and trace. Cancel, compact, clear, and reset are only in this view.
- **Trace**: `/web/?view=trace&run=ID`. Inspect the append-only journal separately
  from chat. The filter and live-update URL retain the trace view. Without a run,
  this view links to run selection. Unknown view names return HTTP 400.

The interface provides:

- New conversations and messages on an existing run.
- Answers to questions, and explicit approval or rejection.
- Cancel, compact, clear, and reset actions.
- Current-branch conversation content, with safe Markdown for assistant messages
  and closed disclosures for full message JSON. User messages remain plain text.
- Compact tool activity rows from Kit message parts. Calls show the tool name and
  a command, path, query, action, or URL when present. Other arguments stay in
  details. Results use the call name from the selected branch and a bounded text
  summary. Unknown names show **Tool result**. Expand a row for arguments or the
  result payload. These are durable records, not live tool progress.
- A composer footer with the real run state. **Send** is disabled while a run is
  pending, running, waiting for input, or retired. **Stop** cancels a pending or
  running turn through the existing channel action, with CSRF and turn ID fields.
  **Ctrl+Enter** sends a valid composer form; plain Enter inserts a newline.
  A draft remains editable while a turn runs.
- An append-only journal trace with sequence, UTC time, role, kind, state,
  entry IDs, formatted payloads, turn groups, and a text filter. Compact summaries
  show text previews and tool names from message payloads, not replay changes.
  The table has six columns: sequence, time, kind, role, state, and summary.
  Data rows use sequence parity for stable zebra backgrounds. Full-width headers
  separate turns; **Initialization** labels records before the first turn.
  Expand a summary to read the full text, payload, and entry and parent IDs.
  The table scrolls horizontally on small screens.

All actions use the HTTP channel handler. All reads, streams, and assets use
`Channel.Protect`, with the channel's configured authenticator. This is an
operator interface: authentication does not add per-run authorization. An
accepted operator can read all non-reserved runs. Use a cookie authenticator
or an authenticated reverse proxy for browser access. There is no separate
login, and no bearer token is saved in browser storage. Without a configured
authenticator, the UI is open, just like the HTTP channel. Keep that mode on
loopback only.

Actions require a CSRF cookie and matching form token. Go's same-origin
protection rejects cross-origin browser actions. The cookie is HttpOnly,
SameSite Strict, and Secure on TLS requests. Terminate TLS at the application
or have the proxy protect its connection to the application. No forwarded
host header is trusted by the CSRF check. The CSP permits `unsafe-eval` for
Datastar expressions, but not inline scripts. Goldmark renders assistant
Markdown with its safe defaults: raw HTML is omitted and dangerous link schemes
are blocked. Do not enable its unsafe renderer option. Other model and journal
text is escaped by templ; no model HTML is trusted.

## Colors and palette source

The web palette follows `cmd/bonnie/tui/styles.go` and
`cmd/bonnie/tui/markdown.go`, not the CLI help theme. The TUI uses fixed
xterm-256 colors. It has no light/dark adaptive palette. Body paragraphs inherit
the terminal color, and faint styles depend on the terminal. The web interface
uses explicit colors and `prefers-color-scheme` instead. It does not import
terminal packages.

The CLI and compiled-agent help use Fang's default adaptive theme
(`github.com/charmbracelet/fang` v1.0.0, `theme.go`, `DefaultColorScheme`).
That separate theme uses Charmtone colors, including Charple for titles and
Pony/Cheeky for commands. No BONNIE help-theme override supplies a shared palette.

`assets/web.css` defines semantic variables. Use these variables for new color
rules; do not add hex values outside the two palette blocks.

| Use | TUI source | Dark web | Light web |
| --- | --- | --- | --- |
| Brand, primary, focus | 205, header/spinner/cursor | `#ff5faf` | `#a82065` |
| User label | 39 | `#00afff` | `#006a9e` |
| Links and tool names | 81 | `#5fd7ff` | `#006580` |
| Connected | 42 | `#00d787` | `#007548` |
| Question and paused status | 227 | `#ffff5f` | `#746500` |
| Error | 196 (`#ff0000`) | `#ff8787` | `#c40000` |
| Page background | Markdown Base, 236 | `#303030` | `#f8f7f9` |
| Card and even trace row | Markdown Surface, 235 | `#262626` | `#ffffff` |
| Body text | Markdown Text, 255 | `#eeeeee` | `#262626` |
| Muted text | Contrast-adjusted gray | `#a8a8a8` | `#626262` |
| Borders / control borders | Web contrast adjustment | `#505050` / `#858585` | `#dedade` / `#898389` |
| Odd trace row | Web subtle stripe | `#302b30` | `#f4f0f4` |
| Trace turn header | Web pink-tinted surface | `#352935` | `#e9e1e9` |

Primary buttons use `#262626` text in dark mode and `#ffffff` in light mode.
Hover colors are `#ff87c3` and `#8d1954`, respectively. Secondary badges use
`#302b30` / `#b2b2b2` in dark mode and `#eee9ee` / `#626262` in light mode.
Row hover backgrounds are `#342e34` and `#ece5ec`. Light accents, muted text,
and error colors differ from the terminal values to retain contrast.

`TestPaletteSource` reads the TUI source and checks ANSI-to-hex values without
terminal imports. A source change requires a palette review. `TestPaletteContrast`
checks WCAG AA text contrast (4.5:1), focus and control borders (3:1), and a
small, nonzero stripe luminance difference in both modes. Chromium E2E checks
the applied brand, button, focus, page, stripe, and turn-header colors in both
modes. Sequence parity keeps trace stripes stable during filtering and updates.
Chat layout changes use these same palette variables; the palette values do not change.

## Live updates

The local Datastar module opens `/web/live`. The adapter polls durable data
once per second, sends a patch only when the rendered snapshot changes, and
sends a keepalive otherwise. It stops on request cancellation or a write
error. No event subscription or background goroutine is retained. It does
not show live reasoning or token deltas. This simple adapter reads full
snapshots and is intended for a small operator console, not a large run index.

The conversation uses the HTTP snapshot API, so clear, branch selection, and
torn-step repair use the same rules as other clients. Journal trace records
remain visible after clear or reset. Initialization contains records before the
first turn marker.

The header shows connection status from Datastar fetch events. A patch confirms
that durable updates are connected; errors and retries show that updates are
paused. The status is not a model activity indicator.

Live patches keep drafts and open disclosures. Datastar v1.0.4 does not preserve
`open` by default. Disclosures use stable IDs and `data-preserve-attr="open"`.
Editable controls use `data-ignore-morph`, while hidden turn fields still update.
Response forms have turn-specific IDs so an old answer does not move to a new
question. Message disclosure IDs include a content hash so branch changes do not
reuse the old disclosure. A successful send clears only the submitted draft, not
text entered while the request was in progress.

## Templates and assets

Regenerate committed template code with:

```sh
go generate ./web
```

The generator is pinned to templ v0.3.1070. The UI calls the prebuilt APIs in
`github.com/axadrn/shadcn-templ/v2` v2.0.0-beta.13:

- `card.Card`, `Header`, `Title`, `Description`, and `Content` for panels.
  Native sections and h1/h2 headings retain the document structure.
- `input.Input`, `textarea.Textarea`, and `label.Label` for editable fields.
  Explicit, stable IDs link labels to controls and prevent random IDs in patches.
- `nativeselect.NativeSelect` and `Option` for approvals and choice answers.
  These render actual select and option elements, with native keyboard controls,
  validation, and form values. Reject remains the first approval option.
- `table.Table`, `Header`, `Body`, `Row`, `Head`, and `Cell` for the run index and journal trace.
  Column headings use `scope="col"`; run links remain in the Runs navigation.
- `separator.Separator` above run controls in the Runs view.
- `button.Button` and `badge.Badge` for actions and run states.

`assets/web.css` styles the generated `cn-*` classes and separator data slot,
including the native-select wrapper and its SVG icon. It supplies local light,
dark, focus, disabled, and narrow-screen styles. It needs no Tailwind build,
CDN, or Node runtime. The chosen components need no library JavaScript. The
native-select icon is rendered by the library as inline SVG, not fetched.

Message and trace disclosures still use native `details` and `summary`.
The library's accordion and collapsible controls need JavaScript and keep state
in several attributes, rather than the native `open` attribute preserved by
Datastar. Keeping native disclosures retains keyboard access, independent open
records, stable selectors, and patch preservation without a new state adapter.
Hidden CSRF, run, and turn fields remain native inputs; they need no component
style or generated ID. Trace disclosures stay inside the summary cell, so
expanded payloads need no row-toggle script. Turn separators are not striped.

Rendering is serialized because that release's shared class-merger cache is not
concurrency-safe.

`assets/datastar.js` is the actual Datastar v1.0.4 release bundle from:

https://raw.githubusercontent.com/starfederation/datastar/v1.0.4/bundles/datastar.js

Its MIT license is in `assets/DATASTAR-LICENSE.txt`. All runtime assets are
embedded. The browser makes no CDN request. Handler tests use memory journals
and scripted agents. Browser tests also use a real SQLite journal, Runner, and
Kit with `internal/fakemodel`. No test needs a live model key or an asset service.

## Browser tests and offline demo

Run the full browser flow with:

```sh
go test -race -run '^TestBrowserEndToEnd$' -count=3 -v ./web
```

The test uses Chromium and Node's built-in WebSocket to control the browser
through CDP. It needs no npm packages. It skips when Chromium or a Node version
with WebSocket support is missing. A separate `TestBrowserLiveState` test checks
the shipped Datastar morph rules.

Rendered-component tests check labels, stable IDs, required fields, native
options, turn IDs, escaping, and morph attributes for approval, choice, and
free-text forms. The live-state browser test also preserves an approval
selection and note across a patch.

The full flow checks first-message run creation, separate view navigation, terminal chat structure, narrow-screen layout, Markdown, Ctrl+Enter, followup context, real SSE
patches without page navigation, draft and disclosure preservation, trace
filtering, clear, reset, approval with resume, tool summaries and expansion across
resume, and Stop on a running turn. It checks native validation,
label associations, table headings, local component styles, 
select defaults, and the absence of external asset requests. It also checks the requests
that Kit sends to the scripted model. It does not test compact, approval
rejection, question forms, stream reconnection, authentication deployment, or
other browser engines. SSE shows durable records, not transient token deltas.

Start an offline manual server with a durable journal:

```sh
BONNIE_WEB_DEMO_ADDR=127.0.0.1:8090 \
BONNIE_WEB_DEMO_JOURNAL="$PWD/.bonnie/web-demo" \
nohup go test -timeout=0 -run '^TestWebDemo$' -v ./web \
  >/tmp/bonnie-web-demo.log 2>&1 </dev/null &
```

Open `http://127.0.0.1:8090/web/`. Both environment variables are required.
The test accepts only a loopback IP address. It serves the actual web UI and
HTTP channel, with SQLite and a hermetic Kit. It blocks until the process is
stopped. It has 10000 helpful Markdown replies per process; they are scripted,
not answers based on your question. Restarting the process keeps the journal
and resets the reply script. The demo does not request approval or run tools.
It needs no provider credentials. Normal tests skip this server.
