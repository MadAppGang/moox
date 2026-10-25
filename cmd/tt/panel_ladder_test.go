package main

// The tall band (architecture §5.2-5.5, §8): the ladder a band row gives way
// on, settle choosing the rung and the collapse state together (R4), the
// band-level hysteresis and its "no previous frame" sentinel (#22), and the
// heading's order of giving way at narrow widths (#32).

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// tallBandNeeds wraps to three lines at the sidebar's width (after
// "waiting …  · "), so a band row with it is four lines tall at rung 4 (head,
// why ×3), three at rung 3, two at rung 2 and one at rung 1: every rung draws
// a different band.
const tallBandNeeds = "confirm the migration plan before it touches the prod replica (step 4 of 6)"

// tallBandFleet is newFleet with a long needs on every background band row.
func tallBandFleet(t *testing.T) fleet {
	t.Helper()
	f := newFleet(t, fleetNow)
	snap := *f.snap
	snap.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
	n := 0
	for i := range snap.Sessions {
		s := &snap.Sessions[i]
		if s.Kind == observer.KindBackground && panelGroupOf(*s) == groupNeedsYou {
			s.Needs = ptr(tallBandNeeds)
			// A model, as the golden fleet's band rows have.
			facts := observer.TranscriptFacts{Model: "claude-opus-5-5"}
			if s.Transcript != nil {
				facts.AgentColor, facts.ColorFound = s.Transcript.AgentColor, s.Transcript.ColorFound
			}
			s.Transcript = &facts
			n++
		}
	}
	if n != 3 {
		t.Fatalf("the fleet has %d background band rows, want 3", n)
	}
	f.snap = &snap
	return f
}

// bandNames is the names of the sessions a frame draws in NEEDS YOU.
func bandNames(snap *observer.Snapshot) []string {
	var out []string
	for _, s := range snap.Sessions {
		if panelGroupOf(s) == groupNeedsYou {
			out = append(out, s.Name)
		}
	}
	return out
}

// shown reports whether a frame draws the session's name.
func shown(frame, name string) bool {
	return regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(name) + `\s`).MatchString(frame + "\n")
}

// settledFrame is a panel's frame, rows rendered, as settle sees it.
func settledFrame(p *panel) panelFrame {
	f := p.frame()
	p.renderRows(&f)
	return f
}

// The ladder, stepped from a tall sidebar down to a tiny one in every mode:
// the band gives way 4 → 3 → 2 and climbs back as the height falls only where
// the poorer rung would name no more rows (a fold both rungs must make); no
// row outside the band is cut while the band is above panelBandFloor; the
// band goes to one line a row only when it alone does not fit at the floor;
// and every band row is on screen at every height, drawn at its rung.
func TestTheBandGivesWayFourThreeTwoBeforeRowsAreCutAndOneLineLast(t *testing.T) {
	// FR3 as decided at Gate 2: the floor is 2, and this test is pinned to it.
	if panelBandFloor != 2 {
		t.Fatalf("panelBandFloor = %d; FR3 (Gate 2) decided 2 — changing it changes FR3", panelBandFloor)
	}
	f := tallBandFleet(t)
	band := bandNames(f.snap)
	seen := map[int]bool{}
	climbs := 0
	for _, mode := range panelModes {
		last := map[panelMode]int{} // by the mode actually drawn: below its floor a mode draws status
		for h := 72; h >= 10; h-- {
			p := sidebar(t, f.now, f.snap, mode, h)
			lines := plain(p.view())
			frame := strings.Join(lines, "\n")
			attn := p.memo.bandAttn
			fr := settledFrame(p)
			label := fmt.Sprintf("%s mode at %d lines (drawn as %s, rung %d)", mode, h, fr.mode, attn)
			seen[attn] = true

			// Nothing folds, so nothing a shorter panel gives up can make room
			// for a richer band: the band only gives way as the panel shrinks.
			if prev, ok := last[fr.mode]; ok && attn > prev {
				climbs++
				t.Errorf("%s: the band grew from rung %d as the panel got shorter", label, prev)
			}
			last[fr.mode] = attn
			if strings.Contains(lines[0], "▼") && attn > panelBandFloor {
				t.Errorf("%s: the list scrolls while the band was above its floor:\n%s", label, frame)
			}
			if attn < panelBandFloor && p.attentionFits(&fr, detailAttn2) {
				t.Errorf("%s: the band went to one line a row though it fits at %d:\n%s", label, panelBandFloor, frame)
			}

			// Every band row is on screen, drawn at the rung: the background
			// rows carry a three-line why, so each rung paints a different
			// number of lines under them.
			fl := parseFrame(lines)
			wantUnder := map[int]int{4: 3, 3: 2, 2: 1, 1: 0}[attn]
			for _, name := range band {
				under, drawn := bandLines(fl, name)
				if !drawn {
					t.Errorf("%s: band row %s is not on screen:\n%s", label, name, frame)
					continue
				}
				if name == "dotfiles-02" && len(under) != wantUnder {
					t.Errorf("%s: %s has %d lines under it %q, want %d at this rung", label, name, len(under), under, wantUnder)
				}
			}
		}
	}
	for _, rung := range []int{4, 3, 2, 1} {
		if !seen[rung] {
			t.Errorf("no height drew the band at rung %d, so the ladder is not exercised end to end", rung)
		}
	}
	if climbs > 0 {
		t.Logf("%d heights drew a richer band than the height above", climbs)
	}
}

