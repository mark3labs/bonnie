package bonnie

import (
	"os"
	"path/filepath"
	"testing"
)

// A .env in the working directory fills the environment before a run reads it,
// which is what lets `bonnie dev` find a provider key the operator wrote once
// into a file instead of exporting by hand.
func TestLoadDotenvFillsTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultDotenv), []byte("BONNIE_TEST_KEY=from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	loaded, err := loadDotenv()
	if err != nil {
		t.Fatalf("loadDotenv: %v", err)
	}
	if !loaded {
		t.Fatal("loadDotenv reported no file, but .env is present")
	}
	if got := os.Getenv("BONNIE_TEST_KEY"); got != "from-file" {
		t.Fatalf("BONNIE_TEST_KEY = %q, want the file's value", got)
	}
}

// An exported variable wins over the file: the .env fills a gap, it never
// overrides what an operator set, the same rule the channel credentials follow
// in fill.
func TestLoadDotenvDoesNotOverrideTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultDotenv), []byte("BONNIE_TEST_KEY=from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("BONNIE_TEST_KEY", "from-env")

	if _, err := loadDotenv(); err != nil {
		t.Fatalf("loadDotenv: %v", err)
	}
	if got := os.Getenv("BONNIE_TEST_KEY"); got != "from-env" {
		t.Fatalf("BONNIE_TEST_KEY = %q, want the exported value to win", got)
	}
}

// A missing .env is the common case, not an error: an agent that never wrote
// one still serves.
func TestLoadDotenvAbsentIsNotAnError(t *testing.T) {
	t.Chdir(t.TempDir())

	loaded, err := loadDotenv()
	if err != nil {
		t.Fatalf("loadDotenv with no file: %v", err)
	}
	if loaded {
		t.Fatal("loadDotenv reported a file where the directory has none")
	}
}
