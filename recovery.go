package bonnie

import (
	"context"
	"slices"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// WithReplaySafeTools permits recovery to repeat the named interrupted tools.
// The default is none. Each tool must make external actions idempotent and
// enforce its own authorization: recovery calls its implementation directly,
// not interactive Kit approval hooks. Both the recorded and current policy
// must permit replay. Do not declare shell or payment tools safe by default.
func WithReplaySafeTools(names ...string) Option {
	names = slices.Clone(names)
	return WithKitSetup(func(_ context.Context, _ *kit.Kit, scope RunScope) error {
		scope.Session.SetReplaySafeTools(names...)
		return nil
	})
}
