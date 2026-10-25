package main

// parseFrame reads a painted panel frame back into fields, by the shape of
// the built-in panel.conf (§6.6), so a test that asserts on a row reads its
// status, age, name and group rather than carrying a regex over the whole
// line. It is the one place that knows the default row's geometry, in
// DISPLAY columns (feedback 4, layout L2):
//
//	col 0       the gutter: "▌" in the session's colour, or a blank cell
//	cols 1-5    the status code badge: " RUN ", the code in 2-4
//	col 6       the template's literal space
//	cols 7-13   the runtime badge: " 3h12m ", the runtime right-aligned in 8-12
//	cols 14-15  the template's two literal spaces
//	col 16      the name badge's leading cell
//	col 17…     the name (then its badge's trailing cell when coloured),
//	            " · group" when the row names one, the spring, and the ctx
//	            count badge (6 columns) at the right edge — or {ctx}'s badge,
//	            gap and bar, when a template opts into the bar
//
// Plain text cannot say whether a row is loud: the code is upper case on
// every row, and the attention cue is the badge's colour. parseStyledFrame
// reads that from the painted line (frameLine.tone).
//
// A heading is a centred section title (revision 4): "───  label  n  ───",
// a rule of at least three "─" either side of the label badge and the count
// badge; lineHeading. A collapsed group is its title alone.
//
// The header is the tabs (lineTitle), the baseline and the count strip
// (lineHeader); the key strip is the last line (lineKeys). A line under a row
// — the latest text, or a band row's why — is five columns in: the gutter
// cell ("▌" in the row's colour, or blank) and four spaces; lineIndented.
//
// Every other line — a partial or merged fold line, the overflow, a note —
// is lineOther and keeps its text.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fleetNow is the instant the fleet fixtures are rendered at.
var fleetNow = time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC)

type frameKind int

const (
	lineOther frameKind = iota
	lineTitle
	lineBlank
	// lineRow is a row's first line (row / attn_row).
	lineRow
	// lineIndented is a line under a row: the gutter cell, four spaces, text.
	lineIndented
	// lineHeading is a section title: rule, label, count, rule.
	lineHeading
	// lineHeader is the baseline or the count strip.
	lineHeader
	// lineKeys is the key strip.
	lineKeys
)

// frameLine is one painted line, classified.
type frameLine struct {
	kind frameKind
	text string // the plain line, right-trimmed
	// A row's fields, trimmed.
	gutter  bool   // "▌" drawn: the session has a /color
	code    string // the status code as drawn ("RUN", "SH"), pad trimmed
	status  string // the status that code stands for ("busy" for RUN), or ""
	runtime string // the badge's value half, trimmed ("3h12m", "—")
	// tone is the badge's look, read by parseStyledFrame from the painted
	// line: badgeAsk or badgeBlk when the row is loud, else badgeUnclear.
	// parseFrame (plain text) leaves it at the zero tone.
	tone        badgeTone
	name, group string
	ctx         string // the count, and the bar when drawn ("371k ▰▰▱▱▱")
	// body is an indented line's text without its indent.
	body string
	// A heading's label and its count ("7").
	heading, right string
	// bar is an indented line's gutter: "▌" drawn, the row has a /color.
	bar bool
}

// The default row's fixed display columns (see the file comment).
const (
	frameGutterCol  = 0
	frameLabelStart = 1                             // the code badge's pad
	frameLabelEnd   = frameLabelStart + badgeLabelW // 6: the literal space
	frameValueStart = frameLabelEnd + 1             // 7: the runtime badge's pad
	frameBadgeEnd   = frameValueStart + badgeValueW // 14: the two literal spaces
	frameNamePad    = frameBadgeEnd + 2             // 16: the name badge's lead
	frameNameStart  = frameNamePad + 1              // 17: the name itself
	frameIndent     = "     "                       // why and meta lines
)

// codeStatus is the status code table read backwards: the status each code
// stands for. ??? reads as unknown.
var codeStatus = func() map[string]string {
	m := map[string]string{}
	for st, code := range statusCodes {
		m[code] = string(st)
	}
	return m
}()

// displayCells splits a plain line into its display columns: a two-column
// glyph fills its column and leaves "" in the next, and a zero-width rune
// joins the column before it.
func displayCells(l string) []string {
	var out []string
	for _, r := range l {
		s := string(r)
		switch w := ansi.StringWidth(s); {
		case w == 0 && len(out) > 0:
			out[len(out)-1] += s
		default:
			out = append(out, s)
			for i := 1; i < w; i++ {
				out = append(out, "")
			}
		}
	}
	return out
}

