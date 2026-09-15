//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	lls "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// init is the child half of [LandlockProvider].
//
// It runs in every binary that imports this package, which is what lets a
// tree's own binary confine a command without shipping a helper: the parent
// re-executes itself, and this hook takes over before main. Without the
// control variable it returns immediately, so an ordinary start is untouched.
//
// It has to be init rather than a call from main. Landlock must be applied
// after the fork and before the command it restricts, and Go gives a caller
// no hook between the two — there is no safe place to run code in a forked
// child. Re-executing and restricting on the way up is the standard answer.
//
// The function never returns in child mode: it either becomes the command or
// exits. Failing closed is the whole point — a child that could not apply the
// restriction must not run the command unconfined.
func init() {
	if os.Getenv(envLandlockExec) == "" {
		return
	}
	if os.Getenv(envLandlockExec) != landlockProtocol {
		fatalChild(fmt.Errorf("unknown jail protocol %q", os.Getenv(envLandlockExec)))
	}

	var rw, ro, argv []string
	if err := json.Unmarshal([]byte(os.Getenv(envLandlockRW)), &rw); err != nil {
		fatalChild(fmt.Errorf("decode writable paths: %w", err))
	}
	if err := json.Unmarshal([]byte(os.Getenv(envLandlockRO)), &ro); err != nil {
		fatalChild(fmt.Errorf("decode readable paths: %w", err))
	}
	if err := json.Unmarshal([]byte(os.Getenv(envLandlockArgv)), &argv); err != nil {
		fatalChild(fmt.Errorf("decode command: %w", err))
	}
	if len(argv) == 0 {
		fatalChild(fmt.Errorf("command has no arguments"))
	}

	if err := applyLandlock(rw, ro); err != nil {
		fatalChild(err)
	}

	// The control variables are the parent's protocol, not the command's
	// environment. Strip them so a tool call cannot read them, and so a
	// nested BONNIE binary does not mistake itself for a jail child.
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		switch {
		case hasPrefix(kv, envLandlockExec+"="),
			hasPrefix(kv, envLandlockRW+"="),
			hasPrefix(kv, envLandlockRO+"="),
			hasPrefix(kv, envLandlockArgv+"="):
			continue
		}
		env = append(env, kv)
	}

	bin, err := exec.LookPath(argv[0])
	if err != nil {
		fatalChild(fmt.Errorf("find %s: %w", argv[0], err))
	}

	// Become the command. The Landlock domain survives execve and cannot be
	// removed, so the command and every process it starts inherit it.
	if err := syscall.Exec(bin, argv, env); err != nil {
		fatalChild(fmt.Errorf("exec %s: %w", argv[0], err))
	}
}

// hasPrefix avoids importing strings into a file that runs before main.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// fatalChild reports a jail failure and exits without running the command.
func fatalChild(err error) {
	fmt.Fprintf(os.Stderr, "bonnie: sandbox: jail: %v\n", err)
	os.Exit(126)
}

// applyLandlock restricts this process to the given paths, irreversibly.
//
// The configuration is a fixed ABI version and NOT BestEffort. BestEffort
// degrades to doing nothing on a kernel without Landlock, which for a default
// sandbox is the worst possible outcome: the operator is told they are
// confined and they are not. [LandlockProvider.Available] has already
// established that the kernel supports this, so an error here is a real
// failure and the caller fails closed.
func applyLandlock(rw, ro []string) error {
	rules := []landlock.Rule{
		landlock.RWDirs(rw...).IgnoreIfMissing(),
		landlock.RODirs(ro...).IgnoreIfMissing(),
		landlock.RWFiles(deviceFiles...).IgnoreIfMissing(),
	}
	if err := landlock.V1.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("restrict paths: %w", err)
	}
	return nil
}

// landlockSupported reports whether this kernel can enforce the restriction.
//
// It asks the kernel for its ABI version instead of testing the release
// string: Landlock can be compiled out, or disabled with the lsm= boot
// parameter, and both look like a supported kernel from userspace otherwise.
func landlockSupported() error {
	v, err := lls.LandlockGetABIVersion()
	if err != nil {
		return fmt.Errorf("%w: the kernel has no Landlock support (%v): "+
			"use --sandbox docker, or a kernel 5.13 or newer with Landlock enabled",
			ErrUnavailable, err)
	}
	if v < 1 {
		return fmt.Errorf("%w: the kernel reports Landlock ABI %d", ErrUnavailable, v)
	}
	return nil
}

// execJailed runs one command in a re-executed child that confines itself.
//
// The child is this same binary: os.Executable resolves it once, and the
// init hook above is what turns it into a jail. The parent stays
// unrestricted, which it must — it owns the journal.
func (s *landlockSandbox) execJailed(ctx context.Context, cmd Command, dir string) (*Result, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: locate this binary: %w", err)
	}

	spec := jailSpec{
		rw:   []string{s.dir, s.tmp},
		ro:   systemPaths,
		argv: cmd.Args,
	}
	control, err := spec.env()
	if err != nil {
		return nil, err
	}

	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(ctx, self)
	c.Dir = dir
	c.Env = append(s.childEnv(cmd.Env), control...)
	return runChild(ctx, c, cmd)
}
