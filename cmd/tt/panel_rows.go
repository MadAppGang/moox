package main

// panel_rows.go pre-renders every row of a frame from the panel.conf
// templates (architecture §5.1), and decides how many of those lines a frame
// draws (§5.2-5.4).
//
// A frame is laid out dozens of times over (settle counts one layout
// after another), so the rows are rendered ONCE per frame, at the
// panel's width, into panelRow.lines — and the layout only COUNTS the lines a
// row has. Line counts are therefore the real template line counts, never an
// assumed two.
//
// It also holds the detail LADDER (§5.2) and settle (§5.3), which picks the
// detail a frame is drawn at.

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/panelfmt"
	"github.com/MadAppGang/moox/internal/projectkey"
)

// panelDetail is how much of each row a layout draws. settle tries the
// ladder from the richest rung down and keeps the first that fits.
type panelDetail struct {
	// liveMeta is the meta line of every row outside the band (an opt-in
	// template since revision 4; the built-in meta is empty).
	liveMeta bool
	// live is how many lines of latest text a row outside the band draws:
	// 3, 2, 1 or 0 (FR9). They give way one line at a time, every row at
	// once, before the band shortens or the list scrolls.
	live int
	// attn is how many lines a band row may use: 4, 3, 2 or 1 — its head and
	// then its text lines (why it waits, re-wrapped to the lines it has), and
	// its meta line only when a line is left over:
	//
	//	4  head · 3 lines of why
	//	3  head · 2 lines of why
	//	2  head · 1 line of why (its meta when it has no why)
	//	1  head
	//
	// A line the row has nothing for is not drawn.
	attn int
	// chrome is how much of the header and the footer is drawn. Anything
	// but chromeFull is a last resort, taken only when the band alone would
	// not fit otherwise.
	chrome chromeLevel
}

// chromeLevel is how much of the whole view's header and footer a frame draws.
type chromeLevel int

const (
	// chromeFull is the header (tabs, baseline, count strip, two blank
	// lines) and the footer (two blank lines, the key strip).
	chromeFull chromeLevel = iota
	// chromeCompact drops the four blank lines.
	chromeCompact
	// chromeMinimal is the tabs alone: no baseline, no strip, no key strip.
	chromeMinimal
)

// The ladder. detailFull is everything; detailAttention is rung 4 with no
// section gaps, no meta and no latest text outside the band; detailNames is
// one line a row, for when the band alone overflows at panelBandFloor;
// detailCompact is that without the chrome's blank lines, and detailMinimal
// that with the tabs alone above and nothing below.
var (
	detailFull      = panelDetail{liveMeta: true, live: panelTextLines, attn: panelBandTallest}
	detailAttention = panelDetail{attn: panelBandTallest}
	detailAttn3     = panelDetail{attn: 3}
	detailAttn2     = panelDetail{attn: 2}
	detailNames     = panelDetail{attn: 1}
	detailCompact   = panelDetail{attn: 1, chrome: chromeCompact}
	detailMinimal   = panelDetail{attn: 1, chrome: chromeMinimal}
)

// panelBandTallest is the most lines a band row is drawn on (§5.2).
const panelBandTallest = 4

// panelBandFloor is FR3 as decided at Gate 2 (2026-09-25, "keep the WHY
// line"): band rows keep head + WHY while any non-band row can still be cut.
// A policy knob, not a tuning knob — changing it changes FR3. Below it the
// ladder stops shrinking the band and cuts the rows outside it instead; the
// band goes to one line a row only when it alone does not fit at this rung.
const panelBandFloor = 2

// rungDetails are the details tried at a rung, richest first: rung 4 keeps
// the meta lines and three lines of latest text outside the band if it can,
// then gives up the meta, then the text a line at
// a time (3, 2, 1, 0); every rung below has already given them all up (§5.3,
// the order of giving way).
func rungDetails(rung int) []panelDetail {
	if rung < panelBandTallest {
		return []panelDetail{{attn: rung}}
	}
	out := []panelDetail{detailFull}
	for live := panelTextLines; live >= 0; live-- {
		out = append(out, panelDetail{live: live, attn: panelBandTallest})
	}
	return out
}

// rowItems is one row at detail d: its head and the lines under it that d
// keeps. It COUNTS the pre-rendered lines (§5.1) and renders nothing.
//
// A row outside the band keeps d.live lines of its latest text, and its meta
// line only while d keeps meta lines. A band row keeps what its rung allows
// (panelDetail.attn), because its lines say why it waits on you: its text
// first, its meta only on a line the text leaves over.
func rowItems(r *panelRow, d panelDetail) []panelItem {
	out := []panelItem{{kind: itemRow, row: r}}
	text := func(n int) int {
		lines := r.lines.text[min(max(n, 0), panelTextLines)]
		for _, l := range lines {
			out = append(out, panelItem{kind: itemText, row: r, text: l})
		}
		return len(lines)
	}
	if !r.band {
		text(d.live)
		if d.liveMeta && r.lines.meta != "" {
			out = append(out, panelItem{kind: itemMeta, row: r})
		}
		return out
	}
	room := d.attn - 1
	if room > text(room) && r.lines.meta != "" {
		out = append(out, panelItem{kind: itemMeta, row: r})
	}
	return out
}

