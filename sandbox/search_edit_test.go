package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func runSandboxTool(t *testing.T, ctx context.Context, tool kit.Tool, input any, wantError bool) string {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Run(ctx, kit.LLMToolCall{Input: string(data)})
	if err != nil || result.IsError != wantError {
		t.Fatalf("tool(%s) = %+v, %v; wantError %v", data, result, err, wantError)
	}
	if len(result.Content) > maxToolOutput || !utf8.ValidString(result.Content) {
		t.Fatalf("invalid output size/UTF-8: %d", len(result.Content))
	}
	if !wantError && !json.Valid([]byte(result.Content)) {
		t.Fatalf("invalid JSON: %s", result.Content)
	}
	return result.Content
}

func decodeSearch(t *testing.T, content string) searchResponse {
	t.Helper()
	var r searchResponse
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSearchGlob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "a.go", true}, {"*.go", "dir/a.go", false}, {"**/*.go", "a.go", true}, {"**/*.go", "x/y/a.go", true},
		{"a/**/b?.[ch]", "a/b1.c", true}, {"a/**/b?.[ch]", "a/x/y/b1.h", true}, {"a/**/b?.[ch]", "a/x/b11.c", false},
		{"**", "x/y", true}, {"a/*/c", "a/b/d/c", false},
	} {
		g, err := compileGlob(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := g.matches(tc.name); got != tc.want {
			t.Errorf("%q matches %q = %v", tc.pattern, tc.name, got)
		}
	}
	for _, bad := range []string{"", " ", "/a", "a/../b", "./a", "a//b", "[", "a/"} {
		if _, err := compileGlob(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSearchAndEditBackends(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		ctx := testCtx(t)
		sb := openSandbox(t, p, "search-edit")
		open := func(context.Context) (Sandbox, error) { return sb, nil }
		files := []string{"a.go", "sub dir/nested.go", "-dash/quote'$.go", "dir; touch injected/new\nline.go", ".hidden.go", ".git/ignored.go"}
		for _, name := range files {
			if err := sb.WriteFile(ctx, name, []byte("α\r\nmatch.*\r\nlast\r")); err != nil {
				t.Fatal(err)
			}
		}
		for _, root := range []string{"", "/workspace"} {
			r := decodeSearch(t, runSandboxTool(t, ctx, findTool(open), map[string]any{"pattern": "**/*.go", "path": root}, false))
			if len(r.Results) != 5 || r.Truncated {
				t.Fatalf("find: %+v", r)
			}
			got := []string{}
			for _, v := range r.Results {
				got = append(got, v.(map[string]any)["path"].(string))
			}
			want := []string{"-dash/quote'$.go", ".hidden.go", "a.go", "dir; touch injected/new\nline.go", "sub dir/nested.go"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("order = %q, want %q", got, want)
			}
		}
		for _, root := range []string{"a.go", "/workspace/a.go"} {
			var r struct {
				Results []grepMatch `json:"results"`
			}
			output := runSandboxTool(t, ctx, grepTool(open), map[string]any{"pattern": "match.*", "literal": true, "path": root, "context": 10}, false)
			if err := json.Unmarshal([]byte(output), &r); err != nil {
				t.Fatal(err)
			}
			if len(r.Results) != 1 || r.Results[0].Line != 2 || r.Results[0].Text != "match.*" || r.Results[0].Before[0].Text != "α" || r.Results[0].After[0].Text != "last\r" {
				t.Fatalf("grep: %+v", r)
			}
		}
		runSandboxTool(t, ctx, editTool(open, make(mutationGate, 1)), map[string]any{"path": "a.go", "replacements": []map[string]string{{"old_text": "α", "new_text": "β"}, {"old_text": "match.*", "new_text": ""}}}, false)
		data, err := sb.ReadFile(ctx, "a.go")
		if err != nil || string(data) != "β\r\n\r\nlast\r" {
			t.Fatalf("edit: %q %v", data, err)
		}
		runSandboxTool(t, ctx, findTool(open), map[string]any{"pattern": "*", "path": "absent"}, true)
		runSandboxTool(t, ctx, grepTool(open), map[string]any{"pattern": "x", "path": "absent"}, true)
		if _, err := sb.ReadFile(ctx, "injected"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("injection side effect: %v", err)
		}
	})
}

func TestSearchValidationBeforeOpen(t *testing.T) {
	t.Parallel()
	open := func(context.Context) (Sandbox, error) {
		t.Error("opened for invalid input")
		return nil, errors.New("unavailable")
	}
	for _, in := range []map[string]any{{"pattern": "["}, {"pattern": ""}, {"pattern": "x", "context": 11}, {"pattern": "x", "limit": -1}, {"pattern": "x", "glob": "../*"}} {
		runSandboxTool(t, context.Background(), grepTool(open), in, true)
	}
	for _, in := range []map[string]any{{"pattern": "["}, {"pattern": "/a"}, {"pattern": "x", "limit": 1001}} {
		runSandboxTool(t, context.Background(), findTool(open), in, true)
	}
	for _, in := range []map[string]any{{"path": "a", "old_text": "x"}, {"path": "a", "old_text": "", "new_text": ""}, {"path": "a", "old_text": "x", "new_text": "y", "replacements": []any{}}, {"path": "a", "replacements": []map[string]string{{"old_text": "x"}}}, {"path": "a", "replacements": []any{}}} {
		runSandboxTool(t, context.Background(), editTool(open, make(mutationGate, 1)), in, true)
	}
}

type contentSandbox struct {
	*stubSandbox
	data              []byte
	writes            int
	readErr, writeErr error
}

func (s *contentSandbox) ReadFile(context.Context, string) ([]byte, error) { return s.data, s.readErr }
func (s *contentSandbox) WriteFile(_ context.Context, _ string, data []byte) error {
	s.writes++
	s.data = append([]byte(nil), data...)
	return s.writeErr
}

func TestExactEditValidationAndWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data, input, want, errorText string }{
		{"single", "abc", `{"old_text":"b","new_text":"B"}`, "aBc", ""},
		{"adjacent", "abc", `{"replacements":[{"old_text":"a","new_text":"x"},{"old_text":"b","new_text":"y"}]}`, "xyc", ""},
		{"original", "abc", `{"replacements":[{"old_text":"a","new_text":"b"},{"old_text":"b","new_text":"c"}]}`, "bcc", ""},
		{"unicode CRLF", "α\r\nxyz", `{"old_text":"α","new_text":"β"}`, "β\r\nxyz", ""},
		{"delete", "abc", `{"old_text":"b","new_text":""}`, "ac", ""},
		{"missing", "abc", `{"old_text":"z","new_text":""}`, "", "replacement 1: old_text is missing"},
		{"overlapping occurrences", "aaa", `{"old_text":"aa","new_text":""}`, "", "ambiguous"},
		{"overlapping regions", "abc", `{"replacements":[{"old_text":"ab","new_text":""},{"old_text":"bc","new_text":""}]}`, "", "overlaps"},
		{"duplicate", "abc", `{"replacements":[{"old_text":"b","new_text":""},{"old_text":"b","new_text":""}]}`, "", "overlaps"},
		{"insert not searched", "abc", `{"replacements":[{"old_text":"a","new_text":"z"},{"old_text":"z","new_text":""}]}`, "", "replacement 2: old_text is missing"},
		{"binary", "a\x00b", `{"old_text":"a","new_text":"b"}`, "", "UTF-8"},
		{"invalid utf8", "a\xffb", `{"old_text":"a","new_text":"b"}`, "", "UTF-8"},
		{"oversized", strings.Repeat("a", searchFileLimit+1), `{"old_text":"a","new_text":"b"}`, "", "1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := &contentSandbox{stubSandbox: &stubSandbox{}, data: []byte(tc.data)}
			var in map[string]any
			if err := json.Unmarshal([]byte(tc.input), &in); err != nil {
				t.Fatal(err)
			}
			in["path"] = "a"
			output := runSandboxTool(t, context.Background(), editTool(func(context.Context) (Sandbox, error) { return sb, nil }, make(mutationGate, 1)), in, tc.errorText != "")
			if tc.errorText != "" {
				if sb.writes != 0 || !strings.Contains(output, tc.errorText) {
					t.Fatalf("writes %d, output %s", sb.writes, output)
				}
			} else if sb.writes != 1 || string(sb.data) != tc.want {
				t.Fatalf("writes %d, content %q", sb.writes, sb.data)
			}
		})
	}
	for _, fail := range []string{"read", "write", "open"} {
		sb := &contentSandbox{stubSandbox: &stubSandbox{}, data: []byte("a")}
		if fail == "read" {
			sb.readErr = ErrNotFound
		}
		if fail == "write" {
			sb.writeErr = errors.New("write failed")
		}
		open := func(context.Context) (Sandbox, error) {
			if fail == "open" {
				return nil, errors.New("unavailable")
			}
			return sb, nil
		}
		runSandboxTool(t, context.Background(), editTool(open, make(mutationGate, 1)), map[string]string{"path": "a", "old_text": "a", "new_text": "b"}, true)
		if fail != "write" && sb.writes != 0 {
			t.Fatal("unexpected write")
		}
	}
}