// springGap is the last run of two or more spaces on a row: what separates
// the name (and group) from the ctx badge. Sanitised session text never holds
// two spaces in a row, and the spring plus the badge's pad is always two.
var springGap = regexp.MustCompile(` {2,}`)

// barTail is the ctx bar at the end of a row, and the blanks before it (the
// count badge's trailing pad and the gap, two cells, read as one).
var barTail = regexp.MustCompile(` +([▰▱]{5})$`)

// headingShape is a section title: the rules either side of the label badge
// (" label ") and the count badge (" n "), a space between each rule and its
// badge.
var headingShape = regexp.MustCompile(`^─{3,}  (\S.*?)  (\d+)  ─{3,}$`)

// headingRe matches the title line of the section labelled label, for
// indexOfLine.
func headingRe(label string) string {
	return `^─{3,}  ` + regexp.QuoteMeta(label) + `  \d+  ─{3,}$`
}

// indentedShape is a line under a row: the gutter cell, four spaces, text.
var indentedShape = regexp.MustCompile(`^([ ▌])    (\S.*)$`)

// keysShape is the key strip, with or without its ══ fill.
var keysShape = regexp.MustCompile(`^(?:═+ +)?(?:\^?[a-z]|/|j/k)  \S.*$`)

// headerShape is the baseline (thin rule with the thick stretch) or the count
// strip (its badges, with or without the fades).
var headerShape = regexp.MustCompile(`^(?:─*━+─*|[░▒▓]* *\d+ .*(?:NEED|RUN|IDL|\?\?\?|\b[NRI?]\b).*)$`)

// frameHeading is the parsed heading labelled label, and whether there is one.
func frameHeading(fl []frameLine, label string) (frameLine, bool) {
	for _, l := range fl {
		if l.kind == lineHeading && l.heading == label {
			return l, true
		}
	}
	return frameLine{}, false
}

// bandLines is the painted lines under a row — its text lines (a band row's
// why, another row's latest text) and its meta — as bodies, and whether the
// row is drawn at all.
func bandLines(fl []frameLine, name string) ([]string, bool) {
	for i, l := range fl {
		if l.kind != lineRow || l.name != name {
			continue
		}
		var body []string
		for _, m := range fl[i+1:] {
			if m.kind != lineIndented {
				break
			}
			body = append(body, m.body)
		}
		return body, true
	}
	return nil, false
}

// parseFrame classifies plain, right-trimmed lines (plain() in
// panel_modes_test.go produces them).
func parseFrame(lines []string) []frameLine {
	out := make([]frameLine, 0, len(lines))
	for i, l := range lines {
		fl := frameLine{text: l}
		switch {
		case i == 0:
			fl.kind = lineTitle
		case l == "":
			fl.kind = lineBlank
		case i == len(lines)-1 && keysShape.MatchString(strings.TrimSpace(l)):
			fl.kind = lineKeys
		case i <= 2 && headerShape.MatchString(strings.TrimSpace(l)):
			fl.kind = lineHeader
		case parseRowLine(l, &fl):
			fl.kind = lineRow
		case headingShape.MatchString(l):
			m := headingShape.FindStringSubmatch(l)
			fl.kind, fl.heading, fl.right = lineHeading, m[1], m[2]
		case indentedShape.MatchString(l):
			m := indentedShape.FindStringSubmatch(l)
			fl.kind, fl.bar, fl.body = lineIndented, m[1] == "▌", strings.TrimSpace(m[2])
		}
		out = append(out, fl)
	}
	return out
}

// parseStyledFrame is parseFrame over a painted view, plus each row's tone:
// whether its status badge's value half is painted in the loud honey or
// coral style (badgeAsk, badgeBlk); any other row reads as badgeUnclear. The
// view must be painted in colour (withPanelTheme before the panel is built):
// with no colour profile every style paints as bare text and nothing could be
// told apart, so that is a test bug and fails loudly.
func parseStyledFrame(t testing.TB, view string) []frameLine {
	t.Helper()
	if paintedRuntime("1m", badgeAsk) == paintedRuntime("1m", badgeBlk) {
		t.Fatal("parseStyledFrame needs a colour profile: call withPanelTheme first")
	}
	styled := strings.Split(view, "\n")
	fl := parseFrame(plain(view))
	for i := range fl {
		if fl[i].kind != lineRow {
			continue
		}
		fl[i].tone = badgeUnclear
		for _, tone := range []badgeTone{badgeBlk, badgeAsk} {
			if strings.Contains(styled[i], paintedRuntime(fl[i].runtime, tone)) {
				fl[i].tone = tone
			}
		}
	}
	return fl
}

