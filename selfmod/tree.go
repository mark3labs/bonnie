package selfmod

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/bonnie"
)

// maxFileBytes bounds one authored file. A tree's source is small; a larger
// file is data that belongs in context/ of a real deployment, not a change
// a model proposes.
const maxFileBytes = 1 << 20

// generatedFile is the wiring codegen writes. It is never authored: the dev
// loop and verification regenerate it from the other files.
const generatedFile = "bonnie_gen.go"

// Authored reports whether rel, a slash-separated path relative to the tree
// root, is a file the self-modification tools may change.
//
// The set is the tree's authored layout: the instructions file, Go files at
// the root (main.go and its siblings), go.mod and go.sum, and everything
// under skills/, context/ and tools/. Generated wiring, the journal, version
// control, and hidden files — which is where a .env with secrets lives —
// are never authored.
func Authored(rel string) bool {
	if rel == "" || path.IsAbs(rel) || rel != path.Clean(rel) || strings.HasPrefix(rel, "../") || rel == ".." {
		return false
	}
	for part := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	if path.Base(rel) == generatedFile {
		return false
	}
	if !strings.Contains(rel, "/") {
		switch {
		case rel == bonnie.DefaultInstructions, rel == "go.mod", rel == "go.sum":
			return true
		case strings.HasSuffix(rel, ".go"):
			return true
		}
		return false
	}
	top, _, _ := strings.Cut(rel, "/")
	switch top {
	case bonnie.DefaultSkills, bonnie.DefaultContextFiles, "tools":
		return true
	}
	return false
}

// snapshot is the content hash of every authored file under a root, keyed by
// slash-separated relative path.
type snapshot map[string]string

// takeSnapshot walks root and hashes each authored regular file. Symbolic
// links are skipped: a link could point the copy outside the tree.
func takeSnapshot(root string) (snapshot, error) {
	snap := snapshot{}
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !Authored(rel) {
			return nil
		}
		hash, err := hashFile(p)
		if err != nil {
			return err
		}
		snap[rel] = hash
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bonnie: selfmod: read tree: %w", err)
	}
	return snap, nil
}

func hashFile(p string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.Size() > maxFileBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", p, maxFileBytes)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return hashBytes(b), nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// copyFiles copies the named files from src to dst, creating directories.
func copyFiles(src, dst string, files []string) error {
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("bonnie: selfmod: copy %s: %w", rel, err)
		}
		if err := writeFileAtomic(filepath.Join(dst, filepath.FromSlash(rel)), b); err != nil {
			return err
		}
	}
	return nil
}

// change is one file that differs between the base and the staged tree.
// Hash is the staged content's hash, or "" when the file is deleted.
type change struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	Hash string `json:"hash,omitempty"`
	Base string `json:"base,omitempty"`
}

const (
	opAdd    = "add"
	opModify = "modify"
	opDelete = "delete"
)

// diffSnapshots returns the changes from base to staged, sorted by path.
func diffSnapshots(base, staged snapshot) []change {
	var out []change
	for p, h := range staged {
		switch old, ok := base[p]; {
		case !ok:
			out = append(out, change{Path: p, Op: opAdd, Hash: h})
		case old != h:
			out = append(out, change{Path: p, Op: opModify, Hash: h, Base: old})
		}
	}
	for p, h := range base {
		if _, ok := staged[p]; !ok {
			out = append(out, change{Path: p, Op: opDelete, Base: h})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// errConflict reports that the live tree changed under a staged change.
var errConflict = errors.New("bonnie: selfmod: the tree changed while the change was prepared")

// applyChanges writes staged changes to root. It is idempotent: a file that
// already has its target content is left alone, so a call that a crash cut
// off can run again. A file whose content is neither the base nor the target
// is a conflict, and nothing is written.
func applyChanges(root, staged string, changes []change) error {
	var conflicts []string
	for _, c := range changes {
		current, err := currentHash(filepath.Join(root, filepath.FromSlash(c.Path)))
		if err != nil {
			return err
		}
		if current != c.Base && current != c.Hash {
			conflicts = append(conflicts, c.Path)
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: %s", errConflict, strings.Join(conflicts, ", "))
	}
	for _, c := range changes {
		target := filepath.Join(root, filepath.FromSlash(c.Path))
		if c.Op == opDelete {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("bonnie: selfmod: delete %s: %w", c.Path, err)
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(staged, filepath.FromSlash(c.Path)))
		if err != nil {
			return fmt.Errorf("bonnie: selfmod: read staged %s: %w", c.Path, err)
		}
		if hashBytes(b) != c.Hash {
			return fmt.Errorf("bonnie: selfmod: staged %s changed after verification", c.Path)
		}
		if current, _ := currentHash(target); current == c.Hash {
			continue
		}
		if err := writeFileAtomic(target, b); err != nil {
			return err
		}
	}
	return nil
}

// currentHash is a file's content hash, or "" when it does not exist.
func currentHash(p string) (string, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("bonnie: selfmod: read %s: %w", p, err)
	}
	return hashBytes(b), nil
}

// writeFileAtomic replaces a file in one rename, so the dev loop's watcher
// never builds a half-written file.
func writeFileAtomic(target string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("bonnie: selfmod: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".selfmod-*")
	if err != nil {
		return fmt.Errorf("bonnie: selfmod: write %s: %w", target, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bonnie: selfmod: write %s: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bonnie: selfmod: write %s: %w", target, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("bonnie: selfmod: write %s: %w", target, err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("bonnie: selfmod: write %s: %w", target, err)
	}
	return nil
}

// ignoredFiles lists files the subagent wrote in staging that are not
// authored, so the result can say they were dropped.
func ignoredFiles(staging string) []string {
	var out []string
	_ = filepath.WalkDir(staging, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !Authored(rel) && path.Base(rel) != generatedFile {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// sortedKeys returns a snapshot's paths in order.
func (s snapshot) sortedKeys() []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
