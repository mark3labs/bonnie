package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// ErrNotDirectory means the selected path exists but is not a directory.
var ErrNotDirectory = errors.New("bonnie: sandbox: not a directory")

// DirectoryEntry describes a direct child without following symbolic links.
// Kind is "file", "directory", "symlink", or "other". Name is one path segment.
type DirectoryEntry struct {
	Name string
	Kind string
}

// DirectoryReader is an optional Sandbox capability. ReadDirectory returns at
// most limit children and reports whether more exist. A non-directory root
// returns ErrNotDirectory; a missing root returns ErrNotFound. The selected
// root follows backend path policy. Descendant links must not be followed.
type DirectoryReader interface {
	ReadDirectory(ctx context.Context, directory string, limit int) ([]DirectoryEntry, bool, error)
}

func readDirectory(ctx context.Context, sb Sandbox, dir string, limit int) ([]DirectoryEntry, bool, error) {
	var entries []DirectoryEntry
	var partial bool
	var err error
	if reader, ok := sb.(DirectoryReader); ok {
		entries, partial, err = reader.ReadDirectory(ctx, dir, limit)
	} else {
		entries, partial, err = execDirectory(ctx, sb, dir, limit)
	}
	if err != nil {
		return nil, false, err
	}
	if len(entries) > limit {
		return nil, false, fmt.Errorf("bonnie: sandbox: directory reader exceeded entry bound")
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !validEntry(entry) || seen[entry.Name] {
			return nil, false, fmt.Errorf("bonnie: sandbox: invalid directory entry")
		}
		seen[entry.Name] = true
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, partial, nil
}

func validEntry(e DirectoryEntry) bool {
	return e.Name != "" && e.Name != "." && e.Name != ".." && !strings.ContainsAny(e.Name, "/\x00") && utf8.ValidString(e.Name) && (e.Kind == "file" || e.Kind == "directory" || e.Kind == "symlink" || e.Kind == "other")
}

func nativeDirectory(ctx context.Context, open func() (*os.File, error), limit int) ([]DirectoryEntry, bool, error) {
	if limit < 1 || limit > searchVisitLimit {
		return nil, false, fmt.Errorf("bonnie: sandbox: invalid directory bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	f, err := open()
	if err != nil {
		return nil, false, directoryError(err)
	}
	defer func() { _ = f.Close() }()
	entries := make([]DirectoryEntry, 0)
	for len(entries) <= limit {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		batch, err := f.ReadDir(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, directoryError(err)
		}
		if len(batch) == 0 {
			break
		}
		e := batch[0]
		info, err := e.Info()
		if err != nil {
			return nil, false, err
		}
		kind := "other"
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			kind = "symlink"
		case info.IsDir():
			kind = "directory"
		case info.Mode().IsRegular():
			kind = "file"
		}
		entries = append(entries, DirectoryEntry{e.Name(), kind})
	}
	partial := len(entries) > limit
	if partial {
		entries = entries[:limit]
	}
	return entries, partial, nil
}

func directoryError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("%w: %v", ErrNotDirectory, err)
	}
	return err
}

// User data is passed only as argv/Dir, never interpolated in this script.
// Test the selected root from WorkDir so Exec cannot create a missing directory.
const directoryScript = `export LC_ALL=C
p=$1
limit=$2
[ -e "$p" ] || [ -L "$p" ] || exit 44
[ -d "$p" ] || { [ -f "$p" ] && exit 45; exit 47; }
cd "$p" || exit 46
[ -r . ] && [ -x . ] || exit 46
n=0
bytes=0
for p in ./* ./.[!.]* ./..?*; do
 [ -e "$p" ] || [ -L "$p" ] || continue
 n=$((n+1))
 [ "$n" -le "$limit" ] || { printf 'truncated\000'; exit 0; }
 name=${p#./}
 bytes=$((bytes+${#name}+16))
 [ "$bytes" -le 1048576 ] || { printf 'truncated\000'; exit 0; }
 if [ -L "$p" ]; then kind=symlink
 elif [ -f "$p" ]; then kind=file
 elif [ -d "$p" ]; then kind=directory
 else kind=other; fi
 printf '%s\000%s\000' "$name" "$kind"
done
`

