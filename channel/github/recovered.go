package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.Recoverer = (*Channel)(nil)

// errRecoveredNoInstallation is the result of a recovered run that has no
// installation to post with. A webhook carries the installation it came
// from, and it is gone after a restart; only the configured one remains.
var errRecoveredNoInstallation = errors.New("bonnie: channel/github: a recovered run needs GITHUB_INSTALLATION_ID to post its reply")

// DeliverRecovered implements [channel.Recoverer]. A run that the host
// continued after a restart posts its reply on the issue, pull request, or
// review thread that started it. The installation comes from
// [Config.InstallationID], as it does for a hand-off.
func (c *Channel) DeliverRecovered(ctx context.Context, runID string, run *runtime.Run, err error) error {
	var missing []string
	deliverErr := c.core.DeliverRecovered(ctx, runID, run, err, func(address string, run *runtime.Run, err error) {
		owner, rest, _ := strings.Cut(address, "/")
		repoName, _, _ := strings.Cut(rest, "/")
		if owner == "" || repoName == "" {
			return
		}
		if c.cfg.InstallationID == 0 {
			missing = append(missing, address)
			return
		}
		env := &envelope{
			Repository:   repo{Owner: actor{Login: owner}, Name: repoName},
			Installation: &installation{ID: c.cfg.InstallationID},
		}
		c.deliver(env, address, run, err)
	})
	if deliverErr != nil {
		return deliverErr
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", errRecoveredNoInstallation, strings.Join(missing, ", "))
	}
	return nil
}
