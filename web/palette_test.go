package web

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Check the embedded CSS against the TUI source without a terminal import.
// Source changes require a deliberate review of the browser palette.
func TestPaletteSource(t *testing.T) {
	t.Parallel()
	css, err := assets.ReadFile("assets/web.css")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := os.ReadFile("../cmd/bonnie/tui/styles.go")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile("../cmd/bonnie/tui/markdown.go")
	if err != nil {
		t.Fatal(err)
	}
	dark := paletteVariables(t, strings.Split(string(css), "@media(prefers-color-scheme:dark)")[1])
	for _, tc := range []struct {
		role, variable string
		index          int
	}{
		{"header", "brand", 205}, {"cursor", "focus", 205},
		{"user", "user", 39}, {"toolMarker", "success", 42}, {"question", "warning", 227},
	} {
		pattern := fmt.Sprintf(`%s:\s+lipgloss.NewStyle\(\).*lipgloss.Color\("%d"\)`, tc.role, tc.index)
		if !regexp.MustCompile(pattern).Match(styles) {
			t.Errorf("TUI %s changed; review web palette", tc.role)
		}
		if want := ansiCubeHex(tc.index); dark[tc.variable] != want {
			t.Errorf("--%s = %s, want ANSI %d (%s)", tc.variable, dark[tc.variable], tc.index, want)
		}
	}
	for _, tc := range []struct {
		role, variable string
		index          int
	}{
		{"Tertiary", "link", 81}, {"Text", "text", 255}, {"Base", "bg", 236}, {"Surface", "card", 235},
	} {
		pattern := fmt.Sprintf(`%s:\s+lipgloss.Color\("%d"\)`, tc.role, tc.index)
		if !regexp.MustCompile(pattern).Match(markdown) {
			t.Errorf("TUI markdown %s changed; review web palette", tc.role)
		}
		if want := ansiCubeHex(tc.index); dark[tc.variable] != want {
			t.Errorf("--%s = %s, want %s", tc.variable, dark[tc.variable], want)
		}
	}
}

func ansiCubeHex(index int) string {
	if index >= 232 {
		v := 8 + 10*(index-232)
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
	levels := []int{0, 95, 135, 175, 215, 255}
	n := index - 16
	return fmt.Sprintf("#%02x%02x%02x", levels[n/36], levels[n/6%6], levels[n%6])
}

func paletteVariables(t *testing.T, css string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, match := range regexp.MustCompile(`--([a-z-]+):\s*(#[0-9a-f]{6})`).FindAllStringSubmatch(css, -1) {
		result[match[1]] = match[2]
	}
	if len(result) != 21 {
		t.Fatalf("palette has %d variables, want 21", len(result))
	}
	return result
}

// Text meets WCAG AA even on hover and stripe surfaces. Focus and control
// borders meet the 3:1 non-text requirement. Stripes remain subtle but distinct.
func TestPaletteContrast(t *testing.T) {
	t.Parallel()
	data, err := assets.ReadFile("assets/web.css")
	if err != nil {
		t.Fatal(err)
	}
	themes := strings.Split(string(data), "@media(prefers-color-scheme:dark)")
	for i, name := range []string{"light", "dark"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := paletteVariables(t, themes[i])
			check := func(fg, bg string, minimum float64) {
				t.Helper()
				a, b := paletteLuminance(t, p[fg]), paletteLuminance(t, p[bg])
				contrast := (math.Max(a, b) + .05) / (math.Min(a, b) + .05)
				if contrast < minimum {
					t.Errorf("%s on %s: %.2f:1, want %.1f:1", fg, bg, contrast, minimum)
				}
			}
			for _, bg := range []string{"bg", "card", "zebra", "row-hover", "trace-turn", "badge-bg"} {
				for _, fg := range []string{"text", "muted", "brand", "link", "user", "success", "warning", "danger", "badge-text"} {
					check(fg, bg, 4.5)
				}
				check("focus", bg, 3)
			}
			check("primary-text", "primary", 4.5)
			check("primary-text", "primary-hover", 4.5)
			check("control-border", "card", 3)
			check("control-border", "bg", 3)
			delta := math.Abs(paletteLuminance(t, p["card"]) - paletteLuminance(t, p["zebra"]))
			if delta < .005 || delta > .15 {
				t.Errorf("stripe luminance difference = %.3f, want .005 to .15", delta)
			}
		})
	}
}

func paletteLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	n, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 24)
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for i, weight := range []float64{.2126, .7152, .0722} {
		v := float64(n>>(16-8*i)&255) / 255
		if v <= .04045 {
			v /= 12.92
		} else {
			v = math.Pow((v+.055)/1.055, 2.4)
		}
		sum += weight * v
	}
	return sum
}
