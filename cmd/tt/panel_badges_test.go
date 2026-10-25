package main

// panel_badges_test.go covers the row of feedback 3, whose parts the built-in
// row still uses or offers as an opt-in: the madbench split status badge
// ({status:badge}, a three-letter code and the session's runtime), the name
// badge in the session's /color, the context count with a bar filled against
// the model's window ({ctx}), the "waiting <age>" that opens the why line, and
// templates that draw no internal id. The width contract below runs on the
// current built-in row (feedback 4's L2; panel_l2_test.go has the rest).

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

// Every status has its code, one to three columns of upper case; a status
// this build has never seen is ???, never a guess. The tone follows
// Attention (loud honey, loud coral for blocked) and is otherwise the soft
// tone of the status.
func TestEveryStatusHasAThreeLetterCodeAndATone(t *testing.T) {
	cases := []struct {
		status    observer.Status
		code      string
		soft, att badgeTone
	}{
		{observer.StatusBusy, "RUN", badgeRun, badgeAsk},
		{observer.StatusWorking, "WRK", badgeRun, badgeAsk},
		{observer.StatusWaiting, "ASK", badgeAskSoft, badgeAsk},
		{observer.StatusBlocked, "BLK", badgeBlkSoft, badgeBlk},
		{observer.StatusIdle, "IDL", badgeIdle, badgeAsk},
		{observer.StatusShell, "SH", badgeShell, badgeAsk},
		{observer.StatusUnknown, "???", badgeUnclear, badgeAsk},
		{observer.StatusDone, "END", badgeEnded, badgeAsk},
		{observer.StatusFailed, "ERR", badgeFail, badgeAsk},
		{"compacting", "???", badgeUnclear, badgeAsk},
	}
	covered := map[observer.Status]bool{}
	for _, c := range cases {
		covered[c.status] = true
		s := observer.Session{Status: c.status}
		if got := statusCode(s); got != c.code || ansi.StringWidth(got) > badgeCodeCols || strings.ToUpper(got) != got {
			t.Errorf("%s: code %q, want %q (≤ %d upper-case columns)", c.status, got, c.code, badgeCodeCols)
		}
		if got := badgeToneOf(s); got != c.soft {
			t.Errorf("%s without attention: tone %v, want %v", c.status, got, c.soft)
		}
		s.Attention = true
		if got := badgeToneOf(s); got != c.att {
			t.Errorf("%s with attention: tone %v, want %v", c.status, got, c.att)
		}
		if got := softBadgeTone(s); got != c.soft {
			t.Errorf("%s quiet: tone %v, want %v", c.status, got, c.soft)
		}
	}
	for _, st := range observer.AllStatuses {
		if !covered[st] {
			t.Errorf("status %q has no expected code in this test", st)
		}
	}
}

// fmtRuntime never exceeds the value half's 5 columns, whatever the input.
func TestTheRuntimeIsAtMostFiveColumns(t *testing.T) {
	const day = 24 * time.Hour
	for d, want := range map[time.Duration]string{
		-1:                                "—",
		0:                                 "0s",
		42 * time.Second:                  "42s",
		17 * time.Minute:                  "17m",
		time.Hour:                         "1h00m",
		3*time.Hour + 12*time.Minute:      "3h12m",
		9*time.Hour + 59*time.Minute:      "9h59m",
		10 * time.Hour:                    "10h",
		13*time.Hour + 59*time.Minute:     "13h",
		23*time.Hour + 59*time.Minute:     "23h",
		day:                               "1d00h",
		6*day + 2*time.Hour:               "6d02h",
		9*day + 23*time.Hour:              "9d23h",
		10 * day:                          "10d",
		999 * day:                         "999d",
		5000 * day:                        "999d",
		time.Duration(1<<62) + time.Hour:  "999d",
		6*day + 2*time.Hour + time.Minute: "6d02h",
	} {
		got := fmtRuntime(d)
		if got != want {
			t.Errorf("fmtRuntime(%v) = %q, want %q", d, got, want)
		}
		if w := ansi.StringWidth(got); w > badgeRuntimeCols {
			t.Errorf("fmtRuntime(%v) = %q is %d columns", d, got, w)
		}
	}
	for d := time.Duration(0); d < 1200*day; d += 37*time.Minute + 11*time.Second {
		if w := ansi.StringWidth(fmtRuntime(d)); w > badgeRuntimeCols {
			t.Fatalf("fmtRuntime(%v) = %q is %d columns", d, fmtRuntime(d), w)
		}
	}
	started := fleetNow.Add(-time.Hour)
	if d := panelRuntime(observer.Session{StartedAt: &started}, fleetNow); d != time.Hour {
		t.Errorf("runtime %v, want 1h", d)
	}
	future := fleetNow.Add(time.Hour)
	if d := panelRuntime(observer.Session{StartedAt: &future}, fleetNow); d != 0 {
		t.Errorf("a start in the future is %v, want 0", d)
	}
	if d := panelRuntime(observer.Session{}, fleetNow); d >= 0 {
		t.Errorf("no start time is %v, want negative (—)", d)
	}
}

