package main

// panel_l2_test.go covers the built-in row of feedback 4 — the design sheet's
// status family A in layout L2 (`task tt:options`): the status code on its own
// badge, the runtime on a second one, the name on L1's colour badge, and the
// context as a count badge at the right edge with no bar.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

// isLoudFill reports whether a painted fill is one of the calm budget's
// saturated fills in that theme.
func isLoudFill(c lipgloss.AdaptiveColor, dark bool) bool {
	return loudFills(dark)[fillSGR(c, dark)]
}

// {status:code} is five columns in every tone: the solid hue with its ink
// (the loud value style, bold) when the tone is loud, and the soft label half
// of its own hue otherwise — the sheet's column A.
func TestTheStatusCodeBadgeIsSolidOnlyWhenLoud(t *testing.T) {
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			dark := half.name == "dark"
			withPanelTheme(t, dark)
			for _, tone := range badgeTones {
				for code, want := range map[string]string{"BLK": " BLK ", "SH": " SH  ", "???": " ??? ", "TOOLONG": " TOO "} {
					seg := statusCodeBadge(code, tone)
					if len(seg.Parts) != 1 || segText(seg) != want || ansi.StringWidth(segText(seg)) != badgeLabelW {
						t.Errorf("%v %q: %q, want %q (%d columns)", tone, code, segText(seg), want, badgeLabelW)
					}
				}
				c := badgeColoursFor(tone)
				fg, bg := c.labelFg, c.labelBg
				if tone.loud() {
					fg, bg = c.valueFg, c.valueBg
				}
				line := paintSegment(statusCodeBadge("BLK", tone))
				if !strings.Contains(line, sgrRGB(t, 38, half.pick(fg))) || !strings.Contains(line, sgrRGB(t, 48, half.pick(bg))) {
					t.Errorf("%v: %q is not %s on %s", tone, line, half.pick(fg), half.pick(bg))
				}
				if got := isLoudFill(bg, dark); got != tone.loud() {
					t.Errorf("%v: fill %s saturated=%v, want %v", tone, half.pick(bg), got, tone.loud())
				}
				if bold := statusCodeBadge("BLK", tone).Parts[0].Style.GetBold(); bold != tone.loud() {
					t.Errorf("%v: bold=%v, want %v", tone, bold, tone.loud())
				}
			}
		})
	}
}

// {runtime:badge} is seven columns, the runtime right-aligned, in every tone:
// a loud tone paints exactly the split badge's loud value half, every other
// tone the one neutral fill with body text.
func TestTheRuntimeBadgeIsSolidOnlyWhenLoud(t *testing.T) {
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			dark := half.name == "dark"
			withPanelTheme(t, dark)
			neutral := runtimeNeutralColours()
			for _, tone := range badgeTones {
				for rt, want := range map[string]string{"3h12m": " 3h12m ", "17m": "   17m ", "—": "     — ", "123456": " 12345 "} {
					if got := segText(runtimeBadge(rt, tone)); got != want || ansi.StringWidth(got) != badgeValueW {
						t.Errorf("%v %q: %q, want %q", tone, rt, got, want)
					}
				}
				fg, bg := neutral.fg, neutral.bg
				if tone.loud() {
					c := badgeColoursFor(tone)
					fg, bg = c.valueFg, c.valueBg
					if got, want := paintSegment(runtimeBadge("2d04h", tone)), paintedRuntime("2d04h", tone); got != want {
						t.Errorf("%v: the loud runtime badge %q is not the loud value half %q", tone, got, want)
					}
				}
				line := paintSegment(runtimeBadge("2d04h", tone))
				if !strings.Contains(line, sgrRGB(t, 38, half.pick(fg))) || !strings.Contains(line, sgrRGB(t, 48, half.pick(bg))) {
					t.Errorf("%v: %q is not %s on %s", tone, line, half.pick(fg), half.pick(bg))
				}
				if got := isLoudFill(bg, dark); got != tone.loud() {
					t.Errorf("%v: fill %s saturated=%v, want %v", tone, half.pick(bg), got, tone.loud())
				}
			}
			// The neutral fill is one of the palette's quiet fills, and its
			// text clears the text floor on it (measured, as painted).
			if !quietFills(dark)[fillSGR(neutral.bg, dark)] {
				t.Errorf("the neutral runtime fill %s is not listed as a quiet fill", half.pick(neutral.bg))
			}
			if r := contrastOnScreen(half.pick(neutral.fg), half.pick(neutral.bg)); r < contrastText {
				t.Errorf("runtime badge neutral: %s on %s is %.2f:1, want ≥ %.1f", half.pick(neutral.fg), half.pick(neutral.bg), r, contrastText)
			}
		})
	}
}

