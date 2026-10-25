package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/projectkey"
)

// This file is the panel's layout: what is drawn, in what order, and what gives
// way when the rows do not fit.
//
// The whole view (FR9, architecture revision 4) is a HEADER — the grouping
// tabs, the baseline under them, the fleet's count strip, two blank lines —
// then the SECTIONS, and a FOOTER pinned to the bottom row: two blank lines
// and the key strip, the panel's last line (panel_header.go draws them).
//
// In status mode the sections are the five status groups the panel has always
// drawn. In the grouped modes (panel_modes.go) they are a pinned NEEDS YOU
// band followed by one group per project, tmux session or colour. Every
// section opens with a centred title that carries its row count, and every
// group shows every one of its rows: nothing is folded (the user, 2026-09-29:
// "we should not fold groups like that" — the collapsed titles and "+ 5 idle"
// lines are gone, and so are "+12 more" and the shared fold line). What gives
// way when the rows do not fit is only how much is drawn under them (the one
// blank line between two groups stays):
//
//  1. every meta line but the band's, then the latest-text lines under the
//     rows outside the band, 3 → 2 → 1 → 0, every row at once;
//  2. band rows go from 4 lines to 3 to 2 (panel_rows.go's ladder: fewer
//     lines of why);
//  3. then the list SCROLLS: the header and the footer stay put, the rows
//     between them move with j/k, and the tabs line's top-right corner says
//     where the view is, "▲▼ 45%" (scrolled). At the top the band is the first
//     thing shown;
//  4. and only if the band alone does not fit in the window at 2 lines a row
//     do its rows go to one line, and then the header and the footer give up
//     their blank lines, and last everything but the tabs.
//
// Below a grouped mode's height floor (panelModeFloor) the chosen grouping is
// kept (before revision 4 the panel drew status instead and said "(short)").
//
// The band is never folded or cut and is placed first, so nothing a grouped
// mode does can bury a session that needs you.

// panelRungSlack is how many spare lines a richer band rung must leave before
// it is taken back: room for one more NEEDS YOU row, so the band does not flip
// between rungs as it grows and shrinks by one.
const panelRungSlack = 2

// panelRuleMin is the shortest rule drawn beside a section title: a shorter
// one reads as a stray dash rather than a rule.
const panelRuleMin = 3

// panelRow is one session as a section draws it.
type panelRow struct {
	s observer.Session
	// class is the session's status group, whatever section it sits in.
	class panelGroup
	// band is whether the row is in the NEEDS YOU band — the rows that keep
	// their lines longest, because they say why they are waiting.
	band bool
	// label is the group named in the row's own line: a band row's home, and
	// the real group of a row folded into "other". "" names nothing. The
	// {group} token draws it.
	label string
	// lines is the row rendered from the templates, once per frame
	// (renderRows); the layout only counts them.
	lines rowLines
}

// panelSection is a title and its rows.
type panelSection struct {
	// key identifies the section.
	key   string
	title string
	// hue is colour mode's group colour (its title is that colour's name
	// badge); the zero hue draws the title neutral.
	hue panelHue
	// band marks the NEEDS YOU section: first, never cut.
	band bool
	rows []panelRow
	// pick is the section's key in the "show only" picker (panel_pick.go).
	pick string
}

// panelFrame is everything one frame is decided from.
type panelFrame struct {
	mode    panelMode
	secs    []panelSection
	hasLive bool
	// counts is the header strip's whole-fleet counts.
	counts panelCounts
}

// panelMemo is the part of the panel that must survive the model being copied:
// bubbletea hands the model around by value, and the last frame, the band's
// rung and the caches belong to the panel, not to one copy of it.
type panelMemo struct {
	key   panelFrameKey
	lines []string

	// bandAttn is the band rung the previous frame was drawn at (§5.4), for
	// the layout rungFor. 0 is the sentinel "no previous frame".
	bandAttn int
	rungFor  panelLayoutKey

	resolver *projectkey.Resolver

	// conf is panel.conf as last loaded (panel_conf.go).
	conf *panelConf
	// latest is the latest-text cache (panel_latest.go).
	latest *latestCache
	// scrollMax is how far the last frame's rows could scroll, 0 when they
	// all fit: the most scrollBy moves panel.scroll to.
	scrollMax int
	// nav is the last frame's selectable rows, in body coordinates, and view
	// the scroll window's height (0 when every row fits) — what moveCursor
	// steers by (panel_cursor.go).
	nav  []panelNavRow
	view int
	// bodyTop is the screen line the rows start on, and bodyOff the body line
	// shown there (the scroll offset): what a click on a row maps back through
	// (panel_mouse.go).
	bodyTop, bodyOff int
}