// The badge is always 12 columns in two halves; each half paints its tone's
// colours, and only a loud tone's value half is a saturated fill.
func TestTheStatusBadgeIsTwelveColumnsInItsTonesColours(t *testing.T) {
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			withPanelTheme(t, half.name == "dark")
			for _, tone := range badgeTones {
				for _, tc := range []struct{ code, runtime string }{{"RUN", "3h12m"}, {"SH", "13h"}, {"???", "—"}, {"TOOLONG", "123456"}} {
					seg := statusBadge(tc.code, tc.runtime, tone)
					if len(seg.Parts) != 2 || ansi.StringWidth(seg.Parts[0].Text) != badgeLabelW || ansi.StringWidth(seg.Parts[1].Text) != badgeValueW {
						t.Fatalf("%v %q: parts %q", tone, tc.code, segText(seg))
					}
					if !strings.HasPrefix(seg.Parts[1].Text, cellPad) || !strings.HasSuffix(seg.Parts[1].Text, cellPad) {
						t.Errorf("%v: the runtime has no pad: %q", tone, seg.Parts[1].Text)
					}
				}
				c := badgeColoursFor(tone)
				line := paintSegment(statusBadge("RUN", "3h12m", tone))
				for _, want := range []string{
					sgrRGB(t, 38, half.pick(c.labelFg)), sgrRGB(t, 48, half.pick(c.labelBg)),
					sgrRGB(t, 38, half.pick(c.valueFg)), sgrRGB(t, 48, half.pick(c.valueBg)),
				} {
					if !strings.Contains(line, want) {
						t.Errorf("%v: %q lacks %s", tone, line, want)
					}
				}
				loudValue := half.pick(c.valueBg) == half.pick(panelPal.ask) || half.pick(c.valueBg) == half.pick(panelPal.coral)
				if loudValue != tone.loud() {
					t.Errorf("%v: value fill %s loud=%v, want %v", tone, half.pick(c.valueBg), loudValue, tone.loud())
				}
				if half.pick(c.labelBg) == half.pick(c.valueBg) && tone != badgeEnded {
					t.Errorf("%v: the two halves share a fill, so the split does not read", tone)
				}
			}
			if segText(statusBadge("SH", "13h", badgeShell)) != " SH     13h " {
				t.Errorf("shell badge = %q", segText(statusBadge("SH", "13h", badgeShell)))
			}
		})
	}
}

// The width contract of the built-in row: every head line the row and
// attn_row draw — for every status, with and without Attention, coloured or
// not, with a known window, an unknown one and no count — is at most its
// width in display columns, and wherever the row has room its name starts in
// column frameNameStart (17, feedback 4's L2 row) and its count ends at the
// right edge.
func TestBadgeRowsFitTheirWidthAndStartTheirNamesInOneColumn(t *testing.T) {
	set := panelDefaultSet
	statuses := append(slices.Clone(observer.AllStatuses), "compacting")
	since, started := fleetNow.Add(-13*time.Minute), fleetNow.Add(-(6*24*time.Hour + 2*time.Hour))
	used := int64(371_000)
	rc := &rowContext{now: fleetNow, ctx: set.Ctx, window: set.Window, project: func(string) string { return "" }}
	transcripts := map[string]*observer.TranscriptFacts{
		"no transcript":  nil,
		"1M window":      {Model: "claude-opus-5-5", ContextTokens: &used, AgentColor: "purple", ColorFound: true},
		"unknown window": {Model: "gpt-9", ContextTokens: &used},
		"no count":       {Model: "claude-opus-5-5", AgentColor: "yellow", ColorFound: true},
	}
	for _, st := range statuses {
		for tname, tr := range transcripts {
			for _, attn := range []bool{false, true} {
				s := observer.Session{SessionID: "id", Name: "row-" + string(st), Status: st, Attention: attn,
					StatusSince: &since, StartedAt: &started, Transcript: tr}
				for _, band := range []bool{false, true} {
					r := panelRow{s: s, class: panelGroupOf(s), band: band, label: "grp"}
					tpl := set.Row
					if band {
						tpl = set.AttnRow
					}
					for w := 1; w <= 80; w++ {
						line, ok := tpl.Line(w, rowResolver(&r, rc))
						if !ok {
							t.Fatalf("%s %s: no line at %d", st, tname, w)
						}
						if got := ansi.StringWidth(line); got > w {
							t.Errorf("%s %s attention=%v band=%v: %d columns at width %d: %q", st, tname, attn, band, got, w, ansi.Strip(line))
						}
						if w < 40 {
							continue
						}
						p := ansi.Strip(line)
						i := strings.Index(p, "row-")
						if i < 0 {
							t.Errorf("%s %s at %d: the name is cut: %q", st, tname, w, p)
							continue
						}
						if col := ansi.StringWidth(p[:i]); col != frameNameStart {
							t.Errorf("%s %s attention=%v at %d: the name starts in column %d, want %d: %q", st, tname, attn, w, col, frameNameStart, p)
						}
						if got := ansi.StringWidth(p); got != w {
							t.Errorf("%s %s at %d: the row is %d columns; the ctx column must end at the edge: %q", st, tname, w, got, p)
						}
					}
				}
			}
		}
	}
}

