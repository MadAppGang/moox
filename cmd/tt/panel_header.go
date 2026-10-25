package main

// panel_header.go draws the whole view's chrome (FR9, architecture revision
// 4): the grouping TABS and the BASELINE under them, the COUNT STRIP, the
// centred SECTION TITLES, and the KEY STRIP pinned to the bottom row. The
// design sheet is moox/cmd/tt/scripts/view-options.zsh (`task tt:views`); every
// colour comes from theme_panel.go's view tokens by name.
//
// Each piece degrades on a narrow panel instead of overflowing it, in the
// order the design gives: decoration first (the fades, the rules, the ══
// fill), then the labels shorten, and only then is anything cut.

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The chrome's glyphs.
const (
	tabDivider     = "│" // between two tabs
	baselineThin   = "─" // the baseline, and a section title's rules
	baselineThick  = "━" // the baseline's stretch under the active tab
	keyFillGlyph   = "═" // the key strip's K4 fill
	fadeLightGlyph = "░"
	fadeMidGlyph   = "▒"
	fadeDarkGlyph  = "▓"
)

// The whole view's fixed line counts: the header is the tabs, the baseline and
// the count strip, then two blank lines before the first section title; the
// footer is two blank lines and the key strip, the last line of the panel.
// When the band alone would not fit otherwise (panelDetail.chrome) a frame
// first drops those blank lines, and then everything but the tabs.
const (
	headerLines        = 3
	headerGap          = 2
	footerGap          = 2
	footerLines        = 1
	panelFadeMin       = 2 // the fewest fade cells worth drawing on a side
	panelKeyFillMin    = 2 // the fewest ══ cells worth drawing on a side
	panelTitleLabelMin = 1 // a section label is cut, never dropped
	stripLabelNeed     = "NEED"
	stripLabelRun      = "RUN"
	stripLabelIdle     = "IDL"
	stripLabelUnclear  = unclearCode
	ctrlKeyPrefix      = "^"
	cursorJumpGlyph    = "↵" // enter: go to the highlighted session
)

// The chrome's styles, built once from the view tokens.
var (
	tabOnStyle      = lipgloss.NewStyle().Bold(true).Foreground(panelPal.view.tabOnFg).Background(panelPal.view.tabOnBg)
	tabOffStyle     = lipgloss.NewStyle().Foreground(panelPal.view.tabOffFg).Background(panelPal.view.tabOffBg)
	tabDividerStyle = lipgloss.NewStyle().Foreground(panelPal.rule).Background(panelPal.view.tabOffBg)
	baselineStyle   = lipgloss.NewStyle().Foreground(panelPal.view.tabOnBg)
	// tabRestStyle is the active tab while the sidebar does not hold the
	// focus: the accent as text on the plain tab fill, no fill of its own.
	tabRestStyle = lipgloss.NewStyle().Bold(true).Foreground(panelPal.view.tabOnBg).Background(panelPal.view.tabOffBg)
	// tabDimStyle and tabDimDividerStyle are the other tabs at rest: no fill,
	// the dimmest text (ash, scored on the page) and rule-coloured dividers,
	// so the active tab is the one filled block on the line.
	tabDimStyle = lipgloss.NewStyle().Foreground(panelPal.ash)
	// restNoteStyle is the at-rest footer's note: the dimmest text, bare on
	// the page.
	restNoteStyle      = lipgloss.NewStyle().Foreground(panelPal.ash)
	tabDimDividerStyle = lipgloss.NewStyle().Foreground(panelPal.rule)
	titleLabelStyle    = lipgloss.NewStyle().Bold(true).Foreground(panelPal.view.titleFg).Background(panelPal.view.titleBg)
	titleCountStyle    = lipgloss.NewStyle().Foreground(panelPal.view.countFg).Background(panelPal.view.countBg)
	latestStyle        = lipgloss.NewStyle().Foreground(panelPal.view.latest)
	keyKeyStyle        = lipgloss.NewStyle().Bold(true).Foreground(panelPal.view.keyFg).Background(panelPal.view.keyBg)
	keyLabelStyle      = lipgloss.NewStyle().Foreground(panelPal.view.keyLabelFg).Background(panelPal.view.keyLabelBg)
)

