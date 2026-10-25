package main

// pane_narrow.go is the sidebar's "too narrow" state (the user, 2026-10-01: a
// sidebar split by hand came out one column wide and showed nothing). Below
// paneMinWidth a row cannot hold its code, runtime, a readable name and its
// context count, so instead of a frame cut to illegibility the sidebar says
// "make me bigger" and offers a button: `w`, or a click anywhere in the pane,
// asks tmux to resize the pane to the sidebar's width (@tt_obs_width, 46 by
// default). The state is the sidebar's alone — `tt panel-render` and the TUI's
// column draw the panel at any width they are given.

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// paneMinWidth is the narrowest sidebar that draws the panel: the name starts
// at column 17, keeps at least 12 columns (nameMinCols), and the 6-column
// context badge closes the row after one spring column.
const paneMinWidth = 17 + nameMinCols + 1 + ctxBadgeWidth

// The narrow state's words and its key.
const (
	paneNarrowText  = "make me bigger"
	paneWidenLabel  = "widen"
	paneWidenKey    = "w"
	paneWidenFormat = "#{@tt_obs_width}"
)

// narrow reports whether the pane is too narrow to draw the panel.
func (m paneModel) narrow() bool {
	return m.panel.width > 0 && m.panel.width < paneMinWidth
}

// narrowView is the too-narrow state, centred in whatever the pane has: the
// words wrapped to the width (a word wider than it is broken, down to one
// letter a line), a blank line, and the widen button in the start button's
// reverse-video style. At a height too small for it all the button wins: it
// is the way out.
func (m paneModel) narrowView() string {
	w, h := m.panel.width, m.panel.height
	words := wrapText(paneNarrowText, w)
	button := wrapText(paneWidenLabel+" "+paneWidenKey, max(w-2, 1))
	if w < 3 {
		button = wrapText(paneWidenKey, w)
	}
	var lines []string
	for _, l := range words {
		lines = append(lines, centreIn(panelMetaStyle.Render(l), ansi.StringWidth(l), w))
	}
	lines = append(lines, "")
	for _, l := range button {
		pad := l
		if w >= 3 {
			pad = " " + l + " "
		}
		lines = append(lines, centreIn(panelButtonStyle.Render(pad), ansi.StringWidth(pad), w))
	}
	if h > 0 && len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	if h > 0 {
		top := (h - len(lines)) / 2
		lines = append(make([]string, top), lines...)
	}
	return strings.Join(lines, "\n")
}

// centreIn pads a painted string of display width sw to the middle of w.
func centreIn(painted string, sw, w int) string {
	return strings.Repeat(" ", max((w-sw)/2, 0)) + painted
}

// widen is the button: ask tmux to make this pane the sidebar's width. It is
// a command, so the tmux call runs off the update loop; the WindowSizeMsg
// tmux sends back redraws the panel.
func (m paneModel) widen() (tea.Model, tea.Cmd) {
	pane := m.opts.Pane
	if pane == "" {
		return m, nil
	}
	return m, func() tea.Msg {
		want := defaultObsWidth
		if out, err := tmuxCapture("display-message", "-p", "-t", pane, paneWidenFormat); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n >= paneMinWidth {
				want = n
			}
		}
		_ = tmuxSet("resize-pane", "-t", pane, "-x", strconv.Itoa(want))
		return nil
	}
}
