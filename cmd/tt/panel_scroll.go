package main

// panel_scroll.go is the sidebar's view state (the user, 2026-09-29): a SCROLL
// AREA for the rows that do not fit — instead of the "+12 more" line and the
// shared fold line, which are gone — and a FILTER, `/`, that narrows the rows
// to the sessions it matches.
//
// Both are view state on the panel value, not layout: the collapse rules, the
// band's rung and the header's fleet counts are decided exactly as before, and
// both are part of the frame key, so a key press redraws at once.

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// panelFilterMax bounds the filter's text: a sidebar is 46 columns, and a
// longer query is a key held down, not a search.
const panelFilterMax = 64

// The filter prompt's glyphs and words.
const (
	filterKey    = "/"
	filterCursor = "▏"
	filterClear  = "esc"
	filterClearL = "clear"
	filterKeepL  = "keep"
	filterEnter  = "enter"
)

// scrollBy moves the scroll area n lines — j is +1, k is −1 — within what the
// last frame could scroll: past either end it stays put, so the first k after
// the bottom moves up at once.
func (p *panel) scrollBy(n int) {
	most := 0
	if p.memo != nil {
		most = p.memo.scrollMax
	}
	p.scroll = min(max(p.scroll+n, 0), most)
}

// startFilter opens the filter's prompt: the keys type into it until enter
// (keep the filter) or esc (clear it).
func (p *panel) startFilter() {
	p.filtering = true
}

// typeFilter adds typed text to the filter, sanitised, and scrolls back to
// the top: a new filter is a new list.
func (p *panel) typeFilter(text string) {
	if text = strings.Map(func(r rune) rune {
		if isControlRune(r) {
			return -1
		}
		return r
	}, text); text == "" || utf8.RuneCountInString(p.filter+text) > panelFilterMax {
		return
	}
	p.filter += text
	p.scroll = 0
}

// backspaceFilter removes the filter's last character.
func (p *panel) backspaceFilter() {
	if _, n := utf8.DecodeLastRuneInString(p.filter); n > 0 {
		p.filter = p.filter[:len(p.filter)-n]
		p.scroll = 0
	}
}

// keepFilter closes the prompt and keeps what was typed; an empty filter is
// no filter.
func (p *panel) keepFilter() {
	p.filtering = false
}

// clearFilter closes the prompt and drops the filter.
func (p *panel) clearFilter() {
	p.filter, p.filtering, p.scroll = "", false, 0
}