// ---------------------------------------------------------------------------
// Tabs and baseline
// ---------------------------------------------------------------------------

// panelTab is one grouping tab: its label at three lengths, and which letter
// of it is the sidebar's hotkey — shown in square brackets in the tab, as
// " [P]roject │ S[t]atus │ T[m]ux │ [C]olour " (the user's amendment of
// 2026-09-29: brackets, not an underline, so the key survives any terminal).
// The hotkey is the first letter unless that is already a sidebar key
// (q g d r s), else the next letter of the word: p, t, m, c. Every shortened
// label keeps the hotkey letter at the same index.
type panelTab struct {
	mode              panelMode
	label, short, min string
	hot               int
}

// panelTabs are the tabs in the mode key's cycle order.
var panelTabs = []panelTab{
	{mode: modeProject, label: "Project", short: "Proj", min: "P", hot: 0},
	{mode: modeStatus, label: "Status", short: "Stat", min: "St", hot: 1},
	{mode: modeTmux, label: "Tmux", short: "Tmux", min: "Tm", hot: 1},
	{mode: modeColour, label: "Colour", short: "Col", min: "C", hot: 0},
}

// key is the tab's hotkey, lower case.
func (t panelTab) key() string { return strings.ToLower(t.label[t.hot : t.hot+1]) }

// panelModeForKey is the mode a sidebar tab hotkey switches to, in either
// case: p, t, m, c. Only the sidebar binds them — in the TUI every printable
// key is a search character.
func panelModeForKey(k string) (panelMode, bool) {
	for _, t := range panelTabs {
		if strings.EqualFold(k, t.key()) {
			return t.mode, true
		}
	}
	return defaultPanelMode, false
}

// tabText is a tab's label as drawn: the hotkey letter bracketed when the
// surface binds it ("S[t]atus"), the plain label otherwise.
// keyedTabs reports whether the tabs bracket their hotkey letters: only on
// the sidebar, the one surface that binds them, and only while it holds the
// focus — at rest no key reaches it, so none is advertised (the user,
// 2026-10-01).
func (p panel) keyedTabs() bool { return p.surface == surfacePane && !p.blurred }

func tabText(label string, hot int, keyed bool) string {
	if !keyed || hot >= len(label) {
		return label
	}
	return label[:hot] + "[" + label[hot:hot+1] + "]" + label[hot+1:]
}

// tabsLine is the header's first line: the tabs joined by │ dividers and
// centred, the active one filled with the accent. The hotkey letters are
// bracketed only on the sidebar, the one surface that binds them: in the TUI
// they are search characters, and a bracket would advertise a key that types
// into the box. It also returns the active tab's columns [start, end) — the
// whole tab, its pads included — for the baseline. When the full labels do
// not fit the short ones are used, then the shortest; below that only the
// active tab is drawn.
func (p panel) tabsLine(active panelMode) (string, int, int) {
	return p.tabsLineIn(active, p.width)
}

// tabsLineBeside is the tabs line of a scrolled frame: the tabs centred in
// what the scroll status leaves, and the status in the top-right corner,
// dim. A panel too narrow for both draws the tabs alone.
func (p panel) tabsLineBeside(active panelMode, status string) (string, int, int) {
	// Too narrow for every tab beside the whole status: the status keeps
	// only its arrows (its first two cells), so the tabs stay a row of tabs.
	if p.width-ansi.StringWidth(status)-1 < p.tabsMinWidth() {
		status = ansi.Truncate(status, 2, "")
	}
	sw := ansi.StringWidth(status)
	w := p.width - sw - 1
	if w < 3 {
		return p.tabsLine(active)
	}
	line, start, end := p.tabsLineIn(active, w)
	pad := max(w-ansi.StringWidth(line), 0) + 1
	return line + strings.Repeat(" ", pad) + panelMetaStyle.Render(status), start, end
}

// tabsMinWidth is the width of every tab at its shortest label.
func (p panel) tabsMinWidth() int {
	w := len(panelTabs) - 1
	for _, t := range panelTabs {
		w += len(tabText(t.min, t.hot, p.keyedTabs())) + 2
	}
	return w
}

