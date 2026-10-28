package main

// This file connects fantasy's Kronk provider (charm.land/fantasy/providers/kronk)
// to Kit. Kronk (github.com/ardanlabs/kronk) runs a GGUF model in this
// process with llama.cpp. The fantasy provider downloads llama.cpp and the
// model, loads the model, and converts Kit's calls to Kronk's chat requests.
//
// It imports fantasy. That is correct in your tree: the rule that keeps
// fantasy out of BONNIE applies to the code BONNIE ships, not to an agent.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	fkronk "charm.land/fantasy/providers/kronk"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// providerName is the provider part of the model string: "kronk/<source>".
// The source is what Kronk downloads: a Hugging Face "org/repo:quant"
// selector, a canonical Kronk model ID, or a full Hugging Face URL.
const providerName = fkronk.Name

// kronkProvider returns the Kit provider factory for Kronk. opts configure
// fantasy's Kronk provider, for example its default sampling parameters.
//
// The first call for a source downloads llama.cpp and the model, then loads
// the model. That takes minutes on a first run. Later calls use the loaded
// model.
func kronkProvider(opts ...fkronk.Option) kit.ProviderFactory {
	// One provider for the process. It keeps each model it loads. BONNIE
	// builds a new Kit for each turn of a run, and Kit calls this factory
	// each time, so a provider for each call would load the model again.
	p, err := fkronk.NewProvider(append([]fkronk.Option{fkronk.WithLogger(logKronk)}, opts...)...)
	return func(ctx context.Context, _ *kit.ProviderConfig, source string) (*kit.ProviderResult, error) {
		if err != nil {
			return nil, fmt.Errorf("kronk: provider: %w", err)
		}
		m, err := p.LanguageModel(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("kronk: model %s: %w", source, err)
		}
		// No Closer. Kit closes it when the Kit of the turn closes, and
		// p.Close unloads every model. The models stay loaded until the
		// process stops.
		return &kit.ProviderResult{Model: m}, nil
	}
}

// logKronk writes Kronk's log lines, including the download progress, to
// logFile. Not to stdout or stderr: `bonnie dev` passes both through to the
// terminal, and the lines would print over its interface.
func logKronk(ctx context.Context, msg string, args ...any) {
	kronkLog().InfoContext(ctx, terminalCodes.Replace(msg), args...)
}

// logFile is where logKronk writes, beside the journal at its default path.
// Watch a first download with `tail -f .bonnie/kronk.log`.
const logFile = ".bonnie/kronk.log"

// kronkLog opens logFile on first use. When the file cannot be opened, the
// lines are dropped: the terminal is not a fallback.
var kronkLog = sync.OnceValue(func() *slog.Logger {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err == nil {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			return slog.New(slog.NewTextHandler(f, nil))
		}
	}
	return slog.New(slog.DiscardHandler)
})

// terminalCodes removes the line-redraw codes in Kronk's progress lines,
// which are for a terminal and not for a log.
var terminalCodes = strings.NewReplacer("\x1b[K", "", "\r", "")
