package evalui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The palette matches internal/runsui and internal/tui, so `bonnie eval`,
// `bonnie runs` and `bonnie chat` speak one visual language: pink for
// BONNIE, cyan for tools and models, green, amber and red for results.
var (
	colorAccent  = lipgloss.Color("205")
	colorBorder  = lipgloss.Color("240")
	colorMuted   = lipgloss.Color("243")
	colorFaint   = lipgloss.Color("238")
	colorText    = lipgloss.Color("252")
	colorCyan    = lipgloss.Color("81")
	colorGreen   = lipgloss.Color("42")
	colorYellow  = lipgloss.Color("221")
	colorRed     = lipgloss.Color("203")
	colorPurple  = lipgloss.Color("141")
	colorSelect  = lipgloss.Color("237")
	colorSelectF = lipgloss.Color("236")
)

var (
	sAccent = lipgloss.NewStyle().Foreground(colorAccent)
	sMuted  = lipgloss.NewStyle().Foreground(colorMuted)
	sFaint  = lipgloss.NewStyle().Foreground(colorFaint)
	sText   = lipgloss.NewStyle().Foreground(colorText)
	sCyan   = lipgloss.NewStyle().Foreground(colorCyan)
	sGreen  = lipgloss.NewStyle().Foreground(colorGreen)
	sYellow = lipgloss.NewStyle().Foreground(colorYellow)
	sRed    = lipgloss.NewStyle().Foreground(colorRed)
	sPurple = lipgloss.NewStyle().Foreground(colorPurple)
	sKey    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	sReason = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
	// sSelection marks text selected with the mouse, as internal/runsui
	// does.
	sSelection = lipgloss.NewStyle().Reverse(true)
)

// resultColor is the color of an eval status.
func resultColor(status string) color.Color {
	switch status {
	case StatusPass:
		return colorGreen
	case StatusFail:
		return colorRed
	case StatusRunning:
		return colorCyan
	}
	return colorMuted
}

// resultGlyph is a one-cell mark for an eval status. A running eval takes
// the spinner frame, so the list shows motion where work happens.
func resultGlyph(status, spin string) string {
	g := "○"
	switch status {
	case StatusPass:
		g = "✓"
	case StatusFail:
		g = "✗"
	case StatusSkip:
		g = "⊘"
	case StatusRunning:
		g = spin
	}
	return lipgloss.NewStyle().Foreground(resultColor(status)).Render(g)
}

// scoreColor is the color of a score status.
func scoreColor(status string) color.Color {
	switch status {
	case "passed":
		return colorGreen
	case "scored":
		return colorYellow
	case "failed":
		return colorRed
	}
	return colorMuted
}

func badge(text string, c color.Color) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(c).Bold(true).Padding(0, 1).Render(text)
}

// bar draws value from 0 to 1 in w cells, with a mark at the threshold.
func bar(rep Report, w int) string {
	if w < 3 {
		return ""
	}
	fill := int(rep.Value*float64(w) + 0.5)
	mark := -1
	switch {
	case rep.Min != nil:
		mark = int(*rep.Min*float64(w) + 0.5)
	case rep.Max != nil:
		mark = int(*rep.Max*float64(w) + 0.5)
	}
	on := lipgloss.NewStyle().Foreground(scoreColor(rep.Status))
	var b strings.Builder
	for i := range w {
		switch {
		case i == mark || (i == w-1 && mark == w):
			b.WriteString(sText.Render("│"))
		case i < fill:
			b.WriteString(on.Render("━"))
		default:
			b.WriteString(sFaint.Render("━"))
		}
	}
	return b.String()
}

// panel draws a rounded box of exactly w×h cells with title set into the
// top border and footer into the bottom one. It is the panel of
// internal/runsui.
func panel(title, footer, body string, w, h int, focused bool) string {
	if w < 4 || h < 2 {
		return ""
	}
	bc := colorBorder
	if focused {
		bc = colorAccent
	}
	border := lipgloss.NewStyle().Foreground(bc)
	inner := w - 2
	top := border.Render("╭" + strings.Repeat("─", inner) + "╮")
	if title != "" {
		t := ansi.Truncate(" "+title+" ", inner-1, "…")
		if focused {
			t = sKey.Render(t)
		}
		top = border.Render("╭─") + t + border.Render(strings.Repeat("─", max(0, inner-1-ansi.StringWidth(t)))+"╮")
	}
	bottom := border.Render("╰" + strings.Repeat("─", inner) + "╯")
	if footer != "" {
		f := ansi.Truncate(" "+footer+" ", inner-1, "…")
		bottom = border.Render("╰"+strings.Repeat("─", max(0, inner-1-ansi.StringWidth(f)))) + f + border.Render("─╯")
	}
	lines := strings.Split(body, "\n")
	out := make([]string, 0, h)
	out = append(out, top)
	side := border.Render("│")
	for i := range h - 2 {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		out = append(out, side+fit(line, inner)+side)
	}
	out = append(out, bottom)
	return strings.Join(out, "\n")
}

// fit cuts or pads a styled line to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// row draws left and right on one line of width w, with right flush against
// the edge. The left part gives way first.
func row(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	if rw >= w {
		return fit(right, w)
	}
	return fit(left, w-rw-1) + " " + right
}

// highlight paints a selected line across its full width.
func highlight(line string, w int, focused bool) string {
	bg := colorSelect
	if !focused {
		bg = colorSelectF
	}
	plain := fit(line, w)
	on := lipgloss.NewStyle().Background(bg).Render("x")
	prefix := on[:strings.Index(on, "x")]
	return prefix + strings.ReplaceAll(plain, ansi.ResetStyle, ansi.ResetStyle+prefix) + ansi.ResetStyle
}

// wrap breaks text into lines of at most w cells, with each line styled.
func wrap(text string, w int, style lipgloss.Style) []string {
	if w <= 0 {
		return nil
	}
	return strings.Split(style.Width(w).Render(text), "\n")
}

func seconds(s float64) string {
	if s >= 60 {
		return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
	}
	return fmt.Sprintf("%.1fs", s)
}
