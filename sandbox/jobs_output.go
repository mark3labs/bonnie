package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// JobOutputReader reads bounded stdout and stderr output for a job.
type JobOutputReader interface {
	// ReadJobOutput returns up to limit bytes from each output stream. truncated is true
	// if either stream contains more than limit bytes.
	ReadJobOutput(ctx context.Context, id string, limit int64) (stdout, stderr string, truncated bool, err error)
}

func readRootPrefix(root *os.Root, name string, limit int64) (string, bool, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", false, err
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	return string(data), truncated, nil
}
func readHostJobOutput(ctx context.Context, directory, id string, limit int64) (string, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	if err := validateJobID(id); err != nil {
		return "", "", false, err
	}
	if limit < 0 {
		return "", "", false, fmt.Errorf("bonnie: sandbox: invalid output limit")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", "", false, fmt.Errorf("bonnie: sandbox: open job root: %w", err)
	}
	defer func() { _ = root.Close() }()
	base := filepath.Join(".bonnie-jobs", id)
	stdout, a, err := readRootPrefix(root, base+".stdout", limit)
	if err != nil {
		return "", "", false, fmt.Errorf("bonnie: sandbox: read job stdout: %w", err)
	}
	stderr, b, err := readRootPrefix(root, base+".stderr", limit)
	if err != nil {
		return "", "", false, fmt.Errorf("bonnie: sandbox: read job stderr: %w", err)
	}
	return stdout, stderr, a || b, nil
}
func (s *localSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	if s.isClosed() {
		return "", "", false, ErrClosed
	}
	return readHostJobOutput(ctx, s.host(WorkDir), id, limit)
}
func (s *landlockSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	if err := validateJobID(id); err != nil {
		return "", "", false, err
	}
	if limit < 0 {
		return "", "", false, fmt.Errorf("bonnie: sandbox: invalid output limit")
	}
	if s.isClosed() {
		return "", "", false, ErrClosed
	}
	base := ".bonnie-jobs/" + id
	stdout, a, err := readRootPrefix(s.root, base+".stdout", limit)
	if err != nil {
		return "", "", false, fmt.Errorf("bonnie: sandbox: read job stdout: %w", err)
	}
	stderr, b, err := readRootPrefix(s.root, base+".stderr", limit)
	if err != nil {
		return "", "", false, fmt.Errorf("bonnie: sandbox: read job stderr: %w", err)
	}
	return stdout, stderr, a || b, nil
}
func (s *cliSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	if err := validateJobID(id); err != nil {
		return "", "", false, err
	}
	if limit < 0 {
		return "", "", false, fmt.Errorf("bonnie: sandbox: invalid output limit")
	}
	release, err := s.operation(ctx)
	if err != nil {
		return "", "", false, err
	}
	defer release()
	if err := s.ensure(ctx); err != nil {
		return "", "", false, err
	}
	dir := s.jobsRoot() + "/" + id
	read := func(stream string) (string, bool, error) {
		command := "head -c " + fmt.Sprint(limit+1) + " -- " + shellQuote(dir+"/"+stream)
		out, stderr, code, e := runCLIRaw(ctx, nil, s.bin, s.execArgs(s, []string{"sh", "-c", command}, Command{})...)
		if e != nil {
			return "", false, e
		}
		if code != 0 {
			return "", false, fmt.Errorf("bonnie: sandbox: read job output: %s", firstLine(stderr))
		}
		truncated := int64(len(out)) > limit
		if truncated {
			out = out[:limit]
		}
		return string(out), truncated, nil
	}
	stdout, a, err := read("stdout")
	if err != nil {
		return "", "", false, err
	}
	stderr, b, err := read("stderr")
	if err != nil {
		return "", "", false, err
	}
	return stdout, stderr, a || b, nil
}

var _ JobOutputReader = (*cliSandbox)(nil)