// Each rung's lines, from one band row, in the order the ladder gives them
// up: the why re-wrapped to fewer lines (its start kept, "…" where it was
// cut), the meta only on a line the why leaves over. Outside the band a row
// keeps the LAST lines of its latest text, three, two, one, none.
func TestABandRowDrawsWhatItsRungAllows(t *testing.T) {
	r := panelRow{band: true, lines: rowLines{head: "H", meta: "M", text: [panelTextLines + 1][]string{
		nil, {"W…"}, {"W1", "W2…"}, {"W1", "W2", "W3"},
	}}}
	noWhy := panelRow{band: true, lines: rowLines{head: "H", meta: "M"}}
	oneWhy := panelRow{band: true, lines: rowLines{head: "H", meta: "M", text: [panelTextLines + 1][]string{
		nil, {"W"}, {"W"}, {"W"},
	}}}
	quiet := panelRow{lines: rowLines{head: "H", meta: "M", text: [panelTextLines + 1][]string{
		nil, {"L3"}, {"L2", "L3"}, {"L1", "L2", "L3"},
	}}}
	draw := func(r panelRow, d panelDetail) string {
		var out []string
		for _, it := range rowItems(&r, d) {
			switch it.kind {
			case itemRow:
				out = append(out, r.lines.head)
			case itemMeta:
				out = append(out, r.lines.meta)
			case itemText:
				out = append(out, it.text)
			}
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		row  panelRow
		d    panelDetail
		want string
	}{
		{r, detailFull, "H W1 W2 W3"},
		{r, detailAttention, "H W1 W2 W3"},
		{r, detailAttn3, "H W1 W2…"},
		{r, detailAttn2, "H W…"},
		{r, detailNames, "H"},
		{noWhy, detailAttention, "H M"},
		{noWhy, detailAttn3, "H M"},
		{noWhy, detailAttn2, "H M"},
		{noWhy, detailNames, "H"},
		{oneWhy, detailAttention, "H W M"},
		{oneWhy, detailAttn3, "H W M"},
		{oneWhy, detailAttn2, "H W"},
		// Outside the band the meta line goes first, then the text a line at
		// a time from its start, so what is left is what was said last.
		{quiet, detailFull, "H L1 L2 L3 M"},
		{quiet, panelDetail{live: 3, attn: 4}, "H L1 L2 L3"},
		{quiet, panelDetail{live: 2, attn: 4}, "H L2 L3"},
		{quiet, panelDetail{live: 1, attn: 4}, "H L3"},
		{quiet, detailAttention, "H"},
		{quiet, detailAttn2, "H"},
	} {
		if got := draw(c.row, c.d); got != c.want {
			t.Errorf("%+v at %+v draws %q, want %q", c.row.lines, c.d, got, c.want)
		}
	}
}

// The text gives way before the band and before the list scrolls: no frame
// draws a line of latest text under a row outside the band while its band is
// below rung 4 or its rows scroll — in any mode, at any height, with or
// without ENDED shown (FR9: the text gives way 3 → 2 → 1 → 0 first). No frame
// folds a group at all. And the text goes a line at a time: every row outside
// the band draws the same number of lines, so a frame with one line of text
// under one row has it under every row that has text.
func TestTextLinesGiveWayBeforeTheBandAndTheScroll(t *testing.T) {
	f := tallBandFleet(t)
	// Text on every fourth session: enough that frames of every depth fit in
	// the heights below.
	withLatestText(t, &f, func(i int) bool { return i%4 == 0 })
	withText, perLine := 0, map[int]int{}
	// What each row carries — its meta and how many text lines it has at
	// most — depends on the mode, not on the height: read once per mode.
	type carried struct {
		meta  string
		lines int
	}
	rows := map[panelMode]map[string]carried{}
	for _, mode := range panelModes {
		p := sidebar(t, f.now, f.snap, mode, 0)
		rows[mode] = map[string]carried{}
		for _, s := range f.snap.Sessions {
			if panelGroupOf(s) == groupEnded {
				continue // not drawn
			}
			_, meta, all, _ := renderedRow(t, p, s.Name)
			rows[mode][s.Name] = carried{strings.TrimSpace(meta), len(all)}
		}
	}
	for _, mode := range panelModes {
		for h := 20; h <= 140; h++ {
			p := sidebar(t, f.now, f.snap, mode, h)
			lines := plain(p.view())
			under := textUnderCalmRows(parseFrame(lines), func(name string) string { return rows[mode][name].meta })
			if len(under) == 0 {
				continue
			}
			withText++
			label := fmt.Sprintf("%s mode, %d lines", mode, h)
			if n := summarisedLines(lines); n > 0 {
				t.Errorf("%s: %d group(s) folded:\n%s", label, n, strings.Join(lines, "\n"))
			}
			if p.memo.bandAttn < panelBandTallest || strings.Contains(lines[0], "▼") {
				t.Errorf("%s: latest text is drawn while the band is at rung %d or the list scrolls:\n%s", label, p.memo.bandAttn, strings.Join(lines, "\n"))
			}
			depth := maxDepth(under)
			for name, n := range under {
				// A row draws `depth` lines, or every line it has when it
				// has fewer (an UNCLEAR row's one "live? …" line).
				if want := min(depth, rows[mode][name].lines); n != want {
					t.Errorf("%s: %s draws %d lines of text, want %d:\n%s", label, name, n, want, strings.Join(lines, "\n"))
				}
			}
			perLine[depth]++
		}
	}

	for n := 1; n <= panelTextLines; n++ {
		if perLine[n] == 0 {
			t.Errorf("no frame drew %d line(s) of latest text a row (%v), so the ladder's text steps are not all exercised", n, perLine)
		}
	}
	if withText == 0 {
		t.Fatal("no frame drew latest text, so this proves nothing")
	}
}

// textUnderCalmRows is, for every row outside the band that has latest text
// under it, how many lines of it it draws: its meta line (metaOf, drawn after
// the text) is not text.
func textUnderCalmRows(fl []frameLine, metaOf func(name string) string) map[string]int {
	out := map[string]int{}
	inBand := false
	for i, l := range fl {
		switch l.kind {
		case lineHeading:
			inBand = l.heading == "NEEDS YOU"
		case lineRow:
			if inBand {
				continue
			}
			n, meta := 0, metaOf(l.name)
			for _, m := range fl[i+1:] {
				if m.kind != lineIndented || (meta != "" && m.body == meta) {
					break
				}
				n++
			}
			if n > 0 {
				out[l.name] = n
			}
		}
	}
	return out
}

// maxDepth is the most lines of text any row draws.
func maxDepth(under map[string]int) int {
	n := 0
	for _, d := range under {
		n = max(n, d)
	}
	return n
}

// A frame is its own fixed point (review round 1, Issue 2): composing it again
// on the same input, its own collapse decisions and rung carried, draws the
// same lines. The first frame after a layout reset — every frame here is one —
// is therefore the settled frame, not an over-collapsed one that re-lays itself
// on the next update.
func TestTheFirstFrameAfterALayoutResetIsTheSettledFrame(t *testing.T) {
	checked, folded := 0, 0
	for _, fl := range []fleet{tallBandFleet(t), goldenFleet(t)} {
		snap := fl.snap
		for _, mode := range panelModes {
			for h := 20; h <= 90; h++ {
				p := sidebar(t, fl.now, snap, mode, h)
				first := p.view()
				// The same snapshot again, as a new event: the frame memo
				// is keyed on the pointer, so this recomposes.
				c := *snap
				deliver(t, p, snapshotEvent(&c))
				if second := p.view(); second != first {
					t.Errorf("%s mode, %d lines: the next update re-laid an unchanged frame:\n%s\n--- then\n%s",
						mode, h, strings.Join(plain(first), "\n"), strings.Join(plain(second), "\n"))
				}
				checked++
				if strings.Contains(plain(first)[0], "▼") {
					folded++
				}
			}
		}

	}
	t.Logf("%d frames recomposed, %d of them scrolling", checked, folded)
	if folded == 0 {
		t.Fatal("no frame scrolled, so the scrolling path is not exercised")
	}
}

// bandHeight is how many lines a painted frame gives NEEDS YOU: its heading,
// its rows and the lines under them.
func bandHeight(lines []string) int {
	fl := parseFrame(lines)
	for i, l := range fl {
		if l.kind != lineHeading || l.heading != "NEEDS YOU" {
			continue
		}
		n := 1
		for _, m := range fl[i+1:] {
			if m.kind != lineRow && m.kind != lineIndented {
				break
			}
			n++
		}
		return n
	}
	return 0
}

// A frame that grows and shrinks by one idle row flips the band's rung at most
// once over 20 frames: a richer rung is taken back only with
// panelCollapseSlack lines to spare. Without the memo the same frames flip it
// every time — the control that shows the heights tested are the ones where
// it could flip.
func TestTheBandRungDoesNotFlipAsTheFrameGrowsAndShrinksByARow(t *testing.T) {
	f := tallBandFleet(t)
	plus := *f.snap
	plus.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
	for _, s := range f.snap.Sessions {
		if s.Name == "authflow-21" {
			s.SessionID, s.Name = "sx", "authflow-99"
			plus.Sessions = append(plus.Sessions, s)
		}
	}
	if len(plus.Sessions) != len(f.snap.Sessions)+1 {
		t.Fatal("the extra idle row was not added")
	}
	seq := make([]*observer.Snapshot, 20)
	for i := range seq {
		seq[i] = f.snap
		if i%2 == 1 {
			seq[i] = &plus
		}
	}
	flips := func(p *panel) int {
		n, last := 0, -1
		for _, s := range seq {
			// A copy per frame: the frame memo is keyed on the pointer.
			c := *s
			deliver(t, p, snapshotEvent(&c))
			h := bandHeight(plain(p.view()))
			if last >= 0 && h != last {
				n++
			}
			last = h
		}
		return n
	}
	found := 0
	for _, mode := range []panelMode{modeStatus, modeProject} {
		for h := 20; h <= 72; h++ {
			stateless := sidebar(t, f.now, f.snap, mode, h)
			stateless.memo = nil // no memory: the rung is decided afresh every frame
			if flips(stateless) < 2 {
				continue
			}
			found++
			stateful := sidebar(t, f.now, f.snap, mode, h)
			if got := flips(stateful); got > 1 {
				t.Errorf("%s mode, %d lines: the band's height flipped %d times over %d frames, want at most once", mode, h, got, len(seq))
			}
		}
	}
	if found == 0 {
		t.Fatal("no height made the stateless rung flip, so this proves nothing")
	}
}

// #22: bandAttn 0 means "no previous frame". The first frame after a layout
// reset takes the richest rung that fits with NO slack, even when the frame
// before it — at another size — drew a poorer one. On an unchanged layout a
// poorer rung carried from the previous frame does wait for the slack.
func TestTheFirstFrameAfterALayoutResetTakesTheRichestRungThatFits(t *testing.T) {
	f := tallBandFleet(t)
	// Status mode: nothing collapses, so the least height that draws rung 4
	// fits it with no line to spare.
	least := 0
	for h := 72; h >= 10; h-- {
		p := sidebar(t, f.now, f.snap, modeStatus, h)
		p.view()
		if p.memo.bandAttn != panelBandTallest {
			break
		}
		least = h
	}
	if least == 0 || least >= 72 {
		t.Fatalf("no least height for rung 4 (%d)", least)
	}

	p := sidebar(t, f.now, f.snap, modeStatus, 24)
	p.view()
	if p.memo.bandAttn >= panelBandTallest {
		t.Fatalf("24 lines drew rung %d; the test needs a poorer rung first", p.memo.bandAttn)
	}
	p.height = least // a resize: the layout resets, and bandAttn with it
	p.view()
	if p.memo.bandAttn != panelBandTallest {
		t.Errorf("after a resize to %d lines the band is at rung %d, want %d: the sentinel did not reset", least, p.memo.bandAttn, panelBandTallest)
	}

	// The same layout with rung 3 carried: rung 4 fits, but not with the
	// slack, so the frame stays at 3.
	p.memo.bandAttn = 3
	c := *f.snap
	deliver(t, p, snapshotEvent(&c))
	p.view()
	if p.memo.bandAttn != 3 {
		t.Errorf("with rung 3 carried and no spare line, the band went to rung %d", p.memo.bandAttn)
	}
}

// FR9 / #32: a section title is "───  label  n  ───", centred on the width;
// narrower, the rules go first, then the label is cut, and the count stays.
// It never exceeds the width.
func TestASectionTitleGivesUpItsRulesThenItsLabelAndKeepsItsCount(t *testing.T) {
	sec := &panelSection{title: "authflow", rows: make([]panelRow, 3)}
	for _, c := range []struct {
		width int
		want  string
	}{
		{46, strings.Repeat("─", 15) + "  authflow  3  " + strings.Repeat("─", 16)},
		{21, "───  authflow  3  ───"},
		{20, "    authflow  3"},
		{13, " authflow  3"},
		{12, " authfl…  3"},
		{6, " …  3"},
		{5, "auth…"},
		{1, "…"},
	} {
		p := panel{width: c.width}
		line := p.sectionTitle(sec)
		if got := strings.TrimRight(ansi.Strip(line), " "); got != c.want {
			t.Errorf("width %d: %q, want %q", c.width, got, c.want)
		}
		if w := lipgloss.Width(line); w > c.width {
			t.Errorf("width %d: the title is %d wide", c.width, w)
		}
	}
}

// FR9 colours: every piece of the whole view's chrome is drawn in its named
// view token, in both themes — the active tab on the accent and the others on
// the dim fill, the thick baseline in the accent, a section title's label and
// count badges, the strip's number and label halves (NEED's number on solid
// red), the rule-coloured fades, the key strip's two halves and its ══ fill —
// in the accent in a focused sidebar, the rule colour in the TUI.
func TestTheChromeIsDrawnInItsPaletteTokens(t *testing.T) {
	for _, dark := range []bool{true, false} {
		withPanelTheme(t, dark)
		pick := func(c lipgloss.AdaptiveColor) string {
			if dark {
				return c.Dark
			}
			return c.Light
		}
		v := panelPal.view
		p := panel{width: panelWidth, surface: surfacePane}
		tabs, start, end := p.tabsLine(modeStatus)
		base := p.baseline(start, end)
		strip := p.countStrip(panelCounts{stripNeed: 2, stripRun: 3, stripIdle: 31, stripUnclear: 5})
		title := p.sectionTitle(&panelSection{title: "madbench", rows: make([]panelRow, 2)})
		keys := p.keyStrip()
		tuiKeys := panel{width: panelWidth, surface: surfaceTUI}.keyStrip()
		fillOn := func(fg, bg lipgloss.AdaptiveColor) string {
			return sgrRGB(t, 38, pick(fg)) + ";" + sgrRGB(t, 48, pick(bg)) + "m"
		}
		for _, c := range []struct {
			what, line, want string
		}{
			{"active tab", tabs, "1;" + fillOn(v.tabOnFg, v.tabOnBg) + " S[t]atus "},
			{"other tab", tabs, fillOn(v.tabOffFg, v.tabOffBg) + " [P]roject "},
			{"tab divider", tabs, fillOn(panelPal.rule, v.tabOffBg) + "│"},
			{"thick baseline", base, sgrRGB(t, 38, pick(v.tabOnBg)) + "m━━━━━━━━━━"},
			{"thin baseline", base, sgrRGB(t, 38, pick(panelPal.rule)) + "m───"},
			{"NEED number", strip, "1;" + fillOn(v.strip[stripNeed].labelFg, v.strip[stripNeed].labelBg) + " 2 "},
			{"NEED label", strip, fillOn(v.strip[stripNeed].valueFg, v.strip[stripNeed].valueBg) + " NEED "},
			{"RUN number", strip, "1;" + fillOn(v.strip[stripRun].labelFg, v.strip[stripRun].labelBg) + " 3 "},
			{"IDL label", strip, fillOn(v.strip[stripIdle].valueFg, v.strip[stripIdle].valueBg) + " IDL "},
			{"??? label", strip, fillOn(v.strip[stripUnclear].valueFg, v.strip[stripUnclear].valueBg) + " ??? "},
			{"fade", strip, sgrRGB(t, 38, pick(panelPal.rule)) + "m░░▒▒▓"},
			{"title label", title, "1;" + fillOn(v.titleFg, v.titleBg) + " madbench "},
			{"title count", title, fillOn(v.countFg, v.countBg) + " 2 "},
			{"title rule", title, sgrRGB(t, 38, pick(panelPal.rule)) + "m───"},
			{"key", keys, "1;" + fillOn(v.keyFg, v.keyBg) + " f "},
			{"key label", keys, fillOn(v.keyLabelFg, v.keyLabelBg) + " only "},
			{"key fill, focused sidebar", keys, sgrRGB(t, 38, pick(v.tabOnBg)) + "m═══"},
			{"key fill, TUI", tuiKeys, sgrRGB(t, 38, pick(panelPal.rule)) + "m══════"},
		} {
			if !strings.Contains(c.line, c.want) {
				t.Errorf("dark=%v %s: %q lacks %q", dark, c.what, c.line, c.want)
			}
		}
	}
}

// BenchmarkComposeFleet is a whole recompose — frame, rows, settle over three
// rungs, paint — on the golden fleet at the sidebar's width. compose itself
// is called, so the frame memo never short-cuts it.
func BenchmarkComposeFleet(b *testing.B) {
	f := goldenFleet(b)
	for _, h := range []int{40, 59} {
		b.Run(fmt.Sprintf("46x%d", h), func(b *testing.B) {
			p := sidebar(b, f.now, f.snap, modeProject, h)
			for b.Loop() {
				p.compose()
			}
		})
	}
}

// A recompose stays under 10 ms (§10): settle plans every detail of the
// ladder, but it counts items and renders nothing.
//
// It is cheap on purpose (review round 1): three batches of ten composes per
// frame, the best batch judged — a load spike slows one batch, not all three —
// in every mode, where colour mode's settle decides all three rungs. About
// 50 ms in all; BenchmarkComposeFleet is the measurement.
func TestComposeStaysUnderTenMilliseconds(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	const batches, per = 3, 10
	f := goldenFleet(t)
	for _, mode := range panelModes {
		for _, h := range []int{40, 59} {
			p := sidebar(t, f.now, f.snap, mode, h)
			best := time.Duration(1<<63 - 1)
			for range batches {
				start := time.Now()
				for range per {
					p.compose()
				}
				best = min(best, time.Since(start)/per)
			}
			t.Logf("%s 46x%d: %s per compose", mode, h, best)
			if best > 10*time.Millisecond {
				t.Errorf("%s 46x%d: compose takes %s, over 10 ms", mode, h, best)
			}
		}
	}
}
