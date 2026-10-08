package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/runtime"
)

// Prune resolves the journal-local runtime without downloads. A stale receipt
// can be rechecked, but failed or uncertain deletion must remain retryable.
func TestSandboxPruneMicrosandboxRecheck(t *testing.T) {
	rm, err := exec.LookPath("rm")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	bin := filepath.Join(root, "microsandbox", "bin", "msb")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
 inspect)
  if [ -f "$0.present" ]; then echo '{"name":"bonnie-run"}'; exit 0; fi
  echo "error: sandbox not found: $4" >&2; exit 1;;
 rm)
  if [ -f "$0.fail" ]; then echo 'error: database locked' >&2; exit 1; fi
  if [ -f "$0.noop" ]; then exit 0; fi
  /bin/rm "$0.present";;
 *) exit 9;;
esac
`
	script = strings.ReplaceAll(script, "/bin/rm", "\""+rm+"\"")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bin + ".present", bin + ".fail"} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j, err := runtime.OpenSQLiteJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	ctx := context.Background()
	if err := j.Checkpoint(ctx, "run", runtime.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordSandboxDeleted}); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		recs, err := j.Replay(ctx, "run")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, rec := range recs {
			if rec.Kind == runtime.RecordSandboxDeleted {
				n++
			}
		}
		return n
	}
	args := []string{"--journal", root, "--sandbox", "microsandbox"}
	out := capture(t, func() error { return execute(newSandboxPruneCmd(), append(args, "--dry-run")...) })
	if !strings.Contains(out, "cleanup already recorded") || strings.Contains(out, "would delete") {
		t.Fatalf("dry run: %s", out)
	}
	out = capture(t, func() error { return execute(newSandboxPruneCmd(), append(args, "--dry-run", "--recheck")...) })
	if !strings.Contains(out, "would delete sandbox") || count() != 1 {
		t.Fatalf("recheck dry run: %s", out)
	}
	for _, mode := range []string{"fail", "noop"} {
		if mode == "noop" {
			if err := os.Remove(bin + ".fail"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin+".noop", nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// Execute directly: capture treats an expected command error as a failure.
		if err := execute(newSandboxPruneCmd(), append(args, "--recheck")...); err == nil {
			t.Fatalf("%s cleanup succeeded", mode)
		}
		if count() != 1 {
			t.Fatal("failed cleanup wrote a receipt")
		}
	}
	if err := os.Remove(bin + ".noop"); err != nil {
		t.Fatal(err)
	}
	out = capture(t, func() error { return execute(newSandboxPruneCmd(), append(args, "--recheck")...) })
	if !strings.Contains(out, "deleted sandbox") || count() != 2 {
		t.Fatalf("retry: %s", out)
	}
	if _, err := os.Stat(bin + ".present"); !os.IsNotExist(err) {
		t.Fatalf("sandbox survived: %v", err)
	}
}