// panelFrameKey is every input lines() reads. A frame whose key is unchanged
// is the previous frame, returned as it was: lines() is called several times a
// frame (the view, the start button's hit-test, the pane's mouse sync), and
// only the first call may move the band's rung on.
type panelFrameKey struct {
	snap    *observer.Snapshot
	now     time.Time
	width   int
	height  int
	mode    panelMode
	note    string
	surface panelSurface
	// confGen changes whenever panel.conf does, so an edited format redraws
	// an otherwise unchanged frame.
	confGen uint64
	// dark is the theme the frame was drawn in: a live switch
	// (theme_live.go) redraws an otherwise unchanged frame.
	dark bool
	// scroll, filter and filtering are the sidebar's view state: the scroll
	// offset, the filter's text, and whether its prompt is taking keys.
	scroll      int
	filter      string
	filtering   bool
	picking     bool
	pickSection string
	pickState   stripState
	pickStateOn bool
	blurred     bool
	cursor      string
	cursorAt    int
}

// panelLayoutKey is what the carried band rung is valid for.
type panelLayoutKey struct {
	width, height int
	mode          panelMode
}

func newPanelMemo() *panelMemo {
	home, _ := os.UserHomeDir()
	return &panelMemo{resolver: projectkey.New(home), conf: newPanelConf(), latest: newLatestCache()}
}

// panelItemKind is one line of a planned frame.
type panelItemKind int

const (
	// itemTabs, itemBaseline and itemStrip are the header's three lines.
	itemTabs panelItemKind = iota
	itemBaseline
	itemStrip
	// itemBlank is a blank line: the header's and the footer's gaps, and the
	// gap between two sections.
	itemBlank
	// itemHeading is a section title over its rows.
	itemHeading
	itemRow
	// itemText is one line of text under a row, painted (panelItem.text): a
	// band row's why, any other row's latest assistant text.
	itemText
	itemMeta
	itemNoSessions
	// itemConfigNote is the first problem in panel.conf, on its own line above
	// the footer.
	itemConfigNote
	itemFooter
	itemButton
	// itemKeys is the key strip: always the last line.
	itemKeys
)

// panelItem is a line before it is styled. Planning in items rather than in
// rendered strings is what lets settle count a layout at every detail without
// styling it each time.
type panelItem struct {
	kind panelItemKind
	sec  *panelSection
	row  *panelRow
	text string
}

// lines is the panel as rendered lines, at most p.height of them.
//
// The tabs are always the first line and the key strip the last: a panel
// with a height pads the gap above the key strip so it sits on the bottom row.
// Rows that do not fit scroll (scrolled): the band is placed first, so a
// sidebar at its top offset always shows what is waiting before anything else.
func (p panel) lines() []string {
	key := panelFrameKey{
		snap: p.snap, now: p.now, width: p.width, height: p.height,
		mode: p.mode, note: p.note, surface: p.surface,
		confGen: p.confGen(), dark: lipgloss.HasDarkBackground(), scroll: p.scroll, filter: p.filter, filtering: p.filtering,
		picking: p.picking, pickSection: p.pickSection, pickState: p.pickState, pickStateOn: p.pickStateOn,
		blurred: p.blurred, cursor: p.cursor, cursorAt: p.cursorAt,
	}
	if p.memo != nil && p.memo.lines != nil && p.memo.key == key {
		return p.memo.lines
	}
	out := p.compose()
	if p.memo != nil {
		p.memo.key, p.memo.lines = key, out
	}
	return out
}

func (p panel) compose() []string {
	if p.snap == nil {
		return p.waitingFrame()
	}
	f := p.frame()
	// Every row is rendered once, here, before settle counts layouts of it
	// (§5.1).
	p.renderRows(&f)
	s := p.settle(&f)
	p.commit(&f, s)
	nav := navRows(s.items, len(headerItems(s.detail)))
	cur := ""
	if i := p.cursorIndex(nav); i >= 0 && p.cursorShown() {
		cur = nav[i].key
	}
	if p.memo != nil {
		p.memo.scrollMax, p.memo.nav, p.memo.view = 0, nav, 0
		p.memo.bodyTop, p.memo.bodyOff = len(headerItems(s.detail)), 0
	}
	lines := p.paint(&f, s.items, cur)
	if s.scrolls {
		return p.scrolled(&f, s.detail, lines)
	}
	if s.detail.chrome == chromeMinimal {
		return panelClamp(lines, p.height) // no key strip to pin
	}
	return p.pin(lines)
}

