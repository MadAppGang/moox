package main

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestPaletteDefinesBothThemes guards the exact regression this palette was
// introduced to fix: a color defined for only one background. lipgloss silently
// resolves a missing half to the empty string, which renders as "no color" —
// which is how a dark-only palette turns a light terminal into unreadable mush
// rather than into an obvious error. (huh's own theme does this in places, so
// it is not a hypothetical mistake.)
func TestPaletteDefinesBothThemes(t *testing.T) {
	for name, c := range map[string]lipgloss.AdaptiveColor{
		"accent": colorAccent,
		"title":  colorTitle,
		"name":   colorName,
		"meta":   colorMeta,
		"error":  colorError,
	} {
		if c.Light == "" {
			t.Errorf("color %q has no Light value — it would vanish on a light terminal", name)
		}
		if c.Dark == "" {
			t.Errorf("color %q has no Dark value — it would vanish on a dark terminal", name)
		}
		if c.Light == c.Dark {
			t.Errorf("color %q is the same on both backgrounds (%s); a color that pops on one rarely does on the other", name, c.Light)
		}
	}
}

// TestThemeFromEnv pins the shared TERM_THEME contract: light and dark are
// statements, everything else is silence. "auto" in particular must NOT read
// as dark — it is what a shell exports when it means "detect".
func TestThemeFromEnv(t *testing.T) {
	for v, want := range map[string]struct{ dark, ok bool }{
		"dark":    {true, true},
		"light":   {false, true},
		" Light ": {false, true},
		"DARK":    {true, true},
		"auto":    {false, false},
		"":        {false, false},
		"dork":    {false, false},
	} {
		dark, ok := themeFromEnv(v)
		if dark != want.dark || ok != want.ok {
			t.Errorf("themeFromEnv(%q) = (%v, %v), want (%v, %v)", v, dark, ok, want.dark, want.ok)
		}
	}
}

// TestMultiplexedTERM pins the gate that decides whether to spoof TERM. The
// load-bearing rows are "dumb" and "" — termenv refuses to query those for good
// reason, and spoofing past that refusal would write escape sequences at a
// terminal that cannot answer, costing a five-second startup freeze.
func TestMultiplexedTERM(t *testing.T) {
	for term, want := range map[string]bool{
		"tmux-256color":   true,
		"tmux":            true,
		"screen-256color": true,
		"screen.xterm":    true,
		"xterm-256color":  false,
		"xterm-ghostty":   false,
		"alacritty":       false,
		"dumb":            false,
		"":                false,
	} {
		if got := multiplexedTERM(term); got != want {
			t.Errorf("multiplexedTERM(%q) = %v, want %v", term, got, want)
		}
	}
}

// TestQueryableEnvSubstitutesOnlyTERM pins the narrowness of the lie. termenv
// reads more than TERM out of the environment, so anything else this reported
// falsely would corrupt a decision that has nothing to do with the background
// query.
func TestQueryableEnvSubstitutesOnlyTERM(t *testing.T) {
	t.Setenv("TERM", "tmux-256color")
	t.Setenv("COLORFGBG", "15;0")

	var e queryableEnv
	if got := e.Getenv("TERM"); got != queryTERM {
		t.Fatalf("Getenv(TERM) = %q, want the spoofed %q", got, queryTERM)
	}
	if multiplexedTERM(e.Getenv("TERM")) {
		t.Fatal("the spoofed TERM must be one termenv is willing to query through")
	}
	if got := e.Getenv("COLORFGBG"); got != "15;0" {
		t.Fatalf("Getenv(COLORFGBG) = %q, want the real value passed through", got)
	}
}

// TestCanQueryDirectlyRequiresTmux covers the cheap half of the guard: a
// multiplexer TERM without $TMUX means we are not actually inside one, so there
// is nothing that could answer and we must not ask.
func TestCanQueryDirectlyRequiresTmux(t *testing.T) {
	t.Setenv("TERM", "tmux-256color")
	t.Setenv("TMUX", "")
	if canQueryDirectly() {
		t.Fatal("must not query with a multiplexer TERM but no $TMUX")
	}

	t.Setenv("TERM", "xterm-256color")
	if canQueryDirectly() {
		t.Fatal("outside a multiplexer termenv queries unaided; no spoof wanted")
	}
}
