package sandbox

import (
	"context"
	"testing"
)

type optionalSandbox struct {
	Sandbox
	env []string
}

func (s *optionalSandbox) Exec(_ context.Context, c Command) (*Result, error) {
	s.env = c.Env
	return &Result{}, nil
}
func (*optionalSandbox) Delete(context.Context) error { return nil }
func (s *optionalSandbox) StartJob(_ context.Context, cmd Command) (string, error) {
	s.env = cmd.Env
	return "id", nil
}
func (*optionalSandbox) JobStatus(context.Context, string) (*JobStatus, error) {
	return &JobStatus{}, nil
}
func (*optionalSandbox) CancelJob(context.Context, string) error { return nil }
func (*optionalSandbox) ReadJobOutput(context.Context, string, int64) (string, string, bool, error) {
	return "out", "err", false, nil
}

// These views expose only the optional interfaces named by each test case.
type sbD struct {
	Sandbox
	inner *optionalSandbox
}

func (s *sbD) Delete(c context.Context) error { return s.inner.Delete(c) }

type sbJ struct {
	Sandbox
	inner *optionalSandbox
}

func (s *sbJ) StartJob(c context.Context, x Command) (string, error) { return s.inner.StartJob(c, x) }
func (s *sbJ) JobStatus(c context.Context, x string) (*JobStatus, error) {
	return s.inner.JobStatus(c, x)
}
func (s *sbJ) CancelJob(c context.Context, x string) error { return s.inner.CancelJob(c, x) }

type sbR struct {
	Sandbox
	inner *optionalSandbox
}

func (s *sbR) ReadJobOutput(c context.Context, x string, n int64) (string, string, bool, error) {
	return s.inner.ReadJobOutput(c, x, n)
}

type sbDJ struct {
	*sbD
	innerJ *sbJ
}

func (s *sbDJ) StartJob(c context.Context, x Command) (string, error) { return s.innerJ.StartJob(c, x) }
func (s *sbDJ) JobStatus(c context.Context, x string) (*JobStatus, error) {
	return s.innerJ.JobStatus(c, x)
}
func (s *sbDJ) CancelJob(c context.Context, x string) error { return s.innerJ.CancelJob(c, x) }

type sbDR struct {
	*sbD
	innerR *sbR
}

func (s *sbDR) ReadJobOutput(c context.Context, x string, n int64) (string, string, bool, error) {
	return s.innerR.ReadJobOutput(c, x, n)
}

type sbJR struct {
	*sbJ
	innerR *sbR
}

func (s *sbJR) ReadJobOutput(c context.Context, x string, n int64) (string, string, bool, error) {
	return s.innerR.ReadJobOutput(c, x, n)
}

type sbDJR struct {
	*sbDJ
	innerR *sbR
}

func (s *sbDJR) ReadJobOutput(c context.Context, x string, n int64) (string, string, bool, error) {
	return s.innerR.ReadJobOutput(c, x, n)
}

type baseSandbox struct{ Sandbox }

func (s *baseSandbox) Exec(c context.Context, x Command) (*Result, error) {
	return s.Sandbox.Exec(c, x)
}
func (s *baseSandbox) ReadFile(c context.Context, p string) ([]byte, error) {
	return s.Sandbox.ReadFile(c, p)
}
func (s *baseSandbox) WriteFile(c context.Context, p string, b []byte) error {
	return s.Sandbox.WriteFile(c, p, b)
}
func (s *baseSandbox) Stop(c context.Context) error { return s.Sandbox.Stop(c) }
func (s *baseSandbox) Close() error                 { return s.Sandbox.Close() }
func optionalView(mask int, in *optionalSandbox) Sandbox {
	base := Sandbox(&baseSandbox{in})
	switch mask {
	case 0:
		return base
	case 1:
		return &sbD{base, in}
	case 2:
		return &sbJ{base, in}
	case 3:
		j := &sbJ{base, in}
		return &sbDJ{&sbD{base, in}, j}
	case 4:
		return &sbR{base, in}
	case 5:
		r := &sbR{base, in}
		return &sbDR{&sbD{base, in}, r}
	case 6:
		j, r := &sbJ{base, in}, &sbR{base, in}
		return &sbJR{j, r}
	default:
		j, r := &sbJ{base, in}, &sbR{base, in}
		return &sbDJR{&sbDJ{&sbD{base, in}, j}, r}
	}
}

type providerForWrapper struct{ s Sandbox }

func (p providerForWrapper) Name() string                                  { return "test" }
func (p providerForWrapper) Available(context.Context) error               { return nil }
func (p providerForWrapper) Open(context.Context, string) (Sandbox, error) { return p.s, nil }
func TestWrappersPreserveSandboxCapabilities(t *testing.T) {
	t.Parallel()
	for mask := range 8 {
		t.Run(string(rune('0'+mask)), func(t *testing.T) {
			in := &optionalSandbox{}
			view := optionalView(mask, in)
			// Seed wrapper construction mirrors Open's optional-interface wrapping.
			seeded := newSeededJobSandbox(view)
			env := newEnvSandbox(view, nil)
			for _, v := range []struct {
				name string
				s    Sandbox
			}{{"seeded", seeded}, {"env", env}} {
				if _, ok := v.s.(Deleter); ok != (mask&1 != 0) {
					t.Errorf("%s Deleter=%v", v.name, ok)
				}
				if _, ok := v.s.(JobSandbox); ok != (mask&2 != 0) {
					t.Errorf("%s JobSandbox=%v", v.name, ok)
				}
				if _, ok := v.s.(JobOutputReader); ok != (mask&4 != 0) {
					t.Errorf("%s JobOutputReader=%v", v.name, ok)
				}
			}
		})
	}
}

func TestEnvInjectedValueWinsForJobs(t *testing.T) {
	t.Parallel()
	in := &optionalSandbox{}
	view := optionalView(2, in)
	p := providerForWrapper{s: view}
	wrapped, err := EnvInjected(p, map[string]string{"TOKEN": "operator"}).Open(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	jobs := wrapped.(JobSandbox)
	if _, err := jobs.StartJob(context.Background(), Command{Env: []string{"TOKEN=command"}}); err != nil {
		t.Fatal(err)
	}
	if len(in.env) != 2 || in.env[0] != "TOKEN=command" || in.env[1] != "TOKEN=operator" {
		t.Fatalf("job env=%v", in.env)
	}
}
