package http

import (
	"encoding/json"
	"net/http"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// SnapshotResponse holds the selected conversation branch and run state from
// one journal replay. Messages keep all content parts, not only display text.
// Cursor covers exactly that replay; subscribe after it to get later events.
// Compaction does not remove older messages from this display history. Clear,
// branch selection, and torn-step repair follow the public runtime restore API.
// Live reasoning and tool events are not part of this durable snapshot.
type SnapshotResponse struct {
	RunResponse
	Messages []kit.LLMMessage `json:"messages"`
}

func (c *Channel) handleSnapshot(w http.ResponseWriter, r *http.Request, in channel.Inbound, _ channel.Outbound) {
	runID, ok := attachedRunID(w, r, in)
	if !ok {
		return
	}
	// Do not read State or Position separately: a write between those reads
	// could move the cursor past messages that the client has not received.
	recs, err := c.core.Runner().Journal().Replay(r.Context(), runID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := SnapshotResponse{RunID: runID, Messages: []kit.LLMMessage{}}
	// Restore a private copy through the public API. Repair can write to this
	// copy, but a read request must not change the durable source journal.
	copyJournal := runtime.NewMemoryJournal()
	defer func() { _ = copyJournal.Close() }()
	for _, rec := range recs {
		out.Cursor = max(out.Cursor, rec.Seq)
		if rec.Kind == runtime.RecordState {
			out.State = rec.State
		}
		if rec.Kind == runtime.RecordSuspend {
			var sus runtime.SuspendRequest
			if err := json.Unmarshal(rec.Payload, &sus); err != nil {
				writeError(w, err)
				return
			}
			out.Suspend = &sus
		}
		if rec.Kind == runtime.RecordResume {
			out.Suspend = nil
		}
		if _, err := copyJournal.Append(r.Context(), rec); err != nil {
			writeError(w, err)
			return
		}
	}
	session, err := runtime.Restore(r.Context(), runID, copyJournal)
	if err != nil {
		writeError(w, err)
		return
	}
	out.Messages = append(out.Messages, session.GetMessages()...)
	for _, msg := range out.Messages {
		if msg.Role == kit.LLMMessageRole("assistant") {
			out.Response = snapshotText(msg)
		}
	}
	if out.State != runtime.RunWaiting {
		out.Suspend = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func snapshotText(msg kit.LLMMessage) string {
	var text strings.Builder
	for _, part := range msg.Content {
		if p, ok := part.(kit.LLMTextPart); ok {
			text.WriteString(p.Text)
		}
	}
	return text.String()
}
