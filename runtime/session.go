package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// ErrEntryNotFound is returned when an entry ID is unknown to the session.
var ErrEntryNotFound = errors.New("bonnie: session entry not found")

// Session is a journal-backed implementation of [kit.SessionManager]. It is
// the seam through which BONNIE gets durability: every message Kit appends to
// the conversation is written through to a [Journal] before it is kept in
// memory, so a crashed run can be rebuilt with [Session.Restore].
//
// Session preserves Kit's branching model — entries form a tree, not a list —
// so branch, collapse, and replay all keep working.
type Session struct {
	mu sync.RWMutex

	runID     string
	name      string
	createdAt time.Time
	journal   Journal
	ids       idgen

	entries map[string]*sessionEntry
	leaf    string // entry ID of the current branch tip; "" means root

	provider string
	modelID  string
}

type sessionEntry struct {
	meta       kit.BranchEntry
	msg        *kit.LLMMessage
	compaction *kit.CompactionEntry
	extData    *kit.ExtensionDataEntry
}

// Compile-time proof that BONNIE satisfies the public Kit contract. If Kit
// adds a method to SessionManager, this line fails the build — which is the
// early warning we want.
var _ kit.SessionManager = (*Session)(nil)

// Compile-time proof that Session takes the atomic step path. Kit
// type-asserts for kit.StepAppender at every site that persists more than
// one message, so implementing it routes every tool-calling step through
// [Session.AppendStep] and its single journal write.
var _ kit.StepAppender = (*Session)(nil)

// NewSession creates an empty journal-backed session for a run.
func NewSession(runID string, j Journal) *Session {
	if j == nil {
		j = NewMemoryJournal()
	}
	return &Session{
		runID:     runID,
		createdAt: now(),
		journal:   j,
		entries:   make(map[string]*sessionEntry),
	}
}

// Restore rebuilds a session by replaying its journal. Message, compaction,
// and extension-data records are re-applied in sequence order; everything else
// is run metadata that does not affect the conversation tree.
//
// The rebuild is lossless: messages are decoded from [Record.Payload], so tool
// calls, tool results, files, and reasoning parts survive a resume in a new
// process. A record written without a payload falls back to its flattened
// text, which keeps journals from older BONNIE versions readable.
//
// Restore also repairs a torn write. A journal can still hold an assistant
// message whose tool call has no tool result: every journal written before
// Kit v0.106.0, every journal whose implementation does not provide
// [StepJournal], and the residual torn single Write that [FileJournal]
// documents. An orphaned tool call is a conversation every provider rejects,
// so Restore drops that incomplete trailing step and journals a
// [RecordRepair]. A mismatch anywhere but the tail means the journal is
// damaged, and Restore returns [ErrCorruptConversation] rather than rewrite
// history.
func Restore(ctx context.Context, runID string, j Journal) (*Session, error) {
	recs, err := j.Replay(ctx, runID)
	if err != nil {
		return nil, err
	}

	s := NewSession(runID, j)
	maxSeq := 0
	for _, rec := range recs {
		// A record with no entry ID is run metadata, not a tree entry.
		// Treating one as an entry would give the tree a node keyed by the
		// empty string, and every later message would hang off it.
		if rec.EntryID == "" {
			continue
		}
		e, err := restoreEntry(rec)
		if err != nil {
			return nil, err
		}
		if e == nil {
			continue
		}
		s.entries[rec.EntryID] = e
		if parent, ok := s.entries[rec.ParentID]; ok {
			parent.meta.Children = append(parent.meta.Children, rec.EntryID)
		}
		s.leaf = rec.EntryID
		if n := entrySeq(rec.EntryID); n > maxSeq {
			maxSeq = n
		}
		// The last model change on the branch decides which model a resumed
		// run reports to Kit.
		if e.meta.Type == kit.EntryTypeModelChange {
			s.provider, s.modelID = e.meta.Provider, e.meta.Model
		}
	}
	// Continue the ID sequence past every restored entry, so a session that
	// keeps appending after a resume cannot collide with a replayed ID.
	s.ids.n = maxSeq

	if err := s.repairTail(ctx, recs); err != nil {
		return nil, err
	}
	return s, nil
}

