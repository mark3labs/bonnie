package runtime

import (
	"strconv"
	"strings"
	"sync"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// now is a package-level clock so tests can freeze time.
var now = time.Now

// messageText flattens the text parts of a message for journal storage and
// tree display. Non-text parts (tool calls, files, reasoning) are skipped —
// the full typed message stays in memory on the entry.
func messageText(m kit.LLMMessage) string {
	var b strings.Builder
	for _, part := range m.Content {
		if t, ok := part.(kit.LLMTextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// toolResultText extracts the text of a tool-result output part.
//
// It exists so BONNIE never names charm.land/fantasy directly. Kit re-exports
// the fantasy types as aliases, but not fantasy's generic accessor, so this is
// the public-API-only way to read a tool result. The pointer case mirrors
// fantasy's own accessor, which accepts either form.
func toolResultText(out kit.LLMToolResultOutputContent) (string, bool) {
	switch v := out.(type) {
	case kit.LLMToolResultOutputContentText:
		return v.Text, true
	case *kit.LLMToolResultOutputContentText:
		return v.Text, true
	}
	return "", false
}

// idgen produces monotonic entry IDs that are stable across a replay.
type idgen struct {
	mu sync.Mutex
	n  int
}

func (g *idgen) next(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + "-" + itoa(g.n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// entrySeq reads the counter out of an entry ID such as "m-12". It returns 0
// for any ID that idgen did not produce, so a restore can never rewind the
// counter below a replayed entry.
func entrySeq(id string) int {
	_, digits, ok := strings.Cut(id, "-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