// tabsLineIn is tabsLine laid out in width columns.
func (p panel) tabsLineIn(active panelMode, width int) (string, int, int) {
	keyed := p.keyedTabs()
	for _, pick := range []func(panelTab) string{
		func(t panelTab) string { return tabText(t.label, t.hot, keyed) },
		func(t panelTab) string { return tabText(t.short, t.hot, keyed) },
		func(t panelTab) string { return tabText(t.min, t.hot, keyed) },
	} {
		w := len(panelTabs) - 1 // the dividers
		for _, t := range panelTabs {
			w += len(pick(t)) + 2
		}
		if w > width {
			continue
		}
		left := (width - w) / 2
		var b strings.Builder
		b.WriteString(strings.Repeat(" ", left))
		col, start, end := left, left, left
		for i, t := range panelTabs {
			if i > 0 {
				b.WriteString(p.tabDivider())
				col++
			}
			label := pick(t)
			if t.mode == active {
				start, end = col, col+len(label)+2
			}
			b.WriteString(p.renderTab(label, t.mode == active))
			col += len(label) + 2
		}
		return b.String(), start, end
	}
	for _, t := range panelTabs {
		if t.mode == active {
			label := panelTruncate(tabText(t.min, t.hot, keyed), max(width-2, 0))
			if width < 3 {
				return tabOnStyle.Render(panelTruncate(t.min, width)), 0, min(width, len(t.min))
			}
			return p.renderTab(label, true), 0, len(label) + 2
		}
	}
	return "", 0, 0
}

// renderTab is one tab, " label ", filled with the accent when it is the
// active one. At rest the active tab is lettered in the accent on the tab
// fill and the others lose their fill and dim, so it stays the prominent one.
func (p panel) renderTab(label string, on bool) string {
	switch {
	case on && p.blurred:
		return tabRestStyle.Render(" " + label + " ")
	case on:
		return tabOnStyle.Render(" " + label + " ")
	}
	if p.blurred {
		return tabDimStyle.Render(" " + label + " ")
	}
	return tabOffStyle.Render(" " + label + " ")
}

// tabDivider is the │ between two tabs, on the tab fill — or, at rest, bare
// on the page with the dimmed tabs.
func (p panel) tabDivider() string {
	if p.blurred {
		return tabDimDividerStyle.Render(tabDivider)
	}
	return tabDividerStyle.Render(tabDivider)
}

// baseline is the header's second line: a thin rule the panel's width, with
// a thick stretch in the accent exactly under the active tab — in the rule
// colour while the sidebar is at rest.
func (p panel) baseline(start, end int) string {
	start, end = min(max(start, 0), p.width), min(max(end, 0), p.width)
	style := baselineStyle
	if p.blurred {
		style = hairlineStyle
	}
	return hairline(start) + style.Render(strings.Repeat(baselineThick, max(end-start, 0))) + hairline(p.width-end)
}

// ---------------------------------------------------------------------------
// Count strip
// ---------------------------------------------------------------------------

// panelCounts is the whole fleet's live sessions by strip state: needs you,
// running (active), idle and unclear. ENDED is not counted: it has its own
// line and its own key.
type panelCounts [stripStates]int

// stripStateOf is the strip state a status group is counted in, and whether
// it is counted at all.
func stripStateOf(g panelGroup) (stripState, bool) {
	switch g {
	case groupNeedsYou:
		return stripNeed, true
	case groupActive:
		return stripRun, true
	case groupIdle:
		return stripIdle, true
	case groupUnclear:
		return stripUnclear, true
	}
	return 0, false
}

// stripLabels are the strip's labels at full length and at one letter.
var (
	stripLabels      = [stripStates]string{stripLabelNeed, stripLabelRun, stripLabelIdle, stripLabelUnclear}
	stripShortLabels = [stripStates]string{"N", "R", "I", "?"}
)

