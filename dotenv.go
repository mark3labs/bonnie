package bonnie

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/subosito/gotenv"
)

// DefaultDotenv is the environment file [Agent.Run] loads when it is present
// in the process's working directory.
const DefaultDotenv = ".env"

// loadDotenv reads [DefaultDotenv] into the process environment when the file
// is there, and does nothing when it is not. It reports whether a file was
// loaded, so the caller can name it in the banner.
//
// A variable already set in the environment wins: the file fills a gap, it
// never overrides what an operator exported. That is the same rule the channel
// credentials follow in [fill], and it keeps a shell `export` an override of
// the file rather than the other way round.
//
// A .env is not a manifest — it names no BONNIE setting, so it does not break
// the rule that a setting is a file at a fixed path or a Go option and never
// both. It is the fixed-path home of the credentials BONNIE already reads from
// the environment: a provider key like ANTHROPIC_API_KEY, a webhook secret
// like SLACK_SIGNING_SECRET. Loading it lets `bonnie dev` in a tree find those
// without the operator exporting each one by hand.
//
// A file that exists but cannot be read or parsed is an error, not a silent
// skip: a key that failed to load would otherwise surface much later as an
// agent with no credentials, and the cause would be invisible.
func loadDotenv() (bool, error) {
	if _, err := os.Stat(DefaultDotenv); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("bonnie: %s: %w", DefaultDotenv, err)
	}
	if err := gotenv.Load(DefaultDotenv); err != nil {
		return false, fmt.Errorf("bonnie: %s: %w", DefaultDotenv, err)
	}
	return true, nil
}
