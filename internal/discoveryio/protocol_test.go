//go:build linux

package discoveryio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGuestProtocolBoundedAndNoFollow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("saved bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args      []string
		wantError bool
	}{
		{[]string{"1", dir, "read", "file", "20"}, false},
		{[]string{"1", dir, "read", "file", "3"}, true},
		{[]string{"1", dir, "read", "link", "20"}, true},
		{[]string{"1", dir, "read", "../file", "20"}, true},
		{[]string{"2", dir, "list", ".", "10"}, true},
		{[]string{"1", dir, "execute", "file", "20"}, true},
	} {
		var out bytes.Buffer
		if err := Serve(tc.args, &out); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if (response.Error != "") != tc.wantError {
			t.Fatalf("args=%v response=%+v", tc.args, response)
		}
	}
}