// repairTail drops an incomplete trailing tool-calling step from a session
// that was just rebuilt, and journals the fact.
//
// The journal is append-only, so the orphaned records stay on disk and every
// later Restore finds the same orphan. The repair is therefore deterministic,
// and it is journalled only once: an identical repair already present in the
// replayed records means a previous Restore reported it.
func (s *Session) repairTail(ctx context.Context, prior []Record) error {
	s.mu.Lock()
	branch := s.branchLocked()

	var (
		msgs []kit.LLMMessage
		at   []int
	)
	for i, e := range branch {
		if e.msg != nil {
			msgs = append(msgs, *e.msg)
			at = append(at, i)
		}
	}

	cut, err := orphanCut(msgs)
	if err != nil || cut < 0 {
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("bonnie: restore %s: %w", s.runID, err)
		}
		return nil
	}

	dropped := branch[at[cut]:]
	ids := make([]string, 0, len(dropped))
	for _, e := range dropped {
		ids = append(ids, e.meta.ID)
	}

	parentID := dropped[0].meta.ParentID
	if p, ok := s.entries[parentID]; ok {
		p.meta.Children = slices.DeleteFunc(p.meta.Children, func(id string) bool {
			return id == dropped[0].meta.ID
		})
	}
	for _, e := range dropped {
		delete(s.entries, e.meta.ID)
	}
	s.leaf = parentID
	s.mu.Unlock()

	if repairAlreadyJournalled(prior, ids) {
		return nil
	}
	_, err = s.journal.Append(ctx, Record{
		RunID:     s.runID,
		Kind:      RecordRepair,
		Timestamp: now(),
		ParentID:  parentID,
		Text:      "dropped incomplete trailing tool-calling step",
		Payload:   encodeRepair(repairPayload{DroppedEntryIDs: ids}),
	})
	return err
}

// repairPayload is the durable form of a torn-write repair.
type repairPayload struct {
	DroppedEntryIDs []string `json:"dropped_entry_ids,omitempty"`
}

func encodeRepair(p repairPayload) json.RawMessage {
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return b
}

// repairAlreadyJournalled reports whether the same repair was recorded before.
func repairAlreadyJournalled(prior []Record, ids []string) bool {
	for _, rec := range prior {
		if rec.Kind != RecordRepair || len(rec.Payload) == 0 {
			continue
		}
		var p repairPayload
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			continue
		}
		if slices.Equal(p.DroppedEntryIDs, ids) {
			return true
		}
	}
	return false
}

// restoreEntry rebuilds one tree entry from a journal record. It returns a nil
// entry for record kinds that carry no conversation state.
func restoreEntry(rec Record) (*sessionEntry, error) {
	base := kit.BranchEntry{
		ID:        rec.EntryID,
		ParentID:  rec.ParentID,
		Role:      rec.Role,
		Content:   rec.Text,
		Timestamp: rec.Timestamp,
	}

	switch rec.Kind {
	case RecordMessage:
		msg, err := decodeMessage(rec)
		if err != nil {
			return nil, err
		}
		base.Type = kit.EntryTypeMessage
		return &sessionEntry{meta: base, msg: &msg}, nil

	case RecordCompaction:
		var p compactionPayload
		if len(rec.Payload) > 0 {
			if err := json.Unmarshal(rec.Payload, &p); err != nil {
				return nil, fmt.Errorf("bonnie: decode compaction %s: %w", rec.EntryID, err)
			}
		}
		base.Type = kit.EntryTypeCompaction
		return &sessionEntry{
			meta: base,
			compaction: &kit.CompactionEntry{
				ID: rec.EntryID, Summary: rec.Text,
				FirstKeptEntryID: p.FirstKeptEntryID,
				TokensBefore:     p.TokensBefore, TokensAfter: p.TokensAfter,
				MessagesRemoved: p.MessagesRemoved,
				ReadFiles:       p.ReadFiles, ModifiedFiles: p.ModifiedFiles,
				Timestamp: rec.Timestamp,
			},
		}, nil

	case RecordExtensionData:
		base.Type = kit.EntryTypeExtensionData
		return &sessionEntry{
			meta:    base,
			extData: &kit.ExtensionDataEntry{ID: rec.EntryID, ExtType: rec.ExtType, Data: rec.Text},
		}, nil

	case RecordModelChange:
		var p modelChangePayload
		if len(rec.Payload) > 0 {
			if err := json.Unmarshal(rec.Payload, &p); err != nil {
				return nil, fmt.Errorf("bonnie: decode model change %s: %w", rec.EntryID, err)
			}
		}
		base.Type = kit.EntryTypeModelChange
		base.Provider, base.Model = p.Provider, p.Model
		return &sessionEntry{meta: base}, nil

	case RecordBranchSummary:
		base.Type = kit.EntryTypeBranchSummary
		return &sessionEntry{meta: base}, nil

	default:
		return nil, nil
	}
}