// stripStyles are each state's two halves: the number bold on the stronger
// fill (solid red for NEED), the label on the dimmer one.
var stripStyles = func() [stripStates]chipStyle {
	var out [stripStates]chipStyle
	for st := range out {
		c := panelPal.view.strip[st]
		out[st] = chipStyle{
			label: lipgloss.NewStyle().Bold(true).Foreground(c.labelFg).Background(c.labelBg),
			value: lipgloss.NewStyle().Foreground(c.valueFg).Background(c.valueBg),
		}
	}
	return out
}()

// countStrip is the header's third line: one split badge [n|LABEL] per
// non-zero count, joined with no gap and centred, with a ░░▒▒▓ fade filling
// the gap on the left and ▓▒▒░░ on the right. "" when nothing is live. On a
// narrow panel the fades go first, then the labels shorten to one letter,
// and only then is the strip cut.
func (p panel) countStrip(c panelCounts) string {
	build := func(labels [stripStates]string) (string, int) {
		var b strings.Builder
		w := 0
		for st := stripState(0); st < stripStates; st++ {
			if c[st] <= 0 {
				continue
			}
			n, l := " "+strconv.Itoa(c[st])+" ", " "+labels[st]+" "
			// In the picker each state's word shows its letter in brackets,
			// as the tabs show theirs ("[N]EED"), in the badge's own colours;
			// the state shown is marked ▸ on the accent, in the picker and
			// out of it.
			if p.picking {
				l = " " + pickWord(labels[st], stripKeys[st]) + " "
			}
			if p.pickStateOn && p.pickState == st {
				b.WriteString(tabOnStyle.Render("▸"))
				w++
			}
			b.WriteString(stripStyles[st].label.Render(n) + stripStyles[st].value.Render(l))
			w += ansi.StringWidth(n) + ansi.StringWidth(l)
		}
		return b.String(), w
	}
	strip, w := build(stripLabels)
	if w == 0 {
		return ""
	}
	// The fades are drawn only beside the full strip: a shortened one already
	// says the panel is too narrow for decoration.
	full := w <= p.width
	if !full {
		strip, w = build(stripShortLabels)
		if w > p.width {
			return ansi.Truncate(strip, p.width, "")
		}
	}
	gapL := (p.width - w) / 2
	gapR := p.width - w - gapL
	if full && gapL-1 >= panelFadeMin && gapR-1 >= panelFadeMin {
		return hairlineStyle.Render(fade(gapL-1, false)) + " " + strip + " " + hairlineStyle.Render(fade(gapR-1, true))
	}
	return strings.Repeat(" ", gapL) + strip
}

// fade is n cells of the strip's side fade: light at the panel's edge and
// darkest next to the strip (░░▒▒▓ at five cells), mirrored when rightSide.
// The proportions hold at any length, so the fade fills whatever gap the
// counts leave.
func fade(n int, rightSide bool) string {
	cells := make([]string, n)
	for i := range cells {
		t := (float64(i) + 0.5) / float64(n)
		switch {
		case t < 0.4:
			cells[i] = fadeLightGlyph
		case t < 0.8:
			cells[i] = fadeMidGlyph
		default:
			cells[i] = fadeDarkGlyph
		}
	}
	if rightSide {
		for i, j := 0, len(cells)-1; i < j; i, j = i+1, j-1 {
			cells[i], cells[j] = cells[j], cells[i]
		}
	}
	return strings.Join(cells, "")
}

// ---------------------------------------------------------------------------
// Section titles
// ---------------------------------------------------------------------------

