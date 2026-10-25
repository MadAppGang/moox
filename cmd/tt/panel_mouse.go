package main

// panel_mouse.go is the sidebar's mouse (the user, 2026-10-20: "handle clicks
// with the mouse without getting into copy mode: when I click a tab, when I
// scroll, when I click NEED, RUN, IDL to filter").
//
// The sidebar holds the mouse whenever it runs, so tmux hands it the clicks
// and the wheel instead of acting on them itself — a wheel over a pane that
// does not want the mouse puts that pane into copy mode. tmux still focuses
// the pane on a click (its MouseDown1Pane binding selects the pane, then
// passes the click on), so click-to-focus keeps working; drag-to-select text
// in the sidebar is what this gives up.
//
//   - a click on a tab switches to that grouping, like its hotkey;
//   - a click on a count in the strip (NEED, RUN, IDL, ???) shows only that
//     state, and a second click turns it off, like f and the state's letter;
//   - a click on a row puts the cursor on it (enter, or a second click, then
//     goes there);
//   - the wheel scrolls the list three lines a notch.
//
// Every hit is read back off the frame the reader is looking at: the plain
// text of the line clicked, and the row map the last frame left in the memo.

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// panelWheelLines is how far one wheel notch scrolls the list.
const panelWheelLines = 3

// clickAt acts on a left click at column x of screen line y. It reports
// whether the click hit anything.
func (p *panel) clickAt(x, y int) bool {
	p.clickedRow = false
	if p.snap == nil || p.memo == nil {
		return false
	}
	lines := p.lines()
	if y < 0 || y >= len(lines) {
		return false
	}
	text := ansi.Strip(lines[y])
	if y == 0 {
		if mode, ok := tabAt(text, x); ok {
			p.chooseMode(mode)
			return true
		}
		return false
	}
	if y < p.memo.bodyTop {
		if st, ok := stripStateAt(text, x); ok {
			p.picking = false
			p.pickByKey(stripKeys[st])
			return true
		}
		return false
	}
	i := y - p.memo.bodyTop + p.memo.bodyOff
	if p.memo.view > 0 && y-p.memo.bodyTop >= p.memo.view {
		return false // the footer, below the scroll window
	}
	for j, r := range p.memo.nav {
		if i >= r.first && i <= r.last {
			p.cursor, p.cursorAt = r.key, j
			p.clickedRow = true
			return true
		}
	}
	return false
}

// wheel scrolls the list by notches (positive is down).
func (p *panel) wheel(notches int) {
	p.lines() // the scroll range of the frame the reader sees
	p.scrollBy(notches * panelWheelLines)
}

// spans is a line's words — runs of non-space cells — as [start, end) columns,
// split at │ too: the tabs line's dividers stand right next to its labels.
type wordSpan struct {
	start, end int
	text       string
	divider    bool
}

func lineSpans(text string) []wordSpan {
	var out []wordSpan
	col := 0
	cur := -1
	var b strings.Builder
	flush := func(end int) {
		if cur >= 0 {
			out = append(out, wordSpan{start: cur, end: end, text: b.String()})
			cur = -1
			b.Reset()
		}
	}
	for _, r := range text {
		w := ansi.StringWidth(string(r))
		switch {
		case r == ' ':
			flush(col)
		case r == '│':
			flush(col)
			out = append(out, wordSpan{start: col, end: col + w, text: "│", divider: true})
		default:
			if cur < 0 {
				cur = col
			}
			b.WriteRune(r)
		}
		col += w
	}
	flush(col)
	return out
}

// tabAt is the tab under column x of the tabs line: the line's tabs are its
// first word and each word after a divider, in panelTabs order. A line that
// does not draw every tab (too narrow) has no tab to click.
func tabAt(text string, x int) (panelMode, bool) {
	var labels []wordSpan
	next := true
	for _, s := range lineSpans(text) {
		switch {
		case s.divider:
			next = true
		case next:
			labels = append(labels, s)
			next = false
		}
	}
	if len(labels) != len(panelTabs) {
		return defaultPanelMode, false
	}
	for i, s := range labels {
		// The tab's pads are part of it: one cell either side.
		if x >= s.start-1 && x <= s.end {
			return panelTabs[i].mode, true
		}
	}
	return defaultPanelMode, false
}

// stripStateAt is the state whose count is under column x of the count strip:
// a count is its number and its word ("5 NEED", "[N]EED" in the picker),
// with a cell of pad either side.
func stripStateAt(text string, x int) (stripState, bool) {
	spans := lineSpans(text)
	for i, s := range spans {
		st, ok := stripWordState(s.text)
		if !ok {
			continue
		}
		start := s.start
		if i > 0 {
			start = spans[i-1].start // the number before the word
		}
		if x >= start-1 && x <= s.end {
			return st, true
		}
	}
	return 0, false
}

// stripWordState is the state a strip word names, picker brackets and all.
func stripWordState(word string) (stripState, bool) {
	w := strings.NewReplacer("[", "", "]", "", "▸", "").Replace(word)
	switch strings.ToUpper(w) {
	case "NEED":
		return stripNeed, true
	case "RUN":
		return stripRun, true
	case "IDL":
		return stripIdle, true
	case "???":
		return stripUnclear, true
	}
	return 0, false
}