// decodeMessage rebuilds the typed message a record stands for.
func decodeMessage(rec Record) (kit.LLMMessage, error) {
	if len(rec.Payload) == 0 {
		return kit.LLMMessage{
			Role:    kit.LLMMessageRole(rec.Role),
			Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: rec.Text}},
		}, nil
	}
	var msg kit.LLMMessage
	if err := json.Unmarshal(rec.Payload, &msg); err != nil {
		return kit.LLMMessage{}, fmt.Errorf("bonnie: decode message %s: %w", rec.EntryID, err)
	}
	return msg, nil
}

// compactionPayload is the durable form of a compaction's metadata.
type compactionPayload struct {
	FirstKeptEntryID string   `json:"first_kept_entry_id,omitempty"`
	TokensBefore     int      `json:"tokens_before,omitempty"`
	TokensAfter      int      `json:"tokens_after,omitempty"`
	MessagesRemoved  int      `json:"messages_removed,omitempty"`
	ReadFiles        []string `json:"read_files,omitempty"`
	ModifiedFiles    []string `json:"modified_files,omitempty"`
}

// RunID returns the run this session belongs to.
func (s *Session) RunID() string { return s.runID }

// AppendMessage implements [kit.SessionManager]. The whole typed message is
// encoded into the journal record, so a replay rebuilds tool calls and tool
// results rather than a text-only approximation of them.
//
// Kit no longer calls this for a multi-message step when the session also
// implements [kit.StepAppender]; it calls [Session.AppendStep] instead. The
// method stays because it is part of the frozen [kit.SessionManager]
// contract, and because single messages — a user turn, a compaction — still
// arrive one at a time.
func (s *Session) AppendMessage(msg kit.LLMMessage) (string, error) {
	// Encode before touching in-memory state: a message that cannot be
	// journalled must not enter the tree, or the session and the journal
	// disagree about what happened.
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("bonnie: encode message: %w", err)
	}

	s.mu.Lock()
	id := s.ids.next("m")
	parent := s.leaf
	text := messageText(msg)
	ts := now()

	e := &sessionEntry{
		meta: kit.BranchEntry{
			ID:        id,
			ParentID:  parent,
			Type:      kit.EntryTypeMessage,
			Role:      string(msg.Role),
			Content:   text,
			Provider:  s.provider,
			Model:     s.modelID,
			Timestamp: ts,
			RawParts:  nil,
		},
		msg: &msg,
	}
	s.entries[id] = e
	if p, ok := s.entries[parent]; ok {
		p.meta.Children = append(p.meta.Children, id)
	}
	s.leaf = id
	s.mu.Unlock()

	_, err = s.journal.Append(context.Background(), Record{
		RunID:     s.runID,
		Kind:      RecordMessage,
		Timestamp: ts,
		EntryID:   id,
		ParentID:  parent,
		Role:      string(msg.Role),
		Text:      text,
		Payload:   payload,
	})
	return id, err
}

