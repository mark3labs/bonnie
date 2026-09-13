package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/indaco/herald"
	heraldmd "github.com/indaco/herald-md"
)

// maxTextWidth caps the readable line length of assistant prose. The
// transcript is scrollback, so the terminal wraps whatever the renderer
// leaves long; the cap only stops a wide terminal from stretching one
// paragraph across the whole screen.
const maxTextWidth = 100

// markdownTypography is the shared herald typography for assistant markdown.
// Constructing a Typography builds dozens of lipgloss styles, so it must
// never happen inside a per-frame render path. The pattern — one cached
// instance, safe without a mutex because Bubble Tea's Update/View cycle is
// single-threaded — comes from Kit's internal/ui, which uses the same pair
// of libraries.
var markdownTypography = newMarkdownTypography()

// newMarkdownTypography builds the markdown theme from a palette plus
// per-element overrides. It deliberately does NOT pass a herald.Theme
// literal: WithTheme replaces the theme wholesale and leaves every field it
// does not mention at its zero value, and many of those fields are glyphs
// and widths rather than colors. Kit hit this for real — a literal rendered
// "- item" as a bare indented line, "---" as nothing at all, and collapsed
// tables to "a│b". Seeding from the palette keeps herald's defaults for every
// token and spends overrides only where the color has to be ours.
//
// The palette colors match the hand-picked styles in styles.go so the
// markdown surface speaks the same visual language as the rest of the TUI.
func newMarkdownTypography() *herald.Typography {
	return herald.New(
		herald.WithPalette(herald.ColorPalette{
			Primary:   lipgloss.Color("205"), // headings — the header/spinner pink
			Secondary: lipgloss.Color("205"),
			Tertiary:  lipgloss.Color("81"), // links — the tool-name cyan
			Accent:    lipgloss.Color("39"), // emphasis accents — the user-message blue
			Highlight: lipgloss.Color("227"),
			Muted:     lipgloss.Color("243"),
			Text:      lipgloss.Color("255"),
			Surface:   lipgloss.Color("235"),
			Base:      lipgloss.Color("236"),
		}),

		// The palette puts a bottom margin under every paragraph. In a
		// scrollback a blank line after each paragraph doubles the height of
		// ordinary prose, so the paragraph style is stripped to nothing —
		// including its foreground: body text inherits the terminal's own
		// text color, and pinning a theme color here can land a near-invisible
		// gray on an unusual background. Color is spent only where it carries
		// meaning: headings, links, emphasis, and code.
		herald.WithParagraphStyle(lipgloss.NewStyle()),

		// Code blocks stay background-free. The transcript renders tool
		// results inline and the question line is already bold; giving
		// assistant prose a filled panel makes the surfaces compete. herald
		// calls a code formatter only when a fence carries a language, and no
		// formatter is set here, so every fence renders plain — syntax
		// highlighting would cost a chroma dependency and can come later
		// through WithCodeFormatter without touching call sites.
		herald.WithCodeBlockStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("250"))),

		herald.WithBlockquoteStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)),
		herald.WithCodeInlineStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("81"))),
		herald.WithHRStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("243"))),

		herald.WithListBulletStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("243"))),
		herald.WithListItemStyle(lipgloss.NewStyle()),

		herald.WithBoldStyle(lipgloss.NewStyle().Bold(true)),
		herald.WithItalicStyle(lipgloss.NewStyle().Italic(true)),
		herald.WithStrikethroughStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Strikethrough(true)),
		herald.WithLinkStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Underline(true)),

		herald.WithTableHeaderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)),
		herald.WithTableBorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("243"))),

		// Lower the heading decoration one notch from herald's default bars so
		// a chat transcript does not read like a rendered document page.
		herald.WithH1UnderlineChar("─"),
		herald.WithH2UnderlineChar("·"),
	)
}

// textWidth returns the wrap width for assistant prose: the terminal width
// less a small right margin, floored for tiny windows and capped at
// [maxTextWidth]. Before the first WindowSizeMsg the width is unknown, and 80
// stands in for it.
func (m Model) textWidth() int {
	w := m.width
	if w <= 0 {
		w = 80
	}
	return min(max(w-2, 20), maxTextWidth)
}

// renderMarkdown renders assistant markdown and wraps the result to width.
//
// herald does no wrapping: given a long line it emits a long line, which the
// terminal then wraps by itself. Wrapping here instead keeps the layout under
// BONNIE's control, and lipgloss.Wrap is ANSI-aware — it measures and breaks
// styled text without splitting escape sequences. The old style's
// MaxWidth(100) was the wrong tool for this: MaxWidth truncates, so an
// assistant line longer than 100 cells silently lost its tail. The trailing
// newline herald leaves behind is trimmed so an entry does not add a blank
// line before the next one.
func renderMarkdown(content string, width int) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	rendered := heraldmd.Render(markdownTypography, []byte(content))
	if width > 0 {
		rendered = lipgloss.Wrap(rendered, width, "")
	}
	return strings.TrimSuffix(rendered, "\n")
}
