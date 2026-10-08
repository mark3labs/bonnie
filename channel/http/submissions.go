package http

import (
	"net/http"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// SubmissionRequest admits durable work on an existing run. RequestID makes
// retries return the same submission. Policy supports queue, reject, and text steer.
type SubmissionRequest struct {
	RequestID string             `json:"request_id,omitempty"`
	Text      string             `json:"text"`
	Context   []string           `json:"context,omitempty"`
	Policy    runtime.BusyPolicy `json:"policy,omitempty"`
}

func (c *Channel) durableTarget(w http.ResponseWriter, r *http.Request) bool {
	id := r.PathValue("id")
	if runtime.IsReservedRun(id) {
		writeError(w, runtime.ErrRunNotFound)
		return false
	}
	if _, err := c.runner.Snapshot(r.Context(), id); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

func (c *Channel) handleSubmit(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	if !c.durableTarget(w, r) {
		return
	}
	var req SubmissionRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := c.runner.Submit(r.Context(), r.PathValue("id"), req.RequestID, runtime.Input{Text: req.Text, Context: req.Context}, req.Policy)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, item)
}
func (c *Channel) handleSubmissions(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	if !c.durableTarget(w, r) {
		return
	}
	items, err := c.runner.Submissions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
func (c *Channel) handleAbortSubmission(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	if !c.durableTarget(w, r) {
		return
	}
	if err := c.runner.AbortSubmission(r.Context(), r.PathValue("id"), r.PathValue("submission")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (c *Channel) handleChildren(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	if !c.durableTarget(w, r) {
		return
	}
	items, err := c.runner.Children(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
func (c *Channel) handleSpawnChild(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	if !c.durableTarget(w, r) {
		return
	}
	var req struct {
		Key        string `json:"key"`
		Text       string `json:"text"`
		Background bool   `json:"background,omitempty"`
	}
	if !decode(w, r, &req) {
		return
	}
	child, err := c.runner.SpawnChild(r.Context(), r.PathValue("id"), req.Key, runtime.Input{Text: req.Text}, req.Background)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, child)
}

func (c *Channel) handleCancelOwned(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	if !c.durableTarget(w, r) {
		return
	}
	var req struct {
		IncludeBackground bool `json:"include_background,omitempty"`
	}
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	if err := c.runner.CancelOwned(r.Context(), r.PathValue("id"), req.IncludeBackground); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