func TestGrepBoundsAndLines(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	sb := openSandbox(t, Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()), "grep-bounds")
	open := func(context.Context) (Sandbox, error) { return sb, nil }
	for _, tc := range []struct {
		name, content, pattern string
		literal                bool
		count                  int
		partial                bool
	}{
		{"no newline", "α hit", "hit", false, 1, false},
		{"literal", "a.b\naxb\n", "a.b", true, 1, false},
		{"regex", "a.b\naxb\n", "a.b", false, 2, false},
		{"binary", "hit\x00hit", "hit", false, 0, false},
		{"empty", "", "hit", false, 0, false},
		{"file limit", strings.Repeat("a", searchFileLimit+1), "a", false, 0, true},
		{"long line", strings.Repeat("界", 30000), "界", false, 0, true},
		{"count limit", "hit\nhit\nhit", "hit", false, 2, true},
	} {
		if err := sb.WriteFile(ctx, "input", []byte(tc.content)); err != nil {
			t.Fatal(err)
		}
		r := decodeSearch(t, runSandboxTool(t, ctx, grepTool(open), map[string]any{"path": "input", "pattern": tc.pattern, "literal": tc.literal, "limit": 2}, false))
		if len(r.Results) != tc.count || r.Truncated != tc.partial {
			t.Fatalf("%s: %+v", tc.name, r)
		}
	}
	// A line longer than Scanner's default still matches if its result is small.
	if err := sb.WriteFile(ctx, "input", []byte(strings.Repeat("x", 70000)+"\nhit")); err != nil {
		t.Fatal(err)
	}
	r := decodeSearch(t, runSandboxTool(t, ctx, grepTool(open), map[string]any{"path": "input", "pattern": "hit"}, false))
	if len(r.Results) != 1 {
		t.Fatalf("long nonmatching line: %+v", r)
	}
}

