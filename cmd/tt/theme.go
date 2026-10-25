package main

import (
	"context"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The palette. Every entry is an AdaptiveColor: lipgloss resolves the pair at
// RENDER time against the background decided in resolveTheme, so one style set
// reads correctly on a light and a dark terminal without any branching at the
// call sites.
//
// The Dark values are the original hardcoded ANSI 256 codes (212/63/252/240/203)
// spelled out in hex, so the dark theme looks exactly as it always did. Only the
// Light halves are new — and they are not the same hues dimmed, because a color
// that pops against black (a near-white name, a hot pink accent) disappears
// against white. Each Light value is a darker, more saturated sibling chosen to
// clear ~4.5:1 contrast on a white background.
var (
	// colorAccent marks the highlighted row: its name, and the leading bar.
	colorAccent = lipgloss.AdaptiveColor{Light: "#B5179E", Dark: "#FF87D7"}

	// colorTitle is the header. The light value is huh's own indigo, so the
	// list header and the prompt titles in the forms land on the same color.
	colorTitle = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#5F5FFF"}

	// colorName is the session name on an unselected row: the bright half of
	// the chip. Near-white on dark, near-black on light.
	colorName = lipgloss.AdaptiveColor{Light: "#1F1F23", Dark: "#D0D0D0"}

	// colorMeta is the window count and the help/footer text: present but
	// receding. #767676 is the lightest gray still meeting AA on white.
	colorMeta = lipgloss.AdaptiveColor{Light: "#767676", Dark: "#585858"}

	// colorError renders a failed action, so it must read as a failure rather
	// than as more help text.
	colorError = lipgloss.AdaptiveColor{Light: "#C41E3A", Dark: "#FF5F5F"}
)

// queryTERM is the TERM value claimed while asking the terminal for its
// background colour. See resolveTheme for why we lie; any value termenv does
// not read as a multiplexer would do.
const queryTERM = "xterm-256color"

// resolveTheme asks the terminal what its background colour actually is and
// pins lipgloss's light/dark decision to the answer. It must be called from
// main() before bubbletea starts: the question is an OSC 11 escape sequence
// written to the tty, and the answer is read back off it, so once bubbletea's
// input loop owns stdin the reply is either swallowed (leaving the fallback
// guess in place) or delivered into the TUI as stray keystrokes.
func resolveTheme() {
	lipgloss.SetHasDarkBackground(hasDarkBackground())
}

// hasDarkBackground reports whether the terminal's background is dark.
//
// The TERM spoof is the whole point of this function. termenv refuses to send
// the OSC 11 query whenever TERM names a multiplexer, on the grounds that a
// multiplexer can be attached to several terminals at once — and then falls
// back to COLORFGBG, and finally to a hardcoded black. tt runs inside tmux
// essentially always, and this repo's tmux config deliberately unsets
// COLORFGBG (see `set-environment -gu COLORFGBG` in tmux/.tmux.conf), so both
// fallbacks are dead here and EVERY terminal was reported as dark. That is not
// a theoretical concern: measured on this machine, the fallback answered
// #000000 while the terminal's real background is #f9f6da.
//
// Asking anyway is sound because tmux answers OSC 11 itself, out of what it
// knows about the client's terminal, rather than forwarding the query to a
// terminal that might not be there — measured at 0ms, from a pane that was not
// even the visible one.
func hasDarkBackground() bool {
	// An explicit statement beats any probe. TERM_THEME is the shared variable
	// this repo's tmux theme.sh publishes (and madbench/magmux honour), and it
	// exists for the case where nothing can answer OSC 11: tmux relays only
	// what its attached client told it, and a client such as an SSH terminal on a
	// tablet never answers a colour query, so every probe inside it read "dark".
	if dark, ok := themeFromEnv(os.Getenv("TERM_THEME")); ok {
		return dark
	}
	if canQueryDirectly() {
		return termenv.NewOutput(os.Stdout, termenv.WithEnvironment(queryableEnv{})).HasDarkBackground()
	}
	// Let termenv decide by its own rules, which for an unqueryable terminal
	// means COLORFGBG if set and dark otherwise.
	return termenv.NewOutput(os.Stdout).HasDarkBackground()
}

// themeFromEnv reads a TERM_THEME value. Only "light" and "dark" are
// statements; anything else (unset, "auto", a typo) is no opinion, so
// detection proceeds as if the variable were absent.
func themeFromEnv(v string) (dark bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "dark":
		return true, true
	case "light":
		return false, true
	}
	return false, false
}

// canQueryDirectly reports whether it is safe to make termenv ask.
//
// The stake is a five-second startup freeze. termenv waits OSCTimeout for a
// reply, that timeout is a const we cannot lower, and running the query in a
// goroutine we could abandon is worse than the freeze: termenv restores the
// termios it changed in a deferred call, so an abandoned query would reset the
// terminal out from under bubbletea several seconds after the TUI had started.
//
// So the rule is to ask only where something will answer. Outside a
// multiplexer that is the terminal itself, and termenv already queries without
// help. Inside tmux it is tmux — but only while a client is attached, since
// with no client there is no terminal whose background tmux could report. A
// pane with no client is one nobody is looking at, so nothing is lost by
// taking the fallback there.
func canQueryDirectly() bool {
	if !multiplexedTERM(os.Getenv("TERM")) {
		return false // termenv will query on its own; no spoof needed or wanted
	}
	if os.Getenv("TMUX") == "" {
		return false // a multiplexer TERM we are not inside: nothing to ask
	}

	out, err := tmuxCommand(context.Background(), "display-message", "-p", "-t",
		os.Getenv("TMUX_PANE"), "#{session_attached}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "0"
}

// multiplexedTERM reports whether term is the kind of value that makes termenv
// skip the query. Note "dumb" is NOT included: termenv refuses to query a dumb
// terminal for good reason, and spoofing past that would write escape
// sequences somewhere they do not belong.
func multiplexedTERM(term string) bool {
	return strings.HasPrefix(term, "tmux") || strings.HasPrefix(term, "screen")
}

// queryableEnv is the real environment with TERM replaced, so termenv will
// send the query instead of skipping straight to its fallbacks. Only TERM is
// substituted — everything else termenv reads must stay truthful.
type queryableEnv struct{}

func (queryableEnv) Environ() []string { return os.Environ() }

func (queryableEnv) Getenv(key string) string {
	if key == "TERM" {
		return queryTERM
	}
	return os.Getenv(key)
}
