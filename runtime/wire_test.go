package runtime_test

import (
	"testing"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/wire"
)

// Bidirectional pointer assignments require aliases, not new defined types.
var (
	_ *wire.Run               = (*runtime.Run)(nil)
	_ *runtime.Run            = (*wire.Run)(nil)
	_ *wire.RunState          = (*runtime.RunState)(nil)
	_ *runtime.RunState       = (*wire.RunState)(nil)
	_ *wire.Event             = (*runtime.Event)(nil)
	_ *runtime.Event          = (*wire.Event)(nil)
	_ *wire.SuspendRequest    = (*runtime.SuspendRequest)(nil)
	_ *runtime.SuspendRequest = (*wire.SuspendRequest)(nil)
	_ *wire.InputResponse     = (*runtime.InputResponse)(nil)
	_ *runtime.InputResponse  = (*wire.InputResponse)(nil)
	_ *wire.CancelResult      = (*runtime.CancelResult)(nil)
	_ *runtime.CancelResult   = (*wire.CancelResult)(nil)
)

func TestWireApprovalCompatibility(t *testing.T) {
	t.Parallel()
	approved := runtime.Approve("yes")
	rejected := runtime.Reject("no")
	if approved.Approved == nil || !*approved.Approved || approved.Text != "yes" {
		t.Fatalf("unexpected approval: %+v", approved)
	}
	if rejected.Approved == nil || *rejected.Approved || rejected.Text != "no" {
		t.Fatalf("unexpected rejection: %+v", rejected)
	}
}
