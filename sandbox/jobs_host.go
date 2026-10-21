package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Job output is written directly to files, so it is not buffered in memory. Direct
// file descriptors avoid pipe-reader goroutines, but disk use is not bounded.

type jobMetadata struct {
	ID       string `json:"id"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

type hostJob struct {
	cmd            *exec.Cmd
	stdout, stderr string
	mu             sync.Mutex
	running        bool
	exit           *int
	waitErr        error
	done           chan struct{}
}

type hostJobs struct {
	mu   sync.Mutex
	jobs map[string]*hostJob
}

func (h *hostJobs) start(ctx context.Context, cmd Command, prepare func(*exec.Cmd) error, directory func(string) string, pinned *os.Root) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := cmd.validate(); err != nil {
		return "", err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("bonnie: sandbox: job ID: %w", err)
	}
	id := hex.EncodeToString(token[:])
	stdout, stderr := filepath.Join(".bonnie-jobs", id+".stdout"), filepath.Join(".bonnie-jobs", id+".stderr")
	base := directory(WorkDir)
	root := pinned
	if root == nil {
		var err error
		root, err = os.OpenRoot(base)
		if err != nil {
			return "", fmt.Errorf("bonnie: sandbox: open job root: %w", err)
		}
		defer func() { _ = root.Close() }()
	}
	if err := root.MkdirAll(".bonnie-jobs", 0o700); err != nil {
		return "", fmt.Errorf("bonnie: sandbox: create job directory: %w", err)
	}
	out, err := root.OpenFile(filepath.Join(".bonnie-jobs", id+".stdout"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("bonnie: sandbox: create job output: %w", err)
	}
	errOut, err := root.OpenFile(filepath.Join(".bonnie-jobs", id+".stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = out.Close()
		return "", fmt.Errorf("bonnie: sandbox: create job output: %w", err)
	}
	metaRoot, err := root.OpenRoot(".")
	if err != nil {
		_ = out.Close()
		_ = errOut.Close()
		return "", fmt.Errorf("bonnie: sandbox: open job metadata root: %w", err)
	}
	launched := false
	defer func() {
		if !launched {
			_ = metaRoot.Close()
		}
	}()
	jobCtx := context.Background()
	cancel := func() {}
	if cmd.Timeout > 0 {
		jobCtx, cancel = context.WithTimeout(jobCtx, cmd.Timeout)
	}
	c := exec.CommandContext(jobCtx, cmd.Args[0], cmd.Args[1:]...)
	configureProcess(c)
	c.Dir = directory(cmd.workdir())
	if pinned == nil {
		if err := os.MkdirAll(c.Dir, 0o700); err != nil {
			cancel()
			_ = out.Close()
			_ = errOut.Close()
			return "", fmt.Errorf("bonnie: sandbox: create workdir: %w", err)
		}
	}
	c.Stdout, c.Stderr = out, errOut
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytesReader(cmd.Stdin)
	}
	c.Env = append(os.Environ(), cmd.Env...)
	if err := prepare(c); err != nil {
		cancel()
		_ = out.Close()
		_ = errOut.Close()
		return "", err
	}
	defer func() {
		for _, f := range c.ExtraFiles {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	if err := c.Start(); err != nil {
		cancel()
		_ = out.Close()
		_ = errOut.Close()
		return "", fmt.Errorf("bonnie: sandbox: start job: %w", err)
	}
	launched = true
	j := &hostJob{cmd: c, stdout: stdout, stderr: stderr, running: true, done: make(chan struct{})}
	h.mu.Lock()
	if h.jobs == nil {
		h.jobs = make(map[string]*hostJob)
	}
	h.jobs[id] = j
	h.mu.Unlock()
	go func() {
		defer func() { _ = metaRoot.Close() }()
		e := c.Wait()
		cancel()
		_ = out.Close()
		_ = errOut.Close()
		j.mu.Lock()
		j.running = false
		if e == nil {
			n := 0
			j.exit = &n
		} else if ee, ok := errors.AsType[*exec.ExitError](e); ok {
			n := ee.ExitCode()
			j.exit = &n
		} else {
			j.waitErr = e
		}
		meta := jobMetadata{ID: id, Running: false, ExitCode: j.exit}
		var persistErr error
		root := metaRoot
		{
			f, createErr := root.OpenFile(filepath.Join(".bonnie-jobs", id+".tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if createErr != nil {
				persistErr = createErr
			} else {
				if encErr := json.NewEncoder(f).Encode(meta); encErr != nil {
					persistErr = encErr
				}
				if syncErr := f.Sync(); syncErr != nil && persistErr == nil {
					persistErr = syncErr
				}
				if closeErr := f.Close(); closeErr != nil && persistErr == nil {
					persistErr = closeErr
				}
				if persistErr == nil {
					persistErr = root.Rename(filepath.Join(".bonnie-jobs", id+".tmp"), filepath.Join(".bonnie-jobs", id+".json"))
				}
			}
		}
		if persistErr != nil && j.waitErr == nil {
			j.waitErr = fmt.Errorf("persist job status: %w", persistErr)
		}
		j.mu.Unlock()
		close(j.done)
	}()
	return id, nil
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func (h *hostJobs) status(ctx context.Context, id, directory string, pinned *os.Root) (*JobStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateJobID(id); err != nil {
		return nil, err
	}
	h.mu.Lock()
	j := h.jobs[id]
	h.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.waitErr != nil {
			return nil, fmt.Errorf("bonnie: sandbox: wait job: %w", j.waitErr)
		}
		return &JobStatus{ID: id, Running: j.running, ExitCode: j.exit, StdoutPath: j.stdout, StderrPath: j.stderr}, nil
	}
	root := pinned
	if root == nil {
		var err error
		root, err = os.OpenRoot(directory)
		if err != nil {
			return nil, fmt.Errorf("bonnie: sandbox: open job root: %w", err)
		}
		defer func() { _ = root.Close() }()
	}
	f, err := root.OpenFile(filepath.Join(".bonnie-jobs", id+".json"), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := root.Stat(filepath.Join(".bonnie-jobs", id+".stdout")); statErr == nil {
			return nil, ErrJobUnknown
		}
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: read job metadata: %w", err)
	}
	defer func() { _ = f.Close() }()
	var m jobMetadata
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: decode job metadata: %w", err)
	}
	if m.ID != id {
		return nil, ErrJobUnknown
	}
	return &JobStatus{ID: id, Running: m.Running, ExitCode: m.ExitCode, StdoutPath: filepath.Join(".bonnie-jobs", id+".stdout"), StderrPath: filepath.Join(".bonnie-jobs", id+".stderr")}, nil
}

func (h *hostJobs) stop(ctx context.Context) error {
	h.mu.Lock()
	ids := make([]string, 0, len(h.jobs))
	for id, job := range h.jobs {
		job.mu.Lock()
		running := job.running
		job.mu.Unlock()
		if running {
			ids = append(ids, id)
		}
	}
	h.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := h.cancel(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *hostJobs) cancel(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateJobID(id); err != nil {
		return err
	}
	h.mu.Lock()
	j := h.jobs[id]
	h.mu.Unlock()
	if j == nil {
		return ErrJobUnknown
	}
	j.mu.Lock()
	if !j.running {
		waitErr := j.waitErr
		j.mu.Unlock()
		return waitErr
	}
	if j.cmd.Process == nil {
		j.mu.Unlock()
		return fmt.Errorf("bonnie: sandbox: job process unavailable")
	}
	err := cancelJobProcess(j.cmd)
	done := j.done
	j.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-done:
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