func execDirectory(ctx context.Context, sb Sandbox, dir string, limit int) ([]DirectoryEntry, bool, error) {
	if limit < 1 || limit > searchVisitLimit {
		return nil, false, fmt.Errorf("bonnie: sandbox: invalid directory bound")
	}
	resolved := Resolve(dir)
	// Use WorkDir as Dir; the selected path is positional data.
	// For /workspace this preserves Local's mapping rather than passing it in argv.
	parent, name := WorkDir, resolved
	if resolved == WorkDir {
		name = "."
	} else if rel, ok := strings.CutPrefix(resolved, WorkDir+"/"); ok {
		name = "./" + rel
	}
	res, err := sb.Exec(ctx, Command{Args: []string{"sh", "-c", directoryScript, "bonnie-directory", name, strconv.Itoa(limit)}, Dir: parent})
	if err != nil {
		return nil, false, err
	}
	if res.ExitCode == 44 {
		return nil, false, ErrNotFound
	}
	if res.ExitCode == 45 {
		return nil, false, ErrNotDirectory
	}
	if !res.OK() {
		return nil, false, fmt.Errorf("bonnie: sandbox: enumerate directory: %s", res.Output())
	}
	if len(res.Stdout) > 1048600 {
		return nil, false, fmt.Errorf("bonnie: sandbox: directory protocol exceeds byte limit")
	}
	parts := strings.Split(res.Stdout, "\x00")
	if parts[len(parts)-1] != "" {
		return nil, false, fmt.Errorf("bonnie: sandbox: incomplete directory protocol")
	}
	parts = parts[:len(parts)-1]
	partial := len(parts) > 0 && parts[len(parts)-1] == "truncated"
	if partial {
		parts = parts[:len(parts)-1]
	}
	if len(parts)%2 != 0 || len(parts)/2 > limit {
		return nil, false, fmt.Errorf("bonnie: sandbox: invalid directory protocol")
	}
	entries := make([]DirectoryEntry, 0, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		e := DirectoryEntry{parts[i], parts[i+1]}
		if !validEntry(e) {
			return nil, false, fmt.Errorf("bonnie: sandbox: invalid directory protocol entry")
		}
		entries = append(entries, e)
	}
	return entries, partial, nil
}

// ReadDirectory implements DirectoryReader using Local's path mapping.
func (s *localSandbox) ReadDirectory(ctx context.Context, dir string, limit int) ([]DirectoryEntry, bool, error) {
	if s.isClosed() {
		return nil, false, ErrClosed
	}
	return nativeDirectory(ctx, func() (*os.File, error) {
		info, err := os.Stat(s.host(dir))
		if err != nil {
			return nil, err
		}
		if err := directoryRootType(info); err != nil {
			return nil, err
		}
		return os.Open(s.host(dir))
	}, limit)
}

// ReadDirectory implements DirectoryReader through sandbox execution.
func (c *cliSandbox) ReadDirectory(ctx context.Context, dir string, limit int) ([]DirectoryEntry, bool, error) {
	return execDirectory(ctx, c, dir, limit)
}

// readDirectory preserves enumeration and routes CLI Exec through env injection.
func (s *envSandbox) readDirectory(ctx context.Context, dir string, limit int) ([]DirectoryEntry, bool, error) {
	if reader, ok := s.Sandbox.(DirectoryReader); ok && !directoryUsesExec(s.Sandbox) {
		return reader.ReadDirectory(ctx, dir, limit)
	}

	return execDirectory(ctx, s, dir, limit)
}

func directoryRootType(info fs.FileInfo) error {
	if info.IsDir() {
		return nil
	}
	if info.Mode().IsRegular() {
		return ErrNotDirectory
	}
	return fmt.Errorf("bonnie: sandbox: search root must be a regular file or directory")
}

type directoryReaderFunc func(context.Context, string, int) ([]DirectoryEntry, bool, error)

func (f directoryReaderFunc) ReadDirectory(ctx context.Context, dir string, limit int) ([]DirectoryEntry, bool, error) {
	return f(ctx, dir, limit)
}

// Preserve existing optional capabilities without adding unrelated ones.
func preserveDirectoryReader(sb Sandbox, reader DirectoryReader) Sandbox {
	capability := directoryCapability{reader, directoryUsesExec(reader)}

	jobs, j := sb.(JobSandbox)
	output, o := sb.(JobOutputReader)
	deleter, d := sb.(Deleter)
	switch {
	case j && o && d:
		return &struct {
			Sandbox
			directoryCapability
			JobSandbox
			JobOutputReader
			Deleter
		}{sb, capability, jobs, output, deleter}
	case j && o:
		return &struct {
			Sandbox
			directoryCapability
			JobSandbox
			JobOutputReader
		}{sb, capability, jobs, output}
	case j && d:
		return &struct {
			Sandbox
			directoryCapability
			JobSandbox
			Deleter
		}{sb, capability, jobs, deleter}
	case o && d:
		return &struct {
			Sandbox
			directoryCapability
			JobOutputReader
			Deleter
		}{sb, capability, output, deleter}
	case j:
		return &struct {
			Sandbox
			directoryCapability
			JobSandbox
		}{sb, capability, jobs}
	case o:
		return &struct {
			Sandbox
			directoryCapability
			JobOutputReader
		}{sb, capability, output}
	case d:
		return &struct {
			Sandbox
			directoryCapability
			Deleter
		}{sb, capability, deleter}
	default:
		return &struct {
			Sandbox
			directoryCapability
		}{sb, capability}
	}
}

type directoryCapability struct {
	DirectoryReader
	usesExec bool
}

func (c directoryCapability) directoryExec() bool { return c.usesExec }
func directoryUsesExec(reader any) bool {
	if _, ok := reader.(*cliSandbox); ok {
		return true
	}
	if marker, ok := reader.(interface{ directoryExec() bool }); ok {
		return marker.directoryExec()
	}
	// Environment adapters execute through their own wrapper.
	if _, ok := reader.(directoryReaderFunc); ok {
		return true
	}
	return false
}