// sectionTitle is a section's one heading line, for every group including
// NEEDS YOU:
//
//	─────────  label  n  ─────────
//
// the label bold on a neutral fill (in colour mode the group's own name badge),
// the row count on the count fill, centred on thin rules. No per-group
// summary: the header's strip has the counts. A collapsed group is this line
// alone, its count the only thing said about its rows. On a narrow panel the
// rules go first, then the label is cut; the count stays.
func (p panel) sectionTitle(sec *panelSection) string {
	style := titleLabelStyle
	if sec.hue.known() {
		style = nameBadgeStyle(sec.hue).Bold(true)
	}
	count := " " + strconv.Itoa(len(sec.rows)) + " "
	// In the "show only" picker the section's key leads its title in
	// brackets, as the tabs show theirs, in the title's own colours.
	title := sec.title
	if p.picking && sec.pick != "" {
		title = "[" + sec.pick + "] " + title
	}
	label := " " + title + " "
	w := ansi.StringWidth(label) + ansi.StringWidth(count)
	switch {
	case w+2+2*panelRuleMin <= p.width:
		left := (p.width - w - 2) / 2
		return hairline(left) + " " + style.Render(label) + titleCountStyle.Render(count) + " " + hairline(p.width-w-2-left)
	case w <= p.width:
		return strings.Repeat(" ", (p.width-w)/2) + style.Render(label) + titleCountStyle.Render(count)
	}
	room := p.width - ansi.StringWidth(count) - 2
	if room < panelTitleLabelMin {
		return panelTruncate(strings.TrimSpace(label)+strings.TrimRight(count, " "), p.width)
	}
	return style.Render(" "+panelTruncate(title, room)+" ") + titleCountStyle.Render(count)
}

// ---------------------------------------------------------------------------
// Key strip
// ---------------------------------------------------------------------------

// panelKeyHint is one key of the key strip: the key and what it does.
type panelKeyHint struct{ key, label string }

// keyHints are this surface's keys, named as this surface binds them: plain
// letters in the sidebar (/ to filter, g, d, and j/k while the rows scroll),
// chords in the TUI (^g, ^d), where every printable key is a search character.
// The TUI never lists the sidebar's keys. r (refresh) is bound and not listed:
// it matters only on a degraded source, whose note says so.
func (p panel) keyHints(scrollable bool) []panelKeyHint {
	if p.surface == surfacePane {
		// The cursor's keys lead; g is left to the tabs, whose brackets
		// already show a key for every grouping (it still cycles).
		return []panelKeyHint{{keyScrollDown + "/" + keyScrollUp, "move"}, {cursorJumpGlyph, "go"}, {filterKey, "filter"}, {pickKey, "only"}}
	}
	return []panelKeyHint{{ctrlKeyPrefix + keyPanelMode, "group"}}
}

// keyStrip is the key strip of a frame that does not scroll.
func (p panel) keyStrip() string { return p.keyStripFor(false) }

// keyStripFor is the panel's last line: one joined strip of dim split badges
// [/|filter][g|group][d|ended] (with [j/k|scroll] first while the rows
// scroll), the key half a shade stronger, centred with the K4 ══ fill either
// side in the rule colour. On a narrow panel the fill goes first, then the
// labels (the keys alone), and only then is it cut.
func (p panel) keyStripFor(scrollable bool) string {
	hints := p.keyHints(scrollable)
	// A sidebar lights its ══ in the accent, matching the baseline above,
	// so the panel is bracketed in it top and bottom while it holds the
	// focus (at rest the footer is restLine, no keys at all).
	fill, keyStyle := hairlineStyle, keyKeyStyle
	if p.surface == surfacePane {
		fill = baselineStyle
	}
	build := func(labels bool) (string, int) {
		var b strings.Builder
		w := 0
		for _, h := range hints {
			k := " " + h.key + " "
			b.WriteString(keyStyle.Render(k))
			w += ansi.StringWidth(k)
			if labels {
				l := " " + h.label + " "
				b.WriteString(keyLabelStyle.Render(l))
				w += ansi.StringWidth(l)
			}
		}
		return b.String(), w
	}
	strip, w := build(true)
	if w > p.width {
		strip, w = build(false)
		if w > p.width {
			return ansi.Truncate(strip, p.width, "")
		}
		return strings.Repeat(" ", (p.width-w)/2) + strip
	}
	gapL := (p.width - w) / 2
	gapR := p.width - w - gapL
	if gapL-1 >= panelKeyFillMin && gapR-1 >= panelKeyFillMin {
		return fill.Render(strings.Repeat(keyFillGlyph, gapL-1)) + " " + strip + " " + fill.Render(strings.Repeat(keyFillGlyph, gapR-1))
	}
	return strings.Repeat(" ", gapL) + strip
}
