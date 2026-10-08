package scripts_test

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The development shell must use the installer's runtime release. An older
// binary can refuse a database that the installed runtime has already migrated.
func TestMicrosandboxPinsMatch(t *testing.T) {
	t.Parallel()

	installer, err := os.ReadFile("../sandbox/microsandbox_install.go")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := os.ReadFile("../nix/microsandbox.nix")
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`const MicrosandboxVersion = "v([^"]+)"`).FindSubmatch(installer)
	if len(version) != 2 || !strings.Contains(string(pkg), `version = "`+string(version[1])+`";`) {
		t.Fatal("Nix and installer runtime versions differ")
	}
	archives := regexp.MustCompile(`\{"(microsandbox-[^"]+)", "([a-f0-9]{64})"\}`).FindAllSubmatch(installer, -1)
	if len(archives) != 3 {
		t.Fatalf("got %d runtime archives, want 3", len(archives))
	}
	for _, archive := range archives {
		hash, err := hex.DecodeString(string(archive[2]))
		if err != nil {
			t.Fatal(err)
		}
		entry := `asset = "` + string(archive[1]) + `";\s+hash = "sha256-` + regexp.QuoteMeta(base64.StdEncoding.EncodeToString(hash)) + `";`
		if !regexp.MustCompile(entry).Match(pkg) {
			t.Errorf("Nix archive pin differs for %s", archive[1])
		}
	}
}