func TestDirectoryFallbackAndWrappers(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	for _, wrap := range []string{"plain", "env", "seed"} {
		p := Provider(Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()))
		if wrap == "env" {
			p = EnvInjected(p, map[string]string{"ENUM_TEST": "yes"})
		}
		if wrap == "seed" {
			p = Seeded(p, t.TempDir())
		}
		sb := openSandbox(t, p, "directory")
		for _, name := range []string{"a", ".hidden", "dir space/x", "quote'\n$/x"} {
			if err := sb.WriteFile(ctx, name, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		for _, target := range []Sandbox{sb, &requiredOnlySandbox{sb}} {
			entries, partial, err := readDirectory(ctx, target, "", 100)
			if err != nil || partial || len(entries) != 4 {
				t.Fatalf("%s: entries %+v partial %v err %v", wrap, entries, partial, err)
			}
			_, partial, err = readDirectory(ctx, target, "", 1)
			if err != nil || !partial {
				t.Fatalf("bound: %v %v", partial, err)
			}
			_, _, err = readDirectory(ctx, target, "missing/child", 100)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing: %v", err)
			}
			_, _, err = readDirectory(ctx, target, "a", 100)
			if !errors.Is(err, ErrNotDirectory) {
				t.Fatalf("file: %v", err)
			}
		}
		if _, err := sb.ReadFile(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("enumeration created missing root: %v", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := readDirectory(canceled, sb, "", 100); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if err := sb.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDirectory(ctx, sb, "", 100); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed: %v", err)
		}
	}
}

type requiredOnlySandbox struct{ Sandbox }

type directoryProtocolSandbox struct {
	*stubSandbox
	result  *Result
	err     error
	command Command
}

func (s *directoryProtocolSandbox) Exec(_ context.Context, c Command) (*Result, error) {
	s.command = c
	return s.result, s.err
}

func TestDirectoryProtocol(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		out  string
		code int
		bad  bool
	}{
		{"a\x00file\x00.hidden\x00directory\x00", 0, false}, {"truncated\x00", 0, false}, {"", 0, false},
		{"../bad\x00file\x00", 0, true}, {"a\x00unknown\x00", 0, true}, {"a\x00file", 0, true}, {"a\x00", 0, true},
		{"a\x00file\x00a\x00file\x00", 0, true}, {"", 46, true}, {"", 44, true}, {"", 45, true},
	} {
		sb := &directoryProtocolSandbox{stubSandbox: &stubSandbox{}, result: &Result{Stdout: tc.out, ExitCode: tc.code}}
		_, _, err := readDirectory(context.Background(), sb, "a'; touch injected/child", 100)
		if (err != nil) != tc.bad {
			t.Fatalf("protocol %q code %d: %v", tc.out, tc.code, err)
		}
		if sb.command.Dir != WorkDir || sb.command.Args[4] != "./a'; touch injected/child" || sb.command.Args[2] != directoryScript {
			t.Fatalf("unsafe command: %+v", sb.command)
		}
	}
}