// The name badge: a /color fills the name's cells (lead, name, trail) in the
// hue's badge colours; no colour is the body colour with no fill; an ENDED
// row is slate with no fill. The lead cell is always there.
func TestTheNameBadgeFillsOnlyAColouredName(t *testing.T) {
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			withPanelTheme(t, half.name == "dark")
			for _, name := range sortedHueNames() {
				h := sessionHue(name)
				seg := nameBadge("wire-ui", h, false)
				if segText(seg) != " wire-ui " {
					t.Errorf("%s: %q", name, segText(seg))
				}
				line := paintSegment(seg)
				if !strings.Contains(line, sgrRGB(t, 38, half.pick(h.badgeFg))) || !strings.Contains(line, sgrRGB(t, 48, half.pick(h.badgeBg))) {
					t.Errorf("%s: %q is not in the %s name badge colours", name, line, name)
				}
			}
			for what, seg := range map[string]panelfmt.Segment{
				"no colour": nameBadge("wire-ui", panelHue{}, false),
				"ended":     nameBadge("wire-ui", sessionHue("blue"), true),
			} {
				if segText(seg) != " wire-ui" {
					t.Errorf("%s: %q, want the lead cell and the name", what, segText(seg))
				}
				if line := paintSegment(seg); strings.Contains(line, "48;2;") {
					t.Errorf("%s: %q paints a fill", what, line)
				}
			}
		})
	}
}

// The bar's fill: the share of the window rounded to fifths, clamped.
func TestTheContextBarFillsAgainstTheWindow(t *testing.T) {
	const m = 1_000_000
	for _, c := range []struct {
		used, window int64
		want         int
	}{
		{371_000, m, 2}, {417_000, m, 2}, {598_000, m, 3}, {612_000, m, 3},
		{99_000, m, 0}, {100_000, m, 1}, {900_000, m, 5}, {m, m, 5}, {1_200_000, m, 5},
		{431_000, 200_000, 5}, {140_000, 200_000, 4}, {0, m, 0}, {-5, m, 0}, {5, 0, 0},
	} {
		if got := ctxBarFilled(c.used, c.window); got != c.want {
			t.Errorf("ctxBarFilled(%d, %d) = %d, want %d", c.used, c.window, got, c.want)
		}
	}

	withPanelTheme(t, true)
	used := int64(371_000)
	for _, tone := range ctxTones {
		seg := ctxWithBar(&used, m, tone)
		if got := segText(seg); got != " 371k  ▰▰▱▱▱" || ansi.StringWidth(got) != ctxWidth {
			t.Errorf("%v: {ctx} = %q", tone, got)
		}
		full := seg.Parts[len(seg.Parts)-2]
		if full.Text != "▰▰" || full.Style.GetForeground() != ctxColoursFor(tone).plainFg {
			t.Errorf("%v: the filled cells %q are not in the tier's colour", tone, full.Text)
		}
		if full.Style.GetBackground() != (lipgloss.NoColor{}) {
			t.Errorf("%v: the bar paints a fill", tone)
		}
	}
	// No window, or no count: the bar's columns are blank, so counts align.
	for what, seg := range map[string]panelfmt.Segment{
		"no window": ctxWithBar(&used, 0, ctxWarm),
		"no count":  ctxWithBar(nil, m, ctxUnknown),
	} {
		got := segText(seg)
		if ansi.StringWidth(got) != ctxWidth || strings.ContainsAny(got, ctxBarFull+ctxBarEmpty) {
			t.Errorf("%s: {ctx} = %q, want the count and %d blank columns", what, got, ctxBarWidth)
		}
	}
}