// scrolled is a frame whose rows do not fit even at the band's floor: the
// header and the footer stay where they are, the rows between them show a
// window of p.height lines' worth from the scroll offset, and the tabs line
// carries the position in its top-right corner. At offset 0 the band is the
// first thing in the window (it is placed first), so nothing that needs you
// is hidden until the reader scrolls.
func (p panel) scrolled(f *panelFrame, d panelDetail, lines []string) []string {
	head, tail := len(headerItems(d)), len(p.appendTail(nil, d))
	body := lines[head : len(lines)-tail]
	header, footer := lines[:head], lines[len(lines)-tail:]
	keys := tail > 0 && d.chrome != chromeMinimal
	// Too short for a window under the whole chrome: the header gives up all
	// but the tabs, then the key strip goes. The notes and the start button
	// under the rows stay: on a panel this short they are what matters.
	if p.height-len(header)-len(footer) < 1 {
		header = header[:1]
	}
	if p.height-len(header)-len(footer) < 1 && keys {
		footer, keys = footer[:len(footer)-1], false
	}
	view := p.height - len(header) - len(footer)
	if view < 1 || len(body) <= view {
		return panelClamp(append(append(append([]string{}, header...), body...), footer...), p.height)
	}
	most := len(body) - view
	off := min(max(p.scroll, 0), most)
	if p.memo != nil {
		p.memo.scrollMax, p.memo.view = most, view
		p.memo.bodyTop, p.memo.bodyOff = len(header), off
	}
	out := make([]string, 0, p.height)
	out = append(out, header...)
	out = append(out, body[off:off+view]...)
	out = append(out, footer...)

	// The gaps around the window say there is more past it, each hint right
	// against the rows it continues: "▼ 14 more" in the first blank line
	// under them, the gap then between it and the key strip, and "▲ 6 more"
	// in the last blank line over them (the user, 2026-10-20: "here we have
	// the gap and no indication that we can scroll further").
	if n := p.rowsOutside(off, view, false); n > 0 {
		if i := firstBlank(out[len(out)-len(footer):]); i >= 0 {
			// Right under the last drawn line: a window that ends on the blank
			// before a section title would otherwise leave a gap above it.
			at := len(out) - len(footer) + i
			for at > len(header) && out[at-1] == "" {
				at--
			}
			out[at] = p.moreHint("▼", n)
		}
	}
	if n := p.rowsOutside(off, view, true); n > 0 {
		if i := lastBlank(out[:len(header)]); i >= 0 {
			out[i] = p.moreHint("▲", n)
		}
	}

	status := scrollStatus(off, view, len(body))
	tabs, start, end := p.tabsLineBeside(f.mode, status)
	out[0] = tabs
	if len(header) > 1 {
		out[1] = p.baseline(start, end)
	}
	if keys {
		out[len(out)-1] = p.footerLine(true)
	}
	return out
}

// rowsOutside counts the sessions wholly above (above) or below the scroll
// window [off, off+view) of the body.
func (p panel) rowsOutside(off, view int, above bool) int {
	if p.memo == nil {
		return 0
	}
	n := 0
	for _, r := range p.memo.nav {
		if above && r.first < off || !above && r.first >= off+view {
			n++
		}
	}
	return n
}

// firstBlank is the index of the first empty line in lines, -1 when none is.
func firstBlank(lines []string) int {
	for i, l := range lines {
		if l == "" {
			return i
		}
	}
	return -1
}

// lastBlank is the index of the last empty line in lines, -1 when none is.
func lastBlank(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "" {
			return i
		}
	}
	return -1
}

// moreHint is a gap line saying how many sessions lie past the window: the
// arrow and the count, dim, centred.
func (p panel) moreHint(arrow string, n int) string {
	text := arrow + " " + strconv.Itoa(n) + " more"
	pad := max((p.width-ansi.StringWidth(text))/2, 0)
	return strings.Repeat(" ", pad) + panelMetaStyle.Render(text)
}