// panelSettlement is what settle decided: the detail and the planned frame.
type panelSettlement struct {
	detail panelDetail
	items  []panelItem
	// scrolls is set when no rung fits: items is every row, and the frame
	// scrolls (panel_layout.go: scrolled).
	scrolls bool
}

// settle chooses the frame's detail (§5.3). Nothing is folded any more — the
// user, 2026-09-29: "we should not fold groups like that" — so every group
// always shows its rows, and what gives way is only how much is drawn under
// them: the meta lines, then the latest text 3 → 2 → 1 → 0,
// then the band 4 → 3 → 2 lines a row. When even that does not fit, the list
// scrolls at the band's floor (panelBandFloor), and the band goes to one line
// a row, then the chrome to its bare tabs, only if it alone would not fit in
// the window.
//
// Band-level hysteresis (§5.4): a rung RICHER than the previous frame's is
// taken only if it fits with panelRungSlack lines to spare, so a band that
// grows and shrinks by a row does not flip between rungs every frame. An equal
// or poorer rung is taken as soon as it fits. The first frame after a layout
// reset has no previous rung (bandAttn 0) and takes the richest that fits.
func (p panel) settle(f *panelFrame) panelSettlement {
	if p.height <= 0 {
		// Unbounded: nothing gives way.
		return panelSettlement{detail: detailFull, items: p.plan(f, detailFull)}
	}
	prevAttn := p.carried(f)
	for rung := panelBandTallest; rung >= panelBandFloor; rung-- {
		slack := 0
		// The floor rung never waits for slack: below it the list scrolls,
		// and hysteresis must never scroll a frame that fits.
		if prevAttn != 0 && rung > prevAttn && rung > panelBandFloor {
			slack = panelRungSlack
		}
		for _, d := range rungDetails(rung) {
			if items := p.plan(f, d); len(items) <= p.height-slack {
				return panelSettlement{detail: d, items: items}
			}
		}
	}
	d := detailAttn2
	for _, poorer := range []panelDetail{detailNames, detailCompact, detailMinimal} {
		if p.attentionFits(f, d) {
			break
		}
		d = poorer
	}
	return panelSettlement{detail: d, items: p.plan(f, d), scrolls: true}
}

// namedRows is how many sessions a planned frame draws a row for.
func namedRows(items []panelItem) int {
	n := 0
	for _, it := range items {
		if it.kind == itemRow {
			n++
		}
	}
	return n
}

// carried is the band rung the previous frame left for this one, or 0 when
// the layout it was drawn for — size, mode, ENDED toggle — has changed since.
func (p panel) carried(f *panelFrame) int {
	if p.memo == nil || p.memo.rungFor != p.layoutKey(f) {
		return 0
	}
	return p.memo.bandAttn
}

// layoutKey is what the carried rung is valid for.
func (p panel) layoutKey(f *panelFrame) panelLayoutKey {
	return panelLayoutKey{width: p.width, height: p.height, mode: f.mode}
}

// commit writes the rung the next frame starts from.
func (p panel) commit(f *panelFrame, s panelSettlement) {
	if p.memo == nil || p.height <= 0 {
		return
	}
	p.memo.rungFor, p.memo.bandAttn = p.layoutKey(f), s.detail.attn
}

// rowLines is one row's candidate lines, rendered at the panel's width.
type rowLines struct {
	head string // row / attn_row: never empty
	meta string // meta / attn_meta; "" = the row has no such line
	// text[n] is the row's text lines when it may draw n of them (0..3),
	// painted with the gutter and the indent: a band row's why re-wrapped to
	// n lines, any other row the last n lines of its latest text.
	text [panelTextLines + 1][]string
}

// renderRows renders every row this frame can draw. The ENDED rows are
// rendered only when they are shown; hidden, they are only counted.
func (p panel) renderRows(f *panelFrame) {
	set := p.confSet()
	rc := p.rowContext(set, f)
	p.latest().sweep()
	for i := range f.secs {
		for j := range f.secs[i].rows {
			r := &f.secs[i].rows[j]
			r.lines = p.renderRow(set, r, rc)
		}
	}
}

// rowContext is what every row of this frame shares.
func (p panel) rowContext(set *panelfmt.Set, f *panelFrame) *rowContext {
	return &rowContext{now: p.now, ctx: set.Ctx, icons: set.Icons, window: set.Window, project: p.projectLabels(f)}
}

