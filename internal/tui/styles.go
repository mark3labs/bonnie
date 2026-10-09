package tui

import "charm.land/lipgloss/v2"

// styles holds the lipgloss styles for the TUI. Kept in one place so the
// whole surface is coherent and easy to restyle.
var styles = struct {
	header         lipgloss.Style
	user           lipgloss.Style
	question       lipgloss.Style
	toolMarker     lipgloss.Style
	toolName       lipgloss.Style
	toolArgs       lipgloss.Style
	toolResultMark lipgloss.Style
	toolResult     lipgloss.Style
	reasoning      lipgloss.Style
	status         lipgloss.Style
	err            lipgloss.Style
	spinner        lipgloss.Style
	cursor         lipgloss.Style
}{
	header:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).MaxWidth(100),
	user:           lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true),
	question:       lipgloss.NewStyle().Foreground(lipgloss.Color("227")).Bold(true).MaxWidth(100),
	toolMarker:     lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true),
	toolName:       lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true),
	toolArgs:       lipgloss.NewStyle().Foreground(lipgloss.Color("250")),
	toolResultMark: lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true),
	toolResult:     lipgloss.NewStyle().Foreground(lipgloss.Color("248")),
	reasoning:      lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true).Faint(true).MaxWidth(100),
	status:         lipgloss.NewStyle().Foreground(lipgloss.Color("247")).Faint(true),
	err:            lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true),
	spinner:        lipgloss.NewStyle().Foreground(lipgloss.Color("205")),
	cursor:         lipgloss.NewStyle().Foreground(lipgloss.Color("205")),
}