// The two tokens follow Attention on every row, :quiet keeps them soft, and
// the runtime badge is never Empty: with no start time it draws "—" in its
// seven columns, so the names after it still align.
func TestTheL2TokensFollowAttentionAndKeepTheirColumns(t *testing.T) {
	started := fleetNow.Add(-(2*24*time.Hour + 4*time.Hour))
	rc := &rowContext{now: fleetNow, ctx: defaultCtxThresholds}
	tok := func(name string, opts ...string) panelfmt.Token {
		tk := panelfmt.Token{Name: name, Known: true}
		for _, o := range opts {
			tk.Opts = append(tk.Opts, panelfmt.Opt{Name: o})
		}
		return tk
	}
	blocked := observer.Session{Name: "b", Status: observer.StatusBlocked, Attention: true, StartedAt: &started}
	idle := observer.Session{Name: "i", Status: observer.StatusIdle, StartedAt: &started}
	for _, c := range []struct {
		what string
		s    observer.Session
		tk   panelfmt.Token
		want panelfmt.Segment
	}{
		{"blocked code", blocked, tok("status", optCode), statusCodeBadge("BLK", badgeBlk)},
		{"blocked code, quiet", blocked, tok("status", optCode, optQuiet), statusCodeBadge("BLK", badgeBlkSoft)},
		{"idle code", idle, tok("status", optCode), statusCodeBadge("IDL", badgeIdle)},
		{"blocked runtime", blocked, tok("runtime", optBadge), runtimeBadge("2d04h", badgeBlk)},
		{"blocked runtime, quiet", blocked, tok("runtime", optBadge, optQuiet), runtimeBadge("2d04h", badgeBlkSoft)},
		{"idle runtime", idle, tok("runtime", optBadge), runtimeBadge("2d04h", badgeIdle)},
		{"no start time", observer.Session{Status: observer.StatusBusy}, tok("runtime", optBadge), runtimeBadge("—", badgeRun)},
	} {
		r := panelRow{s: c.s}
		got := rowResolver(&r, rc)(c.tk)
		if got.Empty || segText(got) != segText(c.want) || paintSegment(got) != paintSegment(c.want) {
			t.Errorf("%s: %q (%q), want %q", c.what, segText(got), paintSegment(got), segText(c.want))
		}
	}
	// Plain {runtime} is unchanged: slate text, Empty with no start time.
	r := panelRow{s: observer.Session{Status: observer.StatusBusy}}
	if got := rowResolver(&r, rc)(tok("runtime")); !got.Empty {
		t.Errorf("plain {runtime} with no start time drew %q", segText(got))
	}
}