// projectLabels resolves the frame's project labels the first time a row asks
// for one, the way project mode labels its groups: every folder in the frame
// is resolved together, so two repositories with one name are told apart.
func (p panel) projectLabels(f *panelFrame) func(cwd string) string {
	var byCWD map[string]string
	return func(cwd string) string {
		if byCWD == nil {
			resolver := p.resolver()
			projects := map[string]projectkey.Project{}
			var list []projectkey.Project
			visit := func(rows []panelRow) {
				for _, r := range rows {
					if _, done := projects[r.s.CWD]; !done {
						projects[r.s.CWD] = resolver.Resolve(r.s.CWD)
						list = append(list, projects[r.s.CWD])
					}
				}
			}
			if f != nil {
				for i := range f.secs {
					visit(f.secs[i].rows)
				}
			}
			if _, done := projects[cwd]; !done {
				projects[cwd] = resolver.Resolve(cwd)
				list = append(list, projects[cwd])
			}
			labels := projectkey.Labels(list)
			byCWD = make(map[string]string, len(projects))
			for dir, pr := range projects {
				if pr.Key != "" {
					byCWD[dir] = labels[pr.Key]
				}
			}
		}
		return byCWD[cwd]
	}
}

// renderRow is one row's lines. A band row draws from the attn_* templates,
// every other row from row and meta (R5: the band chooses the templates; the
// chip's loudness follows the session's attention on every row either way).
func (p panel) renderRow(set *panelfmt.Set, r *panelRow, rc *rowContext) rowLines {
	res := rowResolver(r, rc)
	w := max(p.width, 0)
	head, meta, builtin := set.Row, set.Meta, panelDefaultSet.Row
	if r.band {
		head, meta, builtin = set.AttnRow, set.AttnMeta, panelDefaultSet.AttnRow
	}
	var out rowLines
	out.head = headLine(head, builtin, w, res, rowName(r.s))
	if meta != nil {
		out.meta, _ = meta.Line(w, res)
		// The row's /color bar runs down through every line under it: a meta
		// line that opens with a blank cell (the built-in indent) gets the
		// gutter there, so the bar does not break under the text above it.
		if h := sessionHue(panelAgentColor(r.s)); h.known() && strings.HasPrefix(ansi.Strip(out.meta), " ") {
			bar := gutterCell(h, gutterGlyph).Parts[0]
			out.meta = bar.Style.Render(bar.Text) + ansi.Cut(out.meta, 1, ansi.StringWidth(out.meta))
		}
	}
	limit := w - 1 - panelTextIndent
	most := min(max(set.TextLines, 0), panelTextLines) // panel.conf's text_lines
	if limit < 1 || most == 0 {
		return out
	}
	lead := gutterCell(sessionHue(panelAgentColor(r.s)), gutterGlyph).Parts[0]
	prefix := lead.Style.Render(lead.Text) + strings.Repeat(" ", panelTextIndent)
	paint := func(lines []string) []string {
		for i, l := range lines {
			lines[i] = prefix + latestStyle.Render(l)
		}
		return lines
	}
	if r.band || (r.s.Attention && whyText(r.s) != "") {
		// Why a session that needs you waits — in the band, or an Attention
		// row kept in its home group (R5) that has a why to say: attn_why,
		// drawn as plain dim text and re-wrapped for each line count, so two
		// lines are the question's start rather than its first line cut.
		if set.AttnWhy == nil {
			return out
		}
		for n := 1; n <= panelTextLines; n++ {
			if n > most {
				out.text[n] = out.text[most]
				continue
			}
			line, ok := set.AttnWhy.Line(limit*n, res)
			if !ok {
				break
			}
			out.text[n] = paint(firstLines(sanitizeText(ansi.Strip(line)), limit, n))
		}
		return out
	}
	all := lastLines(p.rowLatest(r), limit, most)
	for n := 1; n <= panelTextLines; n++ {
		out.text[n] = paint(append([]string(nil), all[max(len(all)-n, 0):]...))
	}
	return out
}

// rowLatest is the text drawn under a row outside the band: the session's
// latest assistant text (panel_latest.go), and for a row whose liveness is
// unknown and that has none, why it is unclear — "live? transcript-absent" —
// so an UNCLEAR row still explains itself (§5.5a).
func (p panel) rowLatest(r *panelRow) string {
	if r.s.Transcript != nil {
		if text := p.latest().text(r.s.Transcript.Path); text != "" {
			return text
		}
	}
	if r.s.Live == nil {
		return liveMarker(r.s)
	}
	return ""
}

// latest is the panel's latest-text cache: the memo's, or a fresh one for a
// panel with no memo (a test's), which then reads on every call.
func (p panel) latest() *latestCache {
	if p.memo != nil {
		if p.memo.latest == nil {
			p.memo.latest = newLatestCache()
		}
		return p.memo.latest
	}
	return newLatestCache()
}

// headLine is a row's first line. A user's row template whose every token has
// nothing to say for this session (row = "{kind}" on an interactive one)
// would leave the session with no line at all — a row that silently vanishes
// — so the built-in template draws it instead. The built-in one always draws
// (its gutter and chip are never Empty; TestTheBuiltInHeadTemplatesAlwaysDraw
// pins it), and should that ever break, the row is drawn as its bare name
// rather than as an empty line.
func headLine(tpl, builtin *panelfmt.Template, w int, res panelfmt.Resolver, name string) string {
	if line, ok := tpl.Line(w, res); ok {
		return line
	}
	if line, ok := builtin.Line(w, res); ok {
		return line
	}
	return panelTruncate(name, w)
}