// filterSessions is the sessions a filter matches: its text, case-folded, in
// the session's name, its short id, its tmux session or its working
// directory. An empty filter matches every session.
func filterSessions(sessions []observer.Session, filter string) []observer.Session {
	q := strings.ToLower(strings.TrimSpace(sanitizeText(filter)))
	if q == "" {
		return sessions
	}
	var out []observer.Session
	for _, s := range sessions {
		hay := []string{rowName(s), s.CWD}
		if s.Tmux != nil {
			hay = append(hay, s.Tmux.Session)
		}
		for _, h := range hay {
			if strings.Contains(strings.ToLower(h), q) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// fleetCounts counts every live session by strip state, whatever the filter
// shows: the header's strip is the fleet's.
func fleetCounts(sessions []observer.Session) panelCounts {
	var c panelCounts
	for _, s := range sessions {
		if st, ok := stripStateOf(panelGroupOf(s)); ok {
			c[st]++
		}
	}
	return c
}

// footerLine is the panel's last line: the filter's prompt while there is a
// filter, else the surface's key strip — naming j/k when the rows scroll.
func (p panel) footerLine(scrollable bool) string {
	switch {
	case p.blurred:
		return p.restLine()
	case p.picking:
		return p.pickLine()
	case p.filtering || p.filter != "" || p.anyPick():
		return p.filterLine()
	}
	return p.keyStripFor(scrollable)
}

// restLine is the footer of a sidebar that does not hold the focus (the
// user, 2026-10-01: hide the shortcuts out of focus, and show something quiet
// in their place). No key reaches the pane, so none is shown: the line is the
// ══ fill in the rule colour around one dim note — what the filters keep
// ("claudish · RUN") when any is on, so a filtered list still says it is one,
// then the link to the observer ("connected").
func (p panel) restLine() string {
	var parts []string
	if p.filter != "" {
		parts = append(parts, sanitizeText(p.filter))
	}
	if pick := p.pickSummary(); pick != "" {
		parts = append(parts, pick)
	}
	if conn := p.connection(); conn != "" {
		parts = append(parts, conn)
	}
	fill := func(n int) string { return hairlineStyle.Render(strings.Repeat(keyFillGlyph, max(n, 0))) }
	if len(parts) == 0 {
		return fill(p.width)
	}
	text := " " + strings.Join(parts, " · ") + " "
	if w := ansi.StringWidth(text); w+2+2*panelKeyFillMin > p.width {
		return panelTruncate(strings.TrimSpace(text), p.width)
	}
	w := ansi.StringWidth(text)
	gapL := (p.width - w) / 2
	gapR := p.width - w - gapL
	return fill(gapL-1) + " " + restNoteStyle.Render(text) + " " + fill(gapR-1)
}

// pickLine is the footer while the "show only" picker is open: what its keys
// do, centred like the key strip.
func (p panel) pickLine() string {
	hints := []panelKeyHint{{"1-9", "section"}, {strings.Join(stripKeys[:], ""), "state"}, {pickKey, "done"}}
	var b strings.Builder
	w := 0
	for _, h := range hints {
		k, l := " "+h.key+" ", " "+h.label+" "
		b.WriteString(keyKeyStyle.Render(k) + keyLabelStyle.Render(l))
		w += len(k) + len(l)
	}
	if w > p.width {
		return panelTruncate(ansi.Strip(b.String()), p.width)
	}
	return strings.Repeat(" ", (p.width-w)/2) + b.String()
}

// filterLine is the filter's prompt: " / text▏ " on the key strip's fills,
// and on the right what closes it — enter keeps it while typing, esc clears
// it. A long filter shows its end.
func (p panel) filterLine() string {
	type hint struct{ key, label string }
	hints := []hint{{filterClear, filterClearL}}
	if p.filtering {
		hints = []hint{{filterEnter, filterKeepL}, {filterClear, filterClearL}}
	}
	var right strings.Builder
	rw := 0
	for _, h := range hints {
		right.WriteString(keyKeyStyle.Render(" "+h.key+" ") + keyLabelStyle.Render(" "+h.label+" "))
		rw += len(h.key) + len(h.label) + 4
	}
	cursor := ""
	if p.filtering {
		cursor = filterCursor
	}
	// The text filter and the "show only" filters share the line: "/ text"
	// when there is text (or its prompt is open), "f dotfiles · RUN" when a
	// section or state is picked.
	key, text := filterKey, sanitizeText(p.filter)+cursor
	if pick := p.pickSummary(); pick != "" {
		if p.filter == "" && !p.filtering {
			key, text = pickKey, pick
		} else {
			text += " · " + pick
		}
	}
	room := p.width - rw - 1 - (len(key) + 2) - 2
	if room < 1 {
		// Too narrow for the hints: the prompt alone.
		room, rw = p.width-(len(key)+2)-2, 0
		right.Reset()
	}
	if room < 1 {
		return panelTruncate(key+text, p.width)
	}
	if ansi.StringWidth(text) > room {
		text = ansi.TruncateLeft(text, ansi.StringWidth(text)-room, "")
	}
	left := keyKeyStyle.Render(" "+key+" ") + keyLabelStyle.Render(" "+text+" ")
	gap := p.width - ansi.StringWidth(left) - rw
	return left + strings.Repeat(" ", max(gap, 0)) + right.String()
}