// paintedRuntime is the bytes a status badge's value half is painted with in
// a tone.
func paintedRuntime(runtime string, tone badgeTone) string {
	return badgeStyleFor(tone).value.Render(cellPad + alignRight(runtime, badgeRuntimeCols) + cellPad)
}

// paintedWaiting is the bytes {waiting} paints for an age in a chip tone.
func paintedWaiting(age string, tone chipTone) string {
	return ageStyleFor(tone).Render(waitingPrefix + age)
}

// parseRowLine fills fl from a row's first line and reports whether l is one.
func parseRowLine(l string, fl *frameLine) bool {
	c := displayCells(l)
	if len(c) <= frameNameStart {
		return false
	}
	if c[frameGutterCol] != "▌" && c[frameGutterCol] != " " && c[frameGutterCol] != "❯" { // ❯: the cursor's pointer
		return false
	}
	code := strings.TrimSpace(strings.Join(c[frameLabelStart+1:frameLabelEnd-1], ""))
	runtime := strings.TrimSpace(strings.Join(c[frameValueStart+1:frameBadgeEnd-1], ""))
	if c[frameLabelStart] != " " || c[frameLabelStart+1] == " " || c[frameLabelEnd-1] != " " ||
		c[frameLabelEnd] != " " || c[frameValueStart] != " " || c[frameBadgeEnd-1] != " " ||
		c[frameBadgeEnd-2] == " " || c[frameBadgeEnd] != " " || c[frameBadgeEnd+1] != " " ||
		c[frameNamePad] != " " || c[frameNameStart] == " " || code == "" || runtime == "" {
		return false
	}
	fl.gutter = c[frameGutterCol] == "▌"
	fl.code, fl.status, fl.runtime = code, codeStatus[code], runtime
	rest := strings.TrimRight(strings.Join(c[frameNameStart:], ""), " ")
	bar := ""
	if m := barTail.FindStringSubmatchIndex(rest); m != nil {
		bar, rest = rest[m[2]:m[3]], rest[:m[0]]
	}
	left := rest
	if gaps := springGap.FindAllStringIndex(rest, -1); len(gaps) > 0 {
		last := gaps[len(gaps)-1]
		left, fl.ctx = rest[:last[0]], strings.TrimSpace(rest[last[1]:])
	}
	if bar != "" {
		fl.ctx += " " + bar
	}
	name, group, _ := strings.Cut(left, " · ")
	fl.name, fl.group = strings.TrimSpace(name), strings.TrimSpace(group)
	return true
}

// frameRows is the row lines of a parsed frame.
func frameRows(fl []frameLine) []frameLine {
	var out []frameLine
	for _, l := range fl {
		if l.kind == lineRow {
			out = append(out, l)
		}
	}
	return out
}

// frameRow is the row line for a session name, and whether there is one.
func frameRow(fl []frameLine, name string) (frameLine, bool) {
	for _, l := range fl {
		if l.kind == lineRow && l.name == name {
			return l, true
		}
	}
	return frameLine{}, false
}

// renderedRow is a session's pre-rendered lines in the panel's current frame,
// as plain text: what the row CARRIES, whether or not this frame's rung paints
// every line of it — its head, its meta, and its text lines at their fullest
// (why: up to three, gutter and indent trimmed). Fails when the frame has no
// such row.
func renderedRow(t *testing.T, p *panel, name string) (head, meta string, why []string, band bool) {
	t.Helper()
	f := p.frame()
	p.renderRows(&f)
	rows := func(yield func(*panelRow) bool) {
		for i := range f.secs {
			for j := range f.secs[i].rows {
				if !yield(&f.secs[i].rows[j]) {
					return
				}
			}
		}
	}
	for r := range rows {
		if r.s.Name != name {
			continue
		}
		for _, w := range r.lines.text[panelTextLines] {
			why = append(why, strings.TrimSpace(strings.TrimPrefix(ansi.Strip(w), "▌")))
		}
		return ansi.Strip(r.lines.head), ansi.Strip(r.lines.meta), why, r.band
	}
	t.Fatalf("no row named %q in the frame", name)
	return "", "", nil, false
}