// AppendStep implements [kit.StepAppender]. It receives every message of
// one agent step in a single call and commits them as one unit: the records
// are built first, then written through [StepJournal] with one lock and one
// fsync when the journal supports it.
//
// Before Kit v0.106.0 there was no such call, so a step was two independent
// writes and a crash between them left an orphaned tool call — the condition
// [Restore]'s repair exists to drop. With this method implemented, Kit hands
// the whole step over at once, and a [FileJournal] writes it as a single
// buffered Write followed by one fsync. The torn-write window shrinks from
// "any crash between two fsyncs" to "a torn single Write". The repair
// stays: journals written by older BONNIE versions, journals whose
// implementation does not provide [StepJournal], and the residual short-write
// case all still produce the shape it fixes.
//
// # Cancellation
//
// Kit persists a completed step before it checks for cancellation, so ctx
// may already be cancelled when this runs. Aborting on that would discard
// exactly the finished work this method exists to save, silently, because
// Kit ignores the returned error. The write therefore runs under a context
// that keeps tracing values but drops cancellation, which is the behaviour
// Kit's own contract for [kit.StepAppender] prescribes.
//
// # Ordering
//
// The tree is updated before the journal write, matching [AppendMessage]: a
// journal failure then leaves the live conversation coherent, and the
// divergence — tree ahead of journal — disappears at the next crash, because
// the process and the in-memory tree die together. An orphaned tool call
// cannot result either way: the step enters the journal all at once or not
// at all.
func (s *Session) AppendStep(ctx context.Context, msgs []kit.LLMMessage) ([]string, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	// Encode every message before any state changes: a batch that cannot
	// be journalled must not half-enter the tree.
	payloads := make([]json.RawMessage, len(msgs))
	for i := range msgs {
		b, err := json.Marshal(msgs[i])
		if err != nil {
			return nil, fmt.Errorf("bonnie: encode message: %w", err)
		}
		payloads[i] = b
	}

	s.mu.Lock()
	ids := make([]string, len(msgs))
	recs := make([]Record, len(msgs))
	parent := s.leaf
	ts := now()
	for i := range msgs {
		id := s.ids.next("m")
		ids[i] = id
		text := messageText(msgs[i])
		recs[i] = Record{
			RunID:     s.runID,
			Kind:      RecordMessage,
			Timestamp: ts,
			EntryID:   id,
			ParentID:  parent,
			Role:      string(msgs[i].Role),
			Text:      text,
			Payload:   payloads[i],
		}
		e := &sessionEntry{
			meta: kit.BranchEntry{
				ID:        id,
				ParentID:  parent,
				Type:      kit.EntryTypeMessage,
				Role:      string(msgs[i].Role),
				Content:   text,
				Provider:  s.provider,
				Model:     s.modelID,
				Timestamp: ts,
			},
			msg: &msgs[i],
		}
		s.entries[id] = e
		if p, ok := s.entries[parent]; ok {
			p.meta.Children = append(p.meta.Children, id)
		}
		s.leaf = id
		parent = id
	}
	s.mu.Unlock()

	// A completed step must survive an interrupted turn: keep the values,
	// drop the cancellation. See the Cancellation section above.
	writeCtx := context.WithoutCancel(ctx)
	if err := s.journalStep(writeCtx, recs); err != nil {
		return ids, err
	}
	return ids, nil
}

// journalStep writes one step's records through [StepJournal] when the
// journal provides it, and one [Journal.Append] per record when it does not.
//
// The fallback has the same crash window the pre-v0.106 per-message writes
// had; a host that wants crash-safe steps should implement [StepJournal].
// Both paths keep the step a unit in the tree regardless.
func (s *Session) journalStep(ctx context.Context, recs []Record) error {
	if sj, ok := s.journal.(StepJournal); ok {
		_, err := sj.AppendStep(ctx, recs)
		return err
	}
	for i := range recs {
		if _, err := s.journal.Append(ctx, recs[i]); err != nil {
			return fmt.Errorf("bonnie: append record %d of step: %w", i+1, err)
		}
	}
	return nil
}

// branchLocked walks from the current leaf to the root and returns the path in
// root-to-leaf order. The caller must hold at least a read lock.
func (s *Session) branchLocked() []*sessionEntry {
	var rev []*sessionEntry
	for id := s.leaf; id != ""; {
		e, ok := s.entries[id]
		if !ok {
			break
		}
		rev = append(rev, e)
		id = e.meta.ParentID
	}
	out := make([]*sessionEntry, 0, len(rev))
	for _, r := range slices.Backward(rev) {
		out = append(out, r)
	}
	return out
}

// GetMessages implements [kit.SessionManager].
func (s *Session) GetMessages() []kit.LLMMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []kit.LLMMessage
	for _, e := range s.branchLocked() {
		if e.msg != nil {
			out = append(out, *e.msg)
		}
	}
	return out
}

// BuildContext implements [kit.SessionManager]. It honours the most recent
// compaction on the branch: messages before FirstKeptEntryID are replaced by
// the compaction summary.
func (s *Session) BuildContext() ([]kit.LLMMessage, string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	branch := s.branchLocked()
	cut, summary := s.compactionCutLocked(branch)

	var out []kit.LLMMessage
	if summary != "" {
		out = append(out, kit.NewLLMUserMessage(summary))
	}
	for _, e := range branch[cut:] {
		if e.msg != nil {
			out = append(out, *e.msg)
		}
	}
	return out, s.provider, s.modelID
}