// The built-in row is the sheet's L2, cell by cell, in both themes: the code
// badge at columns 1-5, a space, the runtime badge at 7-13, two spaces, the
// name at 17 (on its colour badge from 16), and the 6-column count badge
// against the right edge — no bar. Saturated fills sit only on an Attention
// row's code and runtime, and on its count above ctx_alert; a calm row's code
// is its soft tint, its runtime the neutral fill, its count its tier's tint.
func TestTheBuiltInRowIsTheSheetsLayoutL2(t *testing.T) {
	const w = panelWidth
	started := fleetNow.Add(-(2*24*time.Hour + 4*time.Hour))
	count := func(n int64) *observer.TranscriptFacts {
		return &observer.TranscriptFacts{Model: "claude-opus-5-5", ContextTokens: &n}
	}
	purple := count(612_000)
	purple.AgentColor, purple.ColorFound = "purple", true
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			dark := half.name == "dark"
			withPanelTheme(t, dark)
			set := panelDefaultSet
			rc := &rowContext{now: fleetNow, ctx: set.Ctx, window: set.Window}
			ctxFill := func(tone ctxTone) string { return fillSGR(ctxColoursFor(tone).bg, dark) }
			for _, c := range []struct {
				s              observer.Session
				text           string
				code, runtime  lipgloss.AdaptiveColor
				ctx            string
				nameBadge      lipgloss.AdaptiveColor
				nameBadgeCells int
			}{
				{
					s:    observer.Session{Name: "magmux", Status: observer.StatusBlocked, Attention: true, StartedAt: &started, Transcript: purple},
					text: "▌ BLK   2d04h    magmux                  612k ",
					code: panelPal.coral, runtime: panelPal.coral, ctx: ctxFill(ctxLoud),
					nameBadge: sessionHue("purple").badgeBg, nameBadgeCells: len(" magmux "),
				},
				{
					s:    observer.Session{Name: "tt-ui", Status: observer.StatusWaiting, Attention: true, StartedAt: &started, Transcript: count(140_000)},
					text: "  ASK   2d04h    tt-ui                   140k ",
					code: panelPal.ask, runtime: panelPal.ask, ctx: ctxFill(ctxQuiet),
				},
				{
					s:    observer.Session{Name: "ssh-agent", Status: observer.StatusIdle, StartedAt: &started, Transcript: count(371_000)},
					text: "  IDL   2d04h    ssh-agent               371k ",
					code: badgeColoursFor(badgeIdle).labelBg, runtime: runtimeNeutralColours().bg, ctx: ctxFill(ctxWarm),
				},
				{
					s:    observer.Session{Name: "big", Status: observer.StatusBusy, StartedAt: &started, Transcript: count(598_000)},
					text: "  RUN   2d04h    big                     598k ",
					code: badgeColoursFor(badgeRun).labelBg, runtime: runtimeNeutralColours().bg, ctx: ctxFill(ctxHot),
				},
			} {
				for _, band := range []bool{false, true} {
					tpl := set.Row
					if band {
						tpl = set.AttnRow
					}
					r := panelRow{s: c.s, class: panelGroupOf(c.s), band: band}
					line, _ := tpl.Line(w, rowResolver(&r, rc))
					if got := ansi.Strip(line); got != c.text {
						t.Errorf("%s band=%v:\n got %q\nwant %q", c.s.Name, band, got, c.text)
						continue
					}
					if strings.ContainsAny(line, ctxBarFull+ctxBarEmpty) {
						t.Errorf("%s: the built-in row draws a bar", c.s.Name)
					}
					bgs := cellBackgrounds(line)
					want := make([]string, w)
					for col := frameLabelStart; col < frameLabelEnd; col++ {
						want[col] = fillSGR(c.code, dark)
					}
					for col := frameValueStart; col < frameBadgeEnd; col++ {
						want[col] = fillSGR(c.runtime, dark)
					}
					for col := frameNamePad; col < frameNamePad+c.nameBadgeCells; col++ {
						want[col] = fillSGR(c.nameBadge, dark)
					}
					for col := w - ctxBadgeWidth; col < w; col++ {
						want[col] = c.ctx
					}
					for col := range want {
						if bgs[col] != want[col] {
							t.Errorf("%s band=%v col %d: fill %q, want %q", c.s.Name, band, col, bgs[col], want[col])
						}
						loud := loudFills(dark)[bgs[col]]
						allowed := c.s.Attention && (col >= frameLabelStart && col < frameBadgeEnd || col >= w-ctxBadgeWidth)
						if loud && !allowed {
							t.Errorf("%s band=%v col %d: a saturated fill outside an Attention row's badges", c.s.Name, band, col)
						}
					}
				}
			}
		})
	}
}
