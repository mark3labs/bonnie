package runsui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mark3labs/bonnie/runtime"
)

// The palette matches internal/tui, so `bonnie chat` and `bonnie runs`
// speak one visual language: pink for BONNIE itself, cyan for tools and
// models, and the state colors below.
var (
	colorAccent  = lipgloss.Color("205")
	colorBorder  = lipgloss.Color("240")
	colorMuted   = lipgloss.Color("243")
	colorFaint   = lipgloss.Color("238")
	colorText    = lipgloss.Color("252")
	colorCyan    = lipgloss.Color("81")
	colorBlue    = lipgloss.Color("39")
	colorGreen   = lipgloss.Color("42")
	colorYellow  = lipgloss.Color("221")
	colorRed     = lipgloss.Color("203")
	colorOrange  = lipgloss.Color("209")
	colorPurple  = lipgloss.Color("141")
	colorSelect  = lipgloss.Color("237")
	colorSelectF = lipgloss.Color("236")
)

var (
	sAccent  = lipgloss.NewStyle().Foreground(colorAccent)
	sBold    = lipgloss.NewStyle().Bold(true)
	sMuted   = lipgloss.NewStyle().Foreground(colorMuted)
	sFaint   = lipgloss.NewStyle().Foreground(colorFaint)
	sText    = lipgloss.NewStyle().Foreground(colorText)
	sCyan    = lipgloss.NewStyle().Foreground(colorCyan)
	sBlue    = lipgloss.NewStyle().Foreground(colorBlue)
	sGreen   = lipgloss.NewStyle().Foreground(colorGreen)
	sYellow  = lipgloss.NewStyle().Foreground(colorYellow)
	sRed     = lipgloss.NewStyle().Foreground(colorRed)
	sPurple  = lipgloss.NewStyle().Foreground(colorPurple)
	sKey     = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	sLabel   = lipgloss.NewStyle().Foreground(colorMuted).Width(12)
	sReason  = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
	sBadgeOn = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(colorAccent).Bold(true).Padding(0, 1)
)

// stateColor is the color of a run state everywhere it appears.
func stateColor(s runtime.RunState) color.Color {
	switch s {
	case runtime.RunRunning:
		return colorCyan
	case runtime.RunWaiting:
		return colorYellow
	case runtime.RunCompleted:
		return colorGreen
	case runtime.RunFailed:
		return colorRed
	case runtime.RunCancelled:
		return colorOrange
	case runtime.RunInterrupted:
		return colorPurple
	default:
		return colorMuted
	}
}

// stateGlyph is a one-cell mark for a run state. A running run takes the
// spinner frame instead, so the listing shows motion where work happens.
func stateGlyph(s runtime.RunState, spin string) string {
	g := "○"
	switch s {
	case runtime.RunRunning:
		g = spin
	case runtime.RunWaiting:
		g = "◆"
	case runtime.RunCompleted:
		g = "✓"
	case runtime.RunFailed:
		g = "✗"
	case runtime.RunCancelled:
		g = "⊘"
	case runtime.RunInterrupted:
		g = "↯"
	case runtime.RunRetired:
		g = "▪"
	}
	return lipgloss.NewStyle().Foreground(stateColor(s)).Render(g)
}

func stateBadge(s runtime.RunState) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(stateColor(s)).Bold(true).Padding(0, 1).Render(string(s))
}

func spanColor(sp Span) color.Color {
	switch sp.Status {
	case StatusError:
		return colorRed
	case StatusUnknown:
		return colorPurple
	}
	switch sp.Kind {
	case SpanAgent:
		return colorAccent
	case SpanChat:
		return colorBlue
	case SpanTool:
		return colorGreen
	case SpanSuspend:
		return colorYellow
	default:
		return colorMuted
	}
}

// panel draws a rounded box of exactly w×h cells with title set into the
// top border and footer into the bottom one. body is cut or padded to fit,
// so a panel never pushes its neighbours out of place.
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

	var top string
	if title != "" {
		t := ansi.Truncate(" "+title+" ", inner-1, "…")
		if focused {
			t = sKey.Render(t)
		}
		top = border.Render("╭─") + t + border.Render(strings.Repeat("─", max(0, inner-1-ansi.StringWidth(t)))+"╮")
	} else {
		top = border.Render("╭" + strings.Repeat("─", inner) + "╮")
	}

	bottom := ""
	if footer != "" {
		f := ansi.Truncate(" "+footer+" ", inner-1, "…")
		bottom = border.Render("╰"+strings.Repeat("─", max(0, inner-1-ansi.StringWidth(f)))) + f + border.Render("─╯")
	} else {
		bottom = border.Render("╰" + strings.Repeat("─", inner) + "╯")
	}

	lines := strings.Split(body, "\n")
	out := make([]string, 0, h)
	out = append(out, top)
	side := border.Render("│")
	for i := 0; i < h-2; i++ {
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

// row draws left and right parts on one line of width w, with the right
// part flush against the edge. The left part gives way first.
func row(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	if rw >= w {
		return fit(right, w)
	}
	return fit(left, w-rw-1) + " " + right
}

// highlight paints a selected row across its full width.
func highlight(line string, w int, focused bool) string {
	bg := colorSelect
	if !focused {
		bg = colorSelectF
	}
	// Re-apply the background after every reset inside the line, so styled
	// fragments keep the selection color.
	plain := fit(line, w)
	on := lipgloss.NewStyle().Background(bg).Render("x")
	prefix := on[:strings.Index(on, "x")]
	return prefix + strings.ReplaceAll(plain, ansi.ResetStyle, ansi.ResetStyle+prefix) + ansi.ResetStyle
}

// oneLine folds s onto one line and caps it at n cells.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n <= 0 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// ago is a compact relative time: "now", "42s", "5m", "3h", "2d", or a date.
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Local().Format("Jan 02")
	}
}

// dur formats a duration with two significant units at most.
func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}
