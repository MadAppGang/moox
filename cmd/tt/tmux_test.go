package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestSessionsShown classifies clients by their ancestry, shaped like the real
// machine: iTerm2 windows hang off iTermServer → login, a tablet's off tsshd.
func TestSessionsShown(t *testing.T) {
	table := map[int]proc{
		10: {ppid: 1, comm: "iTermServer-3.7.6"},
		11: {ppid: 10, comm: "login"},
		12: {ppid: 11, comm: "-zsh"},
		13: {ppid: 12, comm: "tmux"}, // iTerm2 client
		20: {ppid: 1, comm: "tsshd"},
		21: {ppid: 20, comm: "-zsh"},
		22: {ppid: 21, comm: "tmux"}, // tablet client
		23: {ppid: 21, comm: "tmux"}, // second tablet client
		30: {ppid: 1, comm: "tmux"},
		31: {ppid: 30, comm: "tmux"}, // nested client inside a pane
	}
	got := sessionsShown([]client{
		{session: "local", pid: 13},
		{session: "tablet", pid: 22},
		{session: "both", pid: 23},
		{session: "both", pid: 13},
		{session: "nested", pid: 31},
		{session: "vanished", pid: 99}, // exited between list-clients and ps
	}, table)

	want := map[string]shownOn{
		"local":    {here: 1},
		"tablet":   {remote: 1},
		"both":     {here: 1, remote: 1},
		"nested":   {here: 1},
		"vanished": {here: 1},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %v, want %v", name, got[name], w)
		}
	}
	if got["detached"] != (shownOn{}) {
		t.Errorf("a session with no client must have no badges")
	}
}

// TestRowBadges pins the row: name left, then the where-shown badges and the
// window count pushed to the right edge, so the counts line up. Badges are
// outlined in brackets, and filled (no brackets) only on the selected row.
func TestRowBadges(t *testing.T) {
	m := newTestModel(
		session{name: "magus", windows: 2, shown: shownOn{here: 1}},
		session{name: "claudish", windows: 1, shown: shownOn{here: 1, remote: 2}},
		session{name: "authflow", windows: 6},
	)
	view := m.View()
	for _, want := range []string{" local   2 windows ", "[local] [remote ×2] [1 window]", "[6 windows]"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}
	// Every count ends in the same column, filled or outlined.
	ends := map[int]bool{}
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "window") {
			ends[lipgloss.Width(strings.TrimRight(line, " "))+boolToInt(!strings.HasSuffix(strings.TrimRight(line, " "), "]"))] = true
		}
	}
	if len(ends) != 1 {
		t.Errorf("window badges end in %d different columns, want 1:\n%s", len(ends), view)
	}
}

// boolToInt is 1 for true: the filled badge's trailing padding cell is a space
// that TrimRight eats, where the outlined badge ends on its "]".
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