func TestSearchSymlinksAndBackendPolicy(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"local", "landlock"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			p := Provider(Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()))
			if backend == "landlock" {
				p = landlockProvider(t)
			}
			sb := openSandbox(t, p, "links")
			ctx := testCtx(t)
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			host := p.(WorkingDirReporter).WorkingDir("links")
			for name, target := range map[string]string{"loop": ".", "outside": outside, "dangling": "absent"} {
				if err := os.Symlink(target, filepath.Join(host, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := sb.WriteFile(ctx, "visible", []byte("inside")); err != nil {
				t.Fatal(err)
			}
			open := func(context.Context) (Sandbox, error) { return sb, nil }
			r := decodeSearch(t, runSandboxTool(t, ctx, findTool(open), map[string]string{"pattern": "**"}, false))
			if len(r.Results) != 1 {
				t.Fatalf("followed links: %+v", r)
			}
			runSandboxTool(t, ctx, grepTool(open), map[string]string{"pattern": "outside", "path": filepath.Join(outside, "secret")}, backend == "landlock")
			runSandboxTool(t, ctx, grepTool(open), map[string]string{"pattern": "outside", "path": "outside/secret"}, backend == "landlock")
		})
	}
}

type budgetSandbox struct {
	*stubSandbox
	canceled bool
}