// scrollStatus is the scroll area's position: ▲ when there are rows above the
// window, ▼ when there are rows below it, and how much of the list has been
// seen down to the window's last row ("▲▼ 45%", "▼ 38%", "▲ 100%").
func scrollStatus(off, view, total int) string {
	up, down := " ", " "
	if off > 0 {
		up = "▲"
	}
	if off+view < total {
		down = "▼"
	}
	return fmt.Sprintf("%s%s %3d%%", up, down, (off+view)*100/total)
}

// waitingFrame is the panel before its first snapshot: the tabs, what it is
// waiting for, any note (and the start button when there is no observer to
// attach to), and the key strip on the bottom row. The blank lines go first
// when the panel is short.
func (p panel) waitingFrame() []string {
	tabs, start, end := p.tabsLine(p.mode)
	head := []string{tabs, p.baseline(start, end)}
	body := []string{panelMetaStyle.Render(panelTruncate("waiting for the observer…", p.width))}
	if note := p.configNote(); note != "" {
		body = append(body, panelNoteStyle.Render(panelTruncate(note, p.width)))
	}
	if note := p.shownNote(); note != "" {
		body = append(body, panelNoteStyle.Render(panelTruncate(note, p.width)))
	}
	if p.offline() {
		body = append(body, p.startButton())
	}
	top, bottom := 1, footerGap
	if p.height > 0 && len(head)+top+len(body)+bottom+footerLines > p.height {
		top, bottom = 0, 0
	}
	out := append(head, make([]string, top)...)
	out = append(out, body...)
	out = append(out, make([]string, bottom)...)
	return p.pin(append(out, p.footerLine(false)))
}

// pin fits a painted frame to the panel's height with the key strip — its
// last line — on the bottom row: a short frame is padded with blank lines
// above it. A frame taller than the height (only at heights too small for the
// header and the band) keeps its top lines, as it always has.
func (p panel) pin(lines []string) []string {
	if p.height <= 0 || len(lines) == 0 {
		return lines
	}
	if len(lines) > p.height {
		return lines[:p.height]
	}
	pad := make([]string, p.height-len(lines))
	last := len(lines) - 1
	out := make([]string, 0, p.height)
	out = append(out, lines[:last]...)
	out = append(out, pad...)
	return append(out, lines[last])
}

// frame builds this frame's sections, from the sessions the filter keeps. The
// header's counts are always the whole fleet's.
func (p panel) frame() panelFrame {
	// Ended sessions are not drawn (the user, 2026-10-01): a session that
	// finished, or whose process or pane is gone, has nothing left to show.
	var all []observer.Session
	for _, s := range observer.AsOf(p.snap, p.now, p.policy).Sessions {
		if panelGroupOf(s) != groupEnded {
			all = append(all, s)
		}
	}
	sessions := filterSessions(all, p.filter)
	f := panelFrame{mode: p.mode}
	if p.mode == modeStatus {
		f.secs = statusSections(sessions, p.now)
	} else {
		f.secs = p.groupedSections(p.mode, sessions)
	}
	f.secs = p.pickFilters(f.secs)
	for _, s := range f.secs {
		if len(s.rows) > 0 {
			f.hasLive = true
		}
	}
	f.counts = fleetCounts(all)
	return f
}

// headerItems are the header's lines at detail d.
func headerItems(d panelDetail) []panelItem {
	if d.chrome == chromeMinimal {
		return []panelItem{{kind: itemTabs}}
	}
	items := []panelItem{{kind: itemTabs}, {kind: itemBaseline}, {kind: itemStrip}}
	if d.chrome == chromeFull {
		for range headerGap {
			items = append(items, panelItem{kind: itemBlank})
		}
	}
	return items
}

// chromeLines is how many lines the header and the footer take at detail d.
func chromeLines(d panelDetail) int {
	switch d.chrome {
	case chromeMinimal:
		return 1
	case chromeCompact:
		return headerLines + footerLines
	}
	return headerLines + headerGap + footerGap + footerLines
}