// compactionCutLocked returns the index into branch at which live messages
// resume, plus the summary text that replaces everything before it.
func (s *Session) compactionCutLocked(branch []*sessionEntry) (int, string) {
	for i, b := range slices.Backward(branch) {
		c := b.compaction
		if c == nil {
			continue
		}
		for j, e := range branch {
			if e.meta.ID == c.FirstKeptEntryID {
				return j, c.Summary
			}
		}
		return i + 1, c.Summary
	}
	return 0, ""
}

// GetContextEntryIDs implements [kit.SessionManager].
func (s *Session) GetContextEntryIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	branch := s.branchLocked()
	cut, _ := s.compactionCutLocked(branch)

	var out []string
	for _, e := range branch[cut:] {
		if e.msg != nil {
			out = append(out, e.meta.ID)
		}
	}
	return out
}

// Branch implements [kit.SessionManager]. An empty entryID resets to root.
func (s *Session) Branch(entryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID == "" {
		s.leaf = ""
		return nil
	}
	if _, ok := s.entries[entryID]; !ok {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, entryID)
	}
	s.leaf = entryID
	return nil
}

// GetCurrentBranch implements [kit.SessionManager].
func (s *Session) GetCurrentBranch() []kit.BranchEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []kit.BranchEntry
	for _, e := range s.branchLocked() {
		out = append(out, e.meta)
	}
	return out
}

// GetChildren implements [kit.SessionManager].
func (s *Session) GetChildren(parentID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[parentID]
	if !ok {
		return nil
	}
	out := make([]string, len(e.meta.Children))
	copy(out, e.meta.Children)
	return out
}

// GetEntry implements [kit.SessionManager].
func (s *Session) GetEntry(entryID string) *kit.BranchEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[entryID]
	if !ok {
		return nil
	}
	cp := e.meta
	return &cp
}

// GetSessionID implements [kit.SessionManager].
func (s *Session) GetSessionID() string { return s.runID }

// GetSessionName implements [kit.SessionManager].
func (s *Session) GetSessionName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// SetSessionName implements [kit.SessionManager].
func (s *Session) SetSessionName(name string) error {
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
	return nil
}

// GetCreatedAt implements [kit.SessionManager].
func (s *Session) GetCreatedAt() time.Time { return s.createdAt }

// IsPersisted implements [kit.SessionManager]. A session is persisted when its
// journal outlives the process.
func (s *Session) IsPersisted() bool { return s.journal.Persisted() }

// AppendCompaction implements [kit.SessionManager].
func (s *Session) AppendCompaction(summary, firstKeptEntryID string,
	tokensBefore, tokensAfter, messagesRemoved int, readFiles, modifiedFiles []string,
) (string, error) {
	s.mu.Lock()
	id := s.ids.next("c")
	parent := s.leaf
	ts := now()
	e := &sessionEntry{
		meta: kit.BranchEntry{
			ID: id, ParentID: parent, Type: kit.EntryTypeCompaction,
			Content: summary, Timestamp: ts,
		},
		compaction: &kit.CompactionEntry{
			ID: id, Summary: summary, FirstKeptEntryID: firstKeptEntryID,
			TokensBefore: tokensBefore, TokensAfter: tokensAfter,
			MessagesRemoved: messagesRemoved,
			ReadFiles:       readFiles, ModifiedFiles: modifiedFiles,
			Timestamp: ts,
		},
	}
	s.entries[id] = e
	if p, ok := s.entries[parent]; ok {
		p.meta.Children = append(p.meta.Children, id)
	}
	s.leaf = id
	s.mu.Unlock()

	_, err := s.journal.Append(context.Background(), Record{
		RunID: s.runID, Kind: RecordCompaction, Timestamp: ts,
		EntryID: id, ParentID: parent, Text: summary,
		Payload: encodeCompaction(compactionPayload{
			FirstKeptEntryID: firstKeptEntryID,
			TokensBefore:     tokensBefore, TokensAfter: tokensAfter,
			MessagesRemoved: messagesRemoved,
			ReadFiles:       readFiles, ModifiedFiles: modifiedFiles,
		}),
	})
	return id, err
}