func (s *budgetSandbox) ReadDirectory(ctx context.Context, _ string, limit int) ([]DirectoryEntry, bool, error) {
	if s.canceled {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	entries := make([]DirectoryEntry, limit)
	for i := range entries {
		entries[i] = DirectoryEntry{fmt.Sprintf("%05d", i), "file"}
	}
	return entries, true, nil
}
func (s *budgetSandbox) ReadFile(context.Context, string) ([]byte, error) {
	return []byte("no match"), nil
}
func TestSearchVisitBudgetAndDeadline(t *testing.T) {
	t.Parallel()
	sb := &budgetSandbox{stubSandbox: &stubSandbox{}}
	open := func(context.Context) (Sandbox, error) { return sb, nil }
	r := decodeSearch(t, runSandboxTool(t, context.Background(), grepTool(open), map[string]string{"pattern": "absent"}, false))
	if !r.Truncated || !strings.Contains(r.Reason, "visit") {
		t.Fatalf("visit budget: %+v", r)
	}
	sb.canceled = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	runSandboxTool(t, ctx, findTool(open), map[string]string{"pattern": "*"}, true)
}

type blockingEditSandbox struct {
	*contentSandbox
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *blockingEditSandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	s.once.Do(func() { close(s.entered); <-s.resume })
	return s.contentSandbox.ReadFile(ctx, p)
}
func TestEditWriteGateAndCancellation(t *testing.T) {
	t.Parallel()
	sb := &blockingEditSandbox{contentSandbox: &contentSandbox{stubSandbox: &stubSandbox{}, data: []byte("a")}, entered: make(chan struct{}), resume: make(chan struct{})}
	tools := Tools(func(context.Context) (Sandbox, error) { return sb, nil })
	var edit, write kit.Tool
	for _, tool := range tools {
		switch tool.Info().Name {
		case "edit":
			edit = tool
		case "write_file":
			write = tool
		}
	}
	done := make(chan error, 1)
	go func() {
		result, err := edit.Run(context.Background(), kit.LLMToolCall{Input: `{"path":"x","old_text":"a","new_text":"b"}`})
		if result.IsError && err == nil {
			err = errors.New(result.Content)
		}
		done <- err
	}()
	<-sb.entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result, err := write.Run(ctx, kit.LLMToolCall{Input: `{"path":"x","content":"c"}`})
	if err != nil || !result.IsError {
		t.Fatalf("waiting write: %+v %v", result, err)
	}
	close(sb.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(sb.data) != "b" || sb.writes != 1 {
		t.Fatalf("lost update: %q writes %d", sb.data, sb.writes)
	}
}

func hermeticSandboxOptions(model *fakemodel.Model) []kit.Option {
	return []kit.Option{model.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.Quiet = true
	}}
}

func TestDefaultSearchToolsLazyAndDurableApproval(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	p := Local(WithLocalRoot(t.TempDir()))
	model := fakemodel.New(fakemodel.Say("no tools"))
	opts := hermeticSandboxOptions(model)
	opts = append(opts, func(o *kit.Options) {
		if !o.DisableCoreTools {
			t.Error("host core tools enabled")
		}
	})
	factory := Agent(p, opts...)
	journal := runtime.NewMemoryJournal()
	run, err := runtime.NewRunner(journal, factory).Start(ctx, "search-durable", runtime.Input{Text: "hello"})
	if err != nil || run.State != runtime.RunCompleted {
		t.Fatalf("start: %+v %v", run, err)
	}
	req := model.Requests()[0]
	for _, name := range []string{"shell", "shell_status", "shell_wait", "shell_cancel", "read_file", "write_file", "list_files", "grep", "find", "edit"} {
		if !req.HasTool(name) {
			t.Errorf("missing %s", name)
		}
	}
	for _, name := range []string{"read", "write"} {
		if req.HasTool(name) {
			t.Errorf("host core tool %s", name)
		}
	}
	if len(p.opened) != 0 {
		t.Fatal("model-only turn opened sandbox")
	}
	sb, err := p.Open(ctx, "search-durable")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(ctx, "a.txt", []byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	approval := fakemodel.New(fakemodel.Call("find", `{"pattern":"**/*.txt"}`), fakemodel.Call("request_approval", `{"action":"edit a.txt"}`), fakemodel.Call("edit", `{"path":"a.txt","old_text":"before","new_text":"after"}`))
	run, err = runtime.NewRunner(journal, Agent(p, hermeticSandboxOptions(approval)...)).Start(ctx, "search-durable", runtime.Input{Text: "edit with approval"})
	if err != nil || run.State != runtime.RunWaiting || len(approval.Requests()) != 2 {
		t.Fatalf("approval: %+v %v requests %d", run, err, len(approval.Requests()))
	}
	data, err := os.ReadFile(filepath.Join(p.WorkingDir("search-durable"), "a.txt"))
	if err != nil || string(data) != "before" {
		t.Fatalf("edited before approval: %q %v", data, err)
	}
	// A separate Runner and provider retain only the journal and files.
	p2 := Local(WithLocalRoot(p.root))
	resumed := fakemodel.New(fakemodel.Call("edit", `{"path":"a.txt","old_text":"before","new_text":"after"}`), fakemodel.Call("grep", `{"pattern":"after"}`), fakemodel.Say("done"))
	run, err = runtime.NewRunner(journal, Agent(p2, hermeticSandboxOptions(resumed)...)).Resume(ctx, "search-durable", []runtime.InputResponse{runtime.Approve("")})
	if err != nil || run.State != runtime.RunCompleted {
		t.Fatalf("resume: %+v %v", run, err)
	}
	again := fakemodel.New(fakemodel.Say("remembered"))
	_, err = runtime.NewRunner(journal, Agent(Local(WithLocalRoot(p.root)), hermeticSandboxOptions(again)...)).Start(ctx, "search-durable", runtime.Input{Text: "remember"})
	if err != nil {
		t.Fatal(err)
	}
	calls, results := map[string]bool{}, map[string]bool{}
	ids := map[string]string{}
	for _, message := range again.Requests()[0].Messages {
		for _, part := range message.Content {
			switch part := part.(type) {
			case kit.LLMToolCallPart:
				calls[part.ToolName] = true
				ids[part.ToolCallID] = part.ToolName
			case kit.LLMToolResultPart:
				results[ids[part.ToolCallID]] = true
			}
		}
	}
	for _, name := range []string{"find", "edit", "grep"} {
		if !calls[name] || !results[name] {
			t.Errorf("lost typed history for %s: calls %v results %v", name, calls, results)
		}
	}
}

// Exercise the actual CLI marker transport without a Docker/MSB daemon.
func TestCLIDirectoryAdapterAndEnvironment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a", ".hidden", "quote'\n$"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	var commands []Command
	c := &cliSandbox{bin: "sh", backend: "test", killFn: func(context.Context, *cliSandbox) error { return nil }, execArgs: func(_ *cliSandbox, argv []string, cmd Command) []string {
		commands = append(commands, cmd)
		dir := cmd.Dir
		if dir == WorkDir {
			dir = root
		}
		args := []string{"-c", `cd "$1" || exit; shift; exec env "$@"`, "test", dir}
		args = append(args, cmd.Env...)
		return append(args, argv...)
	}}
	// Seed wrapper must not bypass environment injection for a CLI reader.
	sb := newEnvSandbox(newSeededJobSandbox(c), []string{"ENUM_TEST=yes"})
	entries, partial, err := readDirectory(context.Background(), sb, "", 100)
	if err != nil || partial || len(entries) != 4 {
		t.Fatalf("CLI entries %+v partial %v err %v", entries, partial, err)
	}
	if !reflect.DeepEqual(commands[0].Env, []string{"ENUM_TEST=yes"}) || commands[0].Dir != WorkDir {
		t.Fatalf("lost namespace/env: %+v", commands)
	}
	if entries[2].Kind != "symlink" {
		t.Fatalf("dangling link: %+v", entries)
	}
	_, partial, err = readDirectory(context.Background(), sb, "", 1)
	if err != nil || !partial {
		t.Fatalf("CLI bound: %v %v", partial, err)
	}
	_, _, err = readDirectory(context.Background(), sb, "missing/child", 100)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CLI missing: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = readDirectory(context.Background(), sb, "", 100)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("CLI closed: %v", err)
	}
}

func TestDirectoryWrappersDoNotWidenCapabilities(t *testing.T) {
	t.Parallel()
	inner := &stubSandbox{}
	if _, ok := newEnvSandbox(inner, []string{"A=B"}).(DirectoryReader); ok {
		t.Fatal("environment wrapper added directory capability")
	}
	if _, ok := newSeededJobSandbox(inner).(DirectoryReader); ok {
		t.Fatal("seed wrapper added directory capability")
	}
	native := &budgetSandbox{stubSandbox: inner}
	for _, wrapped := range []Sandbox{newEnvSandbox(native, []string{"A=B"}), newSeededJobSandbox(native)} {
		if _, ok := wrapped.(DirectoryReader); !ok {
			t.Fatal("lost directory capability")
		}
		if _, ok := wrapped.(JobSandbox); ok {
			t.Fatal("added jobs")
		}
		if _, ok := wrapped.(Deleter); ok {
			t.Fatal("added delete")
		}
		entries, _, err := readDirectory(context.Background(), wrapped, "", 1)
		if err != nil || len(entries) != 1 {
			t.Fatalf("native forwarding: %+v %v", entries, err)
		}
	}
}
