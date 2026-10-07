package schedule

import (
	"context"
	"fmt"

	"github.com/mark3labs/bonnie/runtime"
)

// snapshotRecords reconstructs the boundary of this dispatch, not the latest
// human turn on a shared conversation. No message payload is reconstructed
// from its display text.
func snapshotRecords(ctx context.Context, id string, records []runtime.Record) (*runtime.Run, error) {
	journal := runtime.NewMemoryJournal()
	for _, rec := range records {
		if _, err := journal.Append(ctx, rec); err != nil {
			return nil, fmt.Errorf("bonnie: schedule: restore boundary: %w", err)
		}
	}
	return runtime.NewRunner(journal, nil).Snapshot(ctx, id)
}