func encodeCompaction(p compactionPayload) json.RawMessage {
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return b
}

// GetLastCompaction implements [kit.SessionManager].
func (s *Session) GetLastCompaction() *kit.CompactionEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	branch := s.branchLocked()
	for _, b := range slices.Backward(branch) {
		if c := b.compaction; c != nil {
			cp := *c
			return &cp
		}
	}
	return nil
}

// AppendExtensionData implements [kit.SessionManager]. This doubles as
// BONNIE's durable, branch-aware key/value store for run state.
func (s *Session) AppendExtensionData(extType, data string) (string, error) {
	s.mu.Lock()
	id := s.ids.next("x")
	parent := s.leaf
	ts := now()
	e := &sessionEntry{
		meta: kit.BranchEntry{
			ID: id, ParentID: parent, Type: kit.EntryTypeExtensionData,
			Content: data, Timestamp: ts,
		},
		extData: &kit.ExtensionDataEntry{ID: id, ExtType: extType, Data: data},
	}
	s.entries[id] = e
	if p, ok := s.entries[parent]; ok {
		p.meta.Children = append(p.meta.Children, id)
	}
	s.leaf = id
	s.mu.Unlock()

	_, err := s.journal.Append(context.Background(), Record{
		RunID: s.runID, Kind: RecordExtensionData, Timestamp: ts,
		EntryID: id, ParentID: parent, ExtType: extType, Text: data,
	})
	return id, err
}

// GetExtensionData implements [kit.SessionManager].
func (s *Session) GetExtensionData(extType string) []kit.ExtensionDataEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []kit.ExtensionDataEntry
	for _, e := range s.branchLocked() {
		if e.extData == nil {
			continue
		}
		if extType == "" || e.extData.ExtType == extType {
			out = append(out, *e.extData)
		}
	}
	return out
}

// AppendModelChange implements [kit.SessionManager]. The entry is journalled
// because it joins the conversation tree: a model change that only lives in
// memory would orphan every message appended after it on replay.
func (s *Session) AppendModelChange(provider, modelID string) (string, error) {
	s.mu.Lock()
	id := s.ids.next("v")
	parent := s.leaf
	ts := now()
	s.provider, s.modelID = provider, modelID
	e := &sessionEntry{meta: kit.BranchEntry{
		ID: id, ParentID: parent, Type: kit.EntryTypeModelChange,
		Provider: provider, Model: modelID, Timestamp: ts,
	}}
	s.entries[id] = e
	if p, ok := s.entries[parent]; ok {
		p.meta.Children = append(p.meta.Children, id)
	}
	s.leaf = id
	s.mu.Unlock()

	_, err := s.journal.Append(context.Background(), Record{
		RunID: s.runID, Kind: RecordModelChange, Timestamp: ts,
		EntryID: id, ParentID: parent,
		Payload: encodeModelChange(modelChangePayload{Provider: provider, Model: modelID}),
	})
	return id, err
}

// modelChangePayload is the durable form of a model switch.
type modelChangePayload struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

func encodeModelChange(p modelChangePayload) json.RawMessage {
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return b
}

// AppendBranchSummary implements [kit.SessionManager].
func (s *Session) AppendBranchSummary(fromID, summary string) (string, error) {
	s.mu.Lock()
	from, ok := s.entries[fromID]
	if !ok {
		s.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrEntryNotFound, fromID)
	}
	id := s.ids.next("b")
	parent := from.meta.ParentID
	ts := now()
	e := &sessionEntry{meta: kit.BranchEntry{
		ID: id, ParentID: parent, Type: kit.EntryTypeBranchSummary,
		Content: summary, Timestamp: ts,
	}}
	s.entries[id] = e
	if p, ok := s.entries[parent]; ok {
		p.meta.Children = append(p.meta.Children, id)
	}
	s.leaf = id
	s.mu.Unlock()

	_, err := s.journal.Append(context.Background(), Record{
		RunID: s.runID, Kind: RecordBranchSummary, Timestamp: ts,
		EntryID: id, ParentID: parent, Text: summary,
	})
	return id, err
}

// Close implements [kit.SessionManager]. It does not close the journal, which
// may be shared across many sessions.
func (s *Session) Close() error { return nil }