// The built-in window rules, and a panel.conf rule laid over them.
func TestTheBuiltInWindowRulesAndAFilesRule(t *testing.T) {
	for model, want := range map[string]int64{
		"claude-opus-5-5":          1_000_000,
		"claude-opus-5-20260901":   1_000_000,
		"claude-sonnet-5":          1_000_000,
		"claude-fable-5-1":         1_000_000,
		"claude-haiku-4-5":         200_000,
		"claude-sonnet-4-20250514": 200_000,
	} {
		if got, ok := panelDefaultSet.Window(model); !ok || got != want {
			t.Errorf("built-in window of %s = %d, %v; want %d", model, got, ok, want)
		}
	}
	for _, model := range []string{"gpt-9", "", "opus-5"} {
		if _, ok := panelDefaultSet.Window(model); ok {
			t.Errorf("the built-in rules know a window for %q", model)
		}
	}
	set := parsePanelConfForTest(t, "window claude-haiku-* = 64k\nwindow gpt-* = 400k\n")
	for model, want := range map[string]int64{"claude-haiku-4-5": 64_000, "gpt-9": 400_000, "claude-opus-5-5": 1_000_000} {
		if got, ok := set.Window(model); !ok || got != want {
			t.Errorf("window of %s with the file = %d, %v; want %d", model, got, ok, want)
		}
	}
}

// {waiting} is "waiting <age:short>" in the row's loud hue on an Attention
// row, and nothing on any other row or when the status time is unknown;
// {ctxbar} is the bar alone, and nothing without a window; {ctx:nobar} is the
// count badge alone; {runtime} is the runtime as text.
func TestTheNewTokensDrawWhatTheySay(t *testing.T) {
	withPanelTheme(t, true)
	since, started := fleetNow.Add(-13*time.Hour), fleetNow.Add(-(3*time.Hour + 12*time.Minute + 13*time.Hour))
	used := int64(598_000)
	tr := &observer.TranscriptFacts{Model: "claude-opus-5-5", ContextTokens: &used}
	set := parsePanelConfForTest(t, `row = "{waiting}|{ctxbar}|{ctx:nobar}|{runtime}"`)
	draw := func(s observer.Session) string {
		r := panelRow{s: s, class: panelGroupOf(s)}
		rc := &rowContext{now: fleetNow, ctx: set.Ctx, window: set.Window, project: func(string) string { return "" }}
		line, _ := set.Row.Line(200, rowResolver(&r, rc))
		return line
	}
	blocked := observer.Session{Name: "b", Status: observer.StatusBlocked, Attention: true, StatusSince: &since, StartedAt: &started, Transcript: tr}
	if got := ansi.Strip(draw(blocked)); got != "waiting 13h|▰▰▰▱▱| 598k |16h" {
		t.Errorf("blocked row: %q", got)
	}
	if line := draw(blocked); !strings.Contains(line, paintedWaiting("13h", chipCoral)) {
		t.Errorf("the blocked wait is not in the loud coral: %q", line)
	}
	calm := blocked
	calm.Status, calm.Attention, calm.StartedAt = observer.StatusIdle, false, nil
	calm.Transcript = &observer.TranscriptFacts{Model: "gpt-9", ContextTokens: &used}
	if got := ansi.Strip(draw(calm)); got != " 598k " {
		t.Errorf("a calm row with no window and no start: %q, want the count badge alone", got)
	}
	noSince := blocked
	noSince.StatusSince = nil
	if got := ansi.Strip(draw(noSince)); strings.Contains(got, "waiting") {
		t.Errorf("an unknown status time drew a wait: %q", got)
	}
}

// The built-in templates draw no tmux pane id, no job hash and no internal
// session id, at any detail, in any mode — only names a person uses.
func TestTheDefaultTemplatesDrawNoInternalIDs(t *testing.T) {
	f := goldenFleet(t)
	pane := regexp.MustCompile(`%\d+`)
	for _, mode := range panelModes {
		p := sidebar(t, f.now, f.snap, mode, 0)
		view := ansi.Strip(p.view())
		if pane.MatchString(view) || strings.Contains(view, "be9663df") || strings.Contains(view, "bg ") {
			t.Errorf("%s: an internal id is drawn:\n%s", mode, view)
		}
	}
}

// The render tick serves the runtime as well as the age: a session whose
// runtime is under a minute (and whose status time is unknown) is redrawn
// within a second, with no timer of its own.
func TestTheTickFollowsTheRuntime(t *testing.T) {
	p, _, _ := openTestPanel(t, fleetNow)
	started := fleetNow.Add(-(20*time.Minute + 30*time.Second))
	live := true
	deliver(t, p, snapshotEvent(&observer.Snapshot{
		ObservedAt: fleetNow, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{SessionID: "r", Name: "runner", Status: observer.StatusBusy, Live: &live, StartedAt: &started}},
	}))
	if d := p.nextTick(); d != 30*time.Second {
		t.Errorf("nextTick = %v, want the 30s until the runtime reads 20m → 21m", d)
	}
}