// The helper is only evidence if it reads the frames the panel paints: every
// session the fleet draws by name is found as a row, with a badge, and the
// fields come out of a known line exactly.
func TestParseFrameReadsTheDefaultRowShape(t *testing.T) {
	fl := parseFrame([]string{
		"       Project │ Status │ Tmux │ Colour",
		"──────━━━━━━━━━───────────────────────────────",
		"░░▒▒▓  1  NEED  1  IDL  ▓▒▒░░",
		"",
		"───────────────  NEEDS YOU  1  ───────────────",
		"▌ ASK   2d04h    magus-00                    —",
		"▌    waiting 13m · input-needed",
		"───────────────  authflow  3  ────────────────",
		// The bar is opt-in ({ctx}); the oracle still reads it.
		"  IDL       —    madbench-32 · orbit-demo  248k ▰▱▱▱▱",
		"     the latest thing it said",
		"  + 2 idle",
		"─────  a-very-long-project-label-at-this  2  ─────",
		"═══════  /  filter  g  group  ═══════",
	})
	kinds := []frameKind{lineTitle, lineHeader, lineHeader, lineBlank, lineHeading, lineRow, lineIndented, lineHeading, lineRow, lineIndented, lineOther, lineHeading, lineKeys}
	for i, k := range kinds {
		if fl[i].kind != k {
			t.Errorf("line %d %q is kind %d, want %d", i, fl[i].text, fl[i].kind, k)
		}
	}
	if r := fl[5]; !r.gutter || r.code != "ASK" || r.status != "waiting" || r.runtime != "2d04h" || r.name != "magus-00" || r.group != "" || r.ctx != "—" {
		t.Errorf("band row parsed as %+v", r)
	}
	if r := fl[8]; r.gutter || r.code != "IDL" || r.status != "idle" || r.runtime != "—" || r.name != "madbench-32" || r.group != "orbit-demo" || r.ctx != "248k ▰▱▱▱▱" {
		t.Errorf("folded row parsed as %+v", r)
	}
	// A coloured name carries its badge's trailing cell before " · group",
	// and a two-letter code is padded to three.
	var sh frameLine
	if !parseRowLine("▌ SH      13h    custom-name  · grp   12k ▱▱▱▱▱", &sh) || sh.code != "SH" || sh.status != "shell" ||
		sh.runtime != "13h" || sh.name != "custom-name" || sh.group != "grp" || sh.ctx != "12k ▱▱▱▱▱" {
		t.Errorf("a coloured shell row parsed as %+v", sh)
	}
	// Feedback 3's split badge ({status:badge}) is not the default shape any
	// more: its runtime ends one column early.
	if parseRowLine("  WRK    29m   tt-ui-rebuil        519k  ▰▰▰▱▱", &frameLine{}) {
		t.Error("a split-badge row parsed as an L2 row")
	}
	// An emoji row of {status:icon} is not the default shape.
	if parseRowLine("▌⏳ 13m  magus-00                           —", &frameLine{}) {
		t.Error("an icon row parsed as a badge row")
	}
	if fl[6].body != "waiting 13m · input-needed" || !fl[6].bar {
		t.Errorf("why line parsed as %+v", fl[6])
	}
	if fl[9].body != "the latest thing it said" || fl[9].bar {
		t.Errorf("latest-text line parsed as %+v", fl[9])
	}
	for i, want := range map[int][2]string{4: {"NEEDS YOU", "1"}, 7: {"authflow", "3"}, 11: {"a-very-long-project-label-at-this", "2"}} {
		if fl[i].heading != want[0] || fl[i].right != want[1] {
			t.Errorf("heading %d parsed as %q / %q, want %q / %q", i, fl[i].heading, fl[i].right, want[0], want[1])
		}
	}
}

func TestParseFrameFindsEveryRowThePanelPaints(t *testing.T) {
	f := newFleet(t, fleetNow)
	for _, mode := range panelModes {
		p := sidebar(t, f.now, f.snap, mode, 0)
		fl := parseFrame(plain(p.view()))
		rows := frameRows(fl)
		live := 0
		for _, s := range f.snap.Sessions {
			if panelGroupOf(s) != groupEnded {
				live++ // ended sessions are not drawn
			}
		}
		if len(rows) != live {
			t.Errorf("%s: parsed %d rows from an unbounded frame of %d live sessions:\n%s",
				mode, len(rows), live, strings.Join(plain(p.view()), "\n"))
		}
		for _, r := range rows {
			if r.code == "" || r.status == "" || r.runtime == "" || r.name == "" || r.ctx == "" {
				t.Errorf("%s: a row parsed with an empty field: %+v", mode, r)
			}
		}
	}
}