// plan is the whole frame at detail d: every row of every section.
func (p panel) plan(f *panelFrame, d panelDetail) []panelItem {
	items := headerItems(d)
	first := true
	// gap is the blank line before a section's title — never before the
	// first, which the header's gap already opens. It is part of the view,
	// not of the ladder (the user, 2026-10-01: "one line gap between groups"):
	// it goes only with the rest of the chrome's blank lines, when the band
	// alone would not fit otherwise.
	gap := func() {
		if d.chrome == chromeFull && !first {
			items = append(items, panelItem{kind: itemBlank})
		}
		first = false
	}
	for i := range f.secs {
		sec := &f.secs[i]
		if len(sec.rows) == 0 {
			continue
		}
		gap()
		items = append(items, panelItem{kind: itemHeading, sec: sec})
		for j := range sec.rows {
			items = append(items, rowItems(&sec.rows[j], d)...)
		}
	}
	if !f.hasLive {
		items = append(items, panelItem{kind: itemNoSessions})
	}
	return p.appendTail(items, d)
}

// appendTail is the lines a frame keeps below its rows: the config note, the
// footer note and the start button when there are ones, then the footer's gap
// and the key strip.
func (p panel) appendTail(items []panelItem, d panelDetail) []panelItem {
	if p.configNote() != "" {
		items = append(items, panelItem{kind: itemConfigNote})
	}
	if note := p.footerNote(); note != "" {
		items = append(items, panelItem{kind: itemFooter, text: note})
	}
	if p.offline() {
		items = append(items, panelItem{kind: itemButton})
	}
	switch d.chrome {
	case chromeMinimal:
		return items
	case chromeFull:
		for range footerGap {
			items = append(items, panelItem{kind: itemBlank})
		}
	}
	return append(items, panelItem{kind: itemKeys})
}

// onCursor is a row's line, on the cursor fill when the row is cur's.
func (p panel) onCursor(it panelItem, cur, line string) string {
	if cur == "" || it.row == nil || panelSessionKey(it.row.s) != cur {
		return line
	}
	return p.cursorLine(line, it.kind == itemRow)
}

// reserveLines is the lines a frame keeps below its rows, besides the
// footer: the config note and the footer note when there are ones.
func (p panel) reserveLines() int {
	n := 0
	if p.configNote() != "" {
		n++
	}
	if p.footerNote() != "" {
		n++
	}
	if p.offline() {
		// The button is never the line that gives way: a panel with no
		// observer has nothing else worth the row.
		n++
	}
	return n
}

// attentionFits reports whether the header, the band's title and every band
// row at detail d fit with the reserve and the footer below them.
func (p panel) attentionFits(f *panelFrame, d panelDetail) bool {
	need := chromeLines(d) + p.reserveLines()
	for i := range f.secs {
		sec := &f.secs[i]
		if !sec.band || len(sec.rows) == 0 {
			continue
		}
		need++
		for j := range sec.rows {
			need += len(rowItems(&sec.rows[j], d))
		}
	}
	return need <= p.height
}

// paint styles a planned frame, the lines of the row whose session is cur
// on the cursor fill.
func (p panel) paint(f *panelFrame, items []panelItem, cur string) []string {
	out := make([]string, 0, len(items))
	tabStart, tabEnd := 0, 0
	for _, it := range items {
		switch it.kind {
		case itemTabs:
			var line string
			line, tabStart, tabEnd = p.tabsLine(f.mode)
			out = append(out, line)
		case itemBaseline:
			out = append(out, p.baseline(tabStart, tabEnd))
		case itemStrip:
			out = append(out, p.countStrip(f.counts))
		case itemBlank:
			out = append(out, "")
		case itemHeading:
			out = append(out, p.sectionTitle(it.sec))
		case itemRow:
			out = append(out, p.onCursor(it, cur, it.row.lines.head))
		case itemText:
			out = append(out, p.onCursor(it, cur, it.text))
		case itemMeta:
			out = append(out, p.onCursor(it, cur, it.row.lines.meta))
		case itemNoSessions:
			what := "no sessions"
			if p.filter != "" {
				what = "no session matches /" + sanitizeText(p.filter)
			}
			out = append(out, panelMetaStyle.Render(panelTruncate(what, p.width)))
		case itemConfigNote:
			out = append(out, panelNoteStyle.Render(panelTruncate(p.configNote(), p.width)))
		case itemFooter:
			out = append(out, panelNoteStyle.Render(panelTruncate(it.text, p.width)))
		case itemButton:
			out = append(out, p.startButton())
		case itemKeys:
			out = append(out, p.footerLine(false))
		}
	}
	return out
}
