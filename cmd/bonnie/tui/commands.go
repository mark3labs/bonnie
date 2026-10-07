package tui

import (
	"crypto/rand"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// slash handles local commands. Retry sends the last user text as a new turn;
// it does not remove journal records or undo tool effects.
func (m Model) slash(value string) (tea.Model, tea.Cmd) {
	switch value {
	case "/exit", "/quit":
		return m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	case "/help":
		m.commit(kindQuestion, "/new — start a separate conversation\n/retry — send the last user message again (tool effects can repeat)\n/cancel — stop the active turn\n/exit, /quit — leave chat\n/help — show commands\nPage Up/Down or mouse wheel — scroll\nCtrl+Home/End — first/latest output\nEnter — send; Shift+Enter — new line; Ctrl+W — cancel")
		m.syncViewport()
		m.viewport.GotoBottom()
		return m, nil
	case "/cancel":
		return m.handleKey(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	case "/new":
		if m.spin || m.loading {
			m.label = "wait for the turn or cancel it before /new"
			return m, nil
		}
		m.stopStream()
		next := New(m.client, m.ctx, "chat-"+rand.Text())
		next.generation = m.generation + 1
		next.width, next.height = m.width, m.height
		next.loading = false
		next.layout()
		return next, next.initCmd
	case "/retry":
		if m.spin || m.loading {
			m.label = "wait for the turn or cancel it before /retry"
			return m, nil
		}
		for _, e := range slices.Backward(m.entries) {
			if e.kind == kindUser {
				return m.send(e.text)
			}
		}
		m.label = "no user message to retry"
		return m, nil
	default:
		m.label = "unknown command: " + strings.Fields(value)[0] + " (use /help)"
		return m, nil
	}
}
