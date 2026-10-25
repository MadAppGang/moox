package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// withPanelTheme forces truecolor and one background for the length of a
// test, so a colour assertion is never vacuous (go test's stdout is not a
// terminal) and each theme is checked on purpose. Not parallel: the profile
// and background are process-global lipgloss state.
func withPanelTheme(t *testing.T, dark bool) {
	t.Helper()
	profile, wasDark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(dark)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(profile)
		lipgloss.SetHasDarkBackground(wasDark)
	})
}

// sgrRGB is the truecolor SGR fragment termenv emits for a #RRGGBB colour,
// as foreground (38) or background (48): the PAINTED value (paintedHex), which
// can be one step under the palette's in a channel.
func sgrRGB(t *testing.T, layer int, hex string) string {
	t.Helper()
	parseTestHex(t, "sgr", hex)
	v, _ := strconv.ParseUint(paintedHex(hex)[1:], 16, 32)
	return fmt.Sprintf("%d;2;%d;%d;%d", layer, v>>16&0xFF, v>>8&0xFF, v&0xFF)
}

// segText is a segment's plain text, and segWidth its display width.
func segText(s panelfmt.Segment) string {
	var b strings.Builder
	for _, p := range s.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func segWidth(s panelfmt.Segment) int { return ansi.StringWidth(segText(s)) }

// renderPart paints one part the way the engine will: its style over its text.
func renderPart(p panelfmt.Part) string { return p.Style.Render(p.Text) }

// TestStatusChipIsAlwaysElevenColumns: names align only if the chip never
// changes width — for any tone, any label (short, long, wide) and any age.
func TestStatusChipIsAlwaysElevenColumns(t *testing.T) {
	labels := []string{"WAIT", "BLCK", "busy", "sh", "?", "", "TOOLONG", "名前"}
	ages := []string{"5s", "13m", "23h", "99d", unknownMark, "", "12345"}
	n := 0
	for _, tone := range chipTones {
		for _, label := range labels {
			for _, age := range ages {
				seg := statusChip(label, age, tone)
				if len(seg.Parts) != 2 {
					t.Fatalf("statusChip(%q,%q,%v) has %d parts, want 2", label, age, tone, len(seg.Parts))
				}
				if w := ansi.StringWidth(seg.Parts[0].Text); w != chipLabelW {
					t.Errorf("label half of (%q,%v) is %d wide, want %d", label, tone, w, chipLabelW)
				}
				if w := ansi.StringWidth(seg.Parts[1].Text); w != chipValueW {
					t.Errorf("value half of (%q,%v) is %d wide, want %d", age, tone, w, chipValueW)
				}
				if w := segWidth(seg); w != 11 {
					t.Errorf("statusChip(%q,%q,%v) is %d wide, want 11", label, age, tone, w)
				}
				if seg.Empty || seg.Shrink != 0 || seg.Drop != 0 {
					t.Errorf("statusChip must be rigid and never Empty: %+v", seg)
				}
				n++
			}
		}
	}
	if n == 0 {
		t.Fatal("no chip was measured")
	}
}

// TestStatusChipAlignment pins the text layout the mockup shows: label left in
// 4, age right in 3, one pad cell each side of each half.
func TestStatusChipAlignment(t *testing.T) {
	for _, c := range []struct{ label, age, want string }{
		{"WAIT", "13m", " WAIT  13m "},
		{"busy", "38s", " busy  38s "},
		{"sh", "5s", " sh     5s "},
		{"?", unknownMark, " ?       — "},
		{"idle", "3h", " idle   3h "},
	} {
		if got := segText(statusChip(c.label, c.age, chipDim)); got != c.want {
			t.Errorf("statusChip(%q,%q) = %q, want %q", c.label, c.age, got, c.want)
		}
	}
	if got := segText(statusChipLabel("WAIT", chipHoney)); got != " WAIT " {
		t.Errorf("statusChipLabel = %q, want %q", got, " WAIT ")
	}
	if seg := statusChipLabel("sh", chipDim); len(seg.Parts) != 1 || segWidth(seg) != chipLabelW {
		t.Errorf("statusChipLabel must be one %d-column part, got %+v", chipLabelW, seg)
	}
}

// TestStatusChipLoudnessFollowsTheTone: a loud chip is bold in both halves and
// its value half sits on a loud (saturated) fill; a dim chip bolds only its age
// and every fill it uses is quiet. This is the chip half of the calm budget.
func TestStatusChipLoudnessFollowsTheTone(t *testing.T) {
	fills := panelFillTones()
	isLoud := map[lipgloss.AdaptiveColor]bool{}
	for _, c := range fills.loud {
		isLoud[c] = true
	}
	for _, tone := range chipTones {
		seg := statusChip("WAIT", "13m", tone)
		label, value := seg.Parts[0].Style, seg.Parts[1].Style
		c := chipColoursFor(tone)
		if label.GetBold() != tone.loud() {
			t.Errorf("%v: label bold = %v, want %v", tone, label.GetBold(), tone.loud())
		}
		if !value.GetBold() {
			t.Errorf("%v: the age must be bold", tone)
		}
		if isLoud[c.labelBg] {
			t.Errorf("%v: the label half uses a loud fill %+v", tone, c.labelBg)
		}
		if isLoud[c.valueBg] != tone.loud() {
			t.Errorf("%v: value fill loud = %v, want %v", tone, isLoud[c.valueBg], tone.loud())
		}
		if label.GetBackground() != c.labelBg || value.GetBackground() != c.valueBg ||
			label.GetForeground() != c.labelFg || value.GetForeground() != c.valueFg {
			t.Errorf("%v: the chip does not draw with its colour table", tone)
		}
	}
}

// TestStatusChipPaintsThePaletteInBothThemes renders each chip under truecolor
// and finds the palette's exact RGB in the SGR, in dark and in light: the
// colours the contrast test scored are the colours on screen.
func TestStatusChipPaintsThePaletteInBothThemes(t *testing.T) {
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			withPanelTheme(t, half.name == "dark")
			for _, tone := range chipTones {
				seg := statusChip("WAIT", "13m", tone)
				c := chipColoursFor(tone)
				label, value := renderPart(seg.Parts[0]), renderPart(seg.Parts[1])
				for _, want := range []struct{ out, sgr, what string }{
					{label, sgrRGB(t, 38, half.pick(c.labelFg)), "label fg"},
					{label, sgrRGB(t, 48, half.pick(c.labelBg)), "label bg"},
					{value, sgrRGB(t, 38, half.pick(c.valueFg)), "value fg"},
					{value, sgrRGB(t, 48, half.pick(c.valueBg)), "value bg"},
				} {
					if !strings.Contains(want.out, want.sgr) {
						t.Errorf("%v %s: %q lacks %s", tone, want.what, want.out, want.sgr)
					}
				}
				if got := ansi.Strip(label + value); got != " WAIT  13m " {
					t.Errorf("%v: painted text %q", tone, got)
				}
			}
		})
	}
}

// TestCtxBadgeToneEdges: the thresholds are absolute and inclusive of the
// warm band at both ends (§8: 199_999 quiet; 200_000 and 400_000 warm;
// 400_001 hot; nil unknown), and panel.conf overrides move the edges. The
// counts alone never make a badge loud.
func TestCtxBadgeToneEdges(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		used *int64
		th   panelfmt.Thresholds
		want ctxTone
	}{
		{nil, defaultCtxThresholds, ctxUnknown},
		{n(0), defaultCtxThresholds, ctxQuiet},
		{n(199_999), defaultCtxThresholds, ctxQuiet},
		{n(200_000), defaultCtxThresholds, ctxWarm},
		{n(400_000), defaultCtxThresholds, ctxWarm},
		{n(400_001), defaultCtxThresholds, ctxHot},
		{n(12_000_000), defaultCtxThresholds, ctxHot},
		// Overridden edges.
		{n(99_999), panelfmt.Thresholds{Warn: 100_000, Alert: 150_000}, ctxQuiet},
		{n(120_000), panelfmt.Thresholds{Warn: 100_000, Alert: 150_000}, ctxWarm},
		{n(150_001), panelfmt.Thresholds{Warn: 100_000, Alert: 150_000}, ctxHot},
		{n(450_000), panelfmt.Thresholds{Warn: 300_000, Alert: 600_000}, ctxWarm},
		{nil, panelfmt.Thresholds{Warn: 1, Alert: 2}, ctxUnknown},
	} {
		if got := ctxToneOf(c.used, c.th); got != c.want {
			v := "nil"
			if c.used != nil {
				v = strconv.FormatInt(*c.used, 10)
			}
			t.Errorf("ctxToneOf(%s, %+v) = %v, want %v", v, c.th, got, c.want)
		}
	}
	if ctxWarn != 200_000 || ctxAlert != 400_000 || defaultCtxThresholds != (panelfmt.Thresholds{Warn: ctxWarn, Alert: ctxAlert}) {
		t.Errorf("the default thresholds moved: warn %d, alert %d", ctxWarn, ctxAlert)
	}
}

// TestCtxRowToneIsLoudOnlyWithAttention (FR6 as revised): above ctx_alert a
// row's badge is the soft hot tint, and the solid loud badge only when the
// session also has Attention. Attention changes no other tier.
func TestCtxRowToneIsLoudOnlyWithAttention(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		used      *int64
		attention bool
		want      ctxTone
	}{
		{n(400_001), false, ctxHot},
		{n(12_000_000), false, ctxHot},
		{n(400_001), true, ctxLoud},
		{n(12_000_000), true, ctxLoud},
		{n(400_000), true, ctxWarm},
		{n(50_000), true, ctxQuiet},
		{nil, true, ctxUnknown},
		{nil, false, ctxUnknown},
	} {
		if got := ctxRowTone(c.used, defaultCtxThresholds, c.attention); got != c.want {
			t.Errorf("ctxRowTone(%v, attention %v) = %v, want %v", ctxCount(c.used), c.attention, got, c.want)
		}
	}
	// Property: no count, however large, is loud without Attention.
	for v := int64(1); v < 1<<40; v = v*3/2 + 1 {
		if got := ctxRowTone(n(v), defaultCtxThresholds, false); got == ctxLoud {
			t.Fatalf("ctxRowTone(%d, no attention) is loud", v)
		}
	}
}

// TestCtxBadgeFmtTokens pins the formatter's edges and its 4-column bound.
func TestCtxBadgeFmtTokens(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{-5, "0"},
		{0, "0"},
		{850, "850"},
		{999, "999"},
		{1000, "1.0k"},
		{1049, "1.0k"},
		{1050, "1.1k"},
		{9_949, "9.9k"},
		{9_950, "10k"},
		{31_400, "31k"},
		{132_000, "132k"},
		{248_000, "248k"},
		{999_499, "999k"},
		{999_500, "1.0M"},
		{1_000_000, "1.0M"},
		{1_234_567, "1.2M"},
		{9_950_000, "10M"},
		{12_345_678, "12M"},
		{100_000_000, "100M"},
		{999_499_999, "999M"},
		{999_500_000, "1.0G"},
		{1 << 62, "999G"},
	} {
		if got := fmtTokens(c.in); got != c.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
	// Property: never wider than the badge's count column, over a sweep that
	// crosses every unit boundary.
	for v := int64(1); v < 1<<40; v = v*3/2 + 1 {
		if w := ansi.StringWidth(fmtTokens(v)); w > ctxCountCols {
			t.Fatalf("fmtTokens(%d) = %q is %d columns", v, fmtTokens(v), w)
		}
	}
}

// TestCtxBadgeIsSixColumnsInEveryTone: the right-hand column aligns whatever
// the tone, including "—" for unknown usage; only loud is bold (hot is not);
// only unknown has no fill.
func TestCtxBadgeIsSixColumnsInEveryTone(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		used *int64
		tone ctxTone
		text string
	}{
		{nil, ctxUnknown, "    — "},
		{n(850), ctxQuiet, "  850 "},
		{n(88_000), ctxQuiet, "  88k "},
		{n(248_000), ctxWarm, " 248k "},
		{n(431_000), ctxHot, " 431k "},
		{n(612_000), ctxLoud, " 612k "},
		{n(1_200_000), ctxLoud, " 1.2M "},
	} {
		seg := ctxBadge(c.used, c.tone)
		if len(seg.Parts) != 1 {
			t.Fatalf("ctxBadge has %d parts", len(seg.Parts))
		}
		if got := segText(seg); got != c.text {
			t.Errorf("ctxBadge text = %q, want %q", got, c.text)
		}
		if w := segWidth(seg); w != ctxBadgeWidth {
			t.Errorf("ctxBadge %q is %d wide, want %d", segText(seg), w, ctxBadgeWidth)
		}
		if seg.Empty || seg.Shrink != 0 || seg.Drop != 0 {
			t.Errorf("ctxBadge must be rigid and never Empty: %+v", seg)
		}
		st := seg.Parts[0].Style
		if st.GetBold() != (c.tone == ctxLoud) {
			t.Errorf("%v: bold = %v", c.tone, st.GetBold())
		}
		cc := ctxColoursFor(c.tone)
		if st.GetForeground() != cc.fg {
			t.Errorf("%v: foreground is not the tone's", c.tone)
		}
		_, noBg := st.GetBackground().(lipgloss.NoColor)
		if noBg == cc.hasFill {
			t.Errorf("%v: has fill = %v, want %v", c.tone, !noBg, cc.hasFill)
		}
	}
}

// TestCtxBadgePaintsTheToneInBothThemes renders each tone under truecolor:
// the loud badge is the coral fill with measured ink, the soft ones (hot
// included) their tinted fills, the unknown dash ash on the page.
func TestCtxBadgePaintsTheToneInBothThemes(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	used := map[ctxTone]*int64{ctxUnknown: nil, ctxQuiet: n(50_000), ctxWarm: n(300_000), ctxHot: n(500_000), ctxLoud: n(500_000)}
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			withPanelTheme(t, half.name == "dark")
			for _, tone := range ctxTones {
				out := renderPart(ctxBadge(used[tone], tone).Parts[0])
				c := ctxColoursFor(tone)
				if !strings.Contains(out, sgrRGB(t, 38, half.pick(c.fg))) {
					t.Errorf("%v: %q lacks the tone's foreground", tone, out)
				}
				if c.hasFill && !strings.Contains(out, sgrRGB(t, 48, half.pick(c.bg))) {
					t.Errorf("%v: %q lacks the tone's fill", tone, out)
				}
				if !c.hasFill && strings.Contains(out, "48;2;") {
					t.Errorf("%v: %q paints a fill", tone, out)
				}
			}
			loud := ctxColoursFor(ctxLoud)
			if half.pick(loud.bg) != half.pick(panelPal.coral) {
				t.Errorf("the loud badge is not coral")
			}
			hot := ctxColoursFor(ctxHot)
			if half.pick(hot.bg) == half.pick(panelPal.coral) || hot.bg == loud.bg {
				t.Errorf("the hot badge must be a coral tint, not the solid coral fill")
			}
		})
	}
}

// TestCtxBadgePlainHasNoFill: {ctx:plain} is the count alone in the tone's
// colour — the alert coral for hot and loud — with no fill and no padding.
func TestCtxBadgePlainHasNoFill(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		used *int64
		tone ctxTone
		text string
	}{
		{nil, ctxUnknown, unknownMark},
		{n(31_000), ctxQuiet, "31k"},
		{n(250_000), ctxWarm, "250k"},
		{n(431_000), ctxHot, "431k"},
		{n(1_200_000), ctxLoud, "1.2M"},
	} {
		seg := ctxPlain(c.used, c.tone)
		if got := segText(seg); got != c.text {
			t.Errorf("ctxPlain text = %q, want %q", got, c.text)
		}
		st := seg.Parts[0].Style
		if _, ok := st.GetBackground().(lipgloss.NoColor); !ok {
			t.Errorf("%v: ctxPlain has a fill", c.tone)
		}
		if st.GetForeground() != ctxColoursFor(c.tone).plainFg {
			t.Errorf("%v: ctxPlain is not in the tone's colour", c.tone)
		}
	}
	if ctxColoursFor(ctxLoud).plainFg != panelPal.alert || ctxColoursFor(ctxHot).plainFg != panelPal.alert {
		t.Error("hot and loud plain must be the alert coral")
	}
}

// TestChipAgeShort pins age:short (§3, #20): at most 3 columns at every edge.
func TestChipAgeShort(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{-1, unknownMark},
		{0, "0s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h"},
		{23*time.Hour + 59*time.Minute, "23h"},
		{day, "1d"},
		{99*day + 23*time.Hour, "99d"},
		{100 * day, "99d"},
		{10000 * day, "99d"},
	} {
		got := fmtAgeShort(c.d)
		if got != c.want {
			t.Errorf("fmtAgeShort(%v) = %q, want %q", c.d, got, c.want)
		}
		if w := ansi.StringWidth(got); w > chipAgeCols {
			t.Errorf("fmtAgeShort(%v) = %q is %d columns", c.d, got, w)
		}
	}
}

// TestChipGutterCell: always exactly one column, never Empty; the hue's glyph
// when the session has a colour, a blank cell when it has none; a glyph that
// is not one cell wide falls back to the default.
func TestChipGutterCell(t *testing.T) {
	blue := sessionHue("blue")
	for _, c := range []struct {
		h     panelHue
		glyph string
		want  string
	}{
		{blue, gutterGlyph, gutterGlyph},
		{blue, "|", "|"},
		{blue, "", gutterGlyph},
		{blue, "ab", gutterGlyph},
		{blue, "名", gutterGlyph},
		{panelHue{}, gutterGlyph, blankCell},
		{sessionHue("#5F87FF"), gutterGlyph, blankCell},
	} {
		seg := gutterCell(c.h, c.glyph)
		if got := segText(seg); got != c.want {
			t.Errorf("gutterCell(%q, %q) = %q, want %q", c.h.name, c.glyph, got, c.want)
		}
		if segWidth(seg) != 1 || seg.Empty {
			t.Errorf("gutterCell(%q, %q) must be one non-empty column: %+v", c.h.name, c.glyph, seg)
		}
	}
	if fg := gutterCell(blue, gutterGlyph).Parts[0].Style.GetForeground(); fg != blue.gutter {
		t.Errorf("the gutter is not drawn in the session's hue: %v", fg)
	}
}

// TestChipNameStyle: the soft tint when coloured, the body colour otherwise,
// slate for ENDED.
func TestChipNameStyle(t *testing.T) {
	if fg := nameStyle(sessionHue("green")).GetForeground(); fg != sessionHue("green").tint {
		t.Errorf("a coloured name is %v, want its tint", fg)
	}
	if fg := nameStyle(panelHue{}).GetForeground(); fg != panelPal.cloud {
		t.Errorf("an uncoloured name is %v, want cloud", fg)
	}
	if fg := endedNameStyle.GetForeground(); fg != panelPal.slate {
		t.Errorf("an ended name is %v, want slate", fg)
	}
}

// TestChipHairline: n rule cells in the rule colour, nothing for n ≤ 0.
func TestChipHairline(t *testing.T) {
	withPanelTheme(t, true)
	for _, n := range []int{-3, 0} {
		if got := hairline(n); got != "" {
			t.Errorf("hairline(%d) = %q, want empty", n, got)
		}
	}
	got := hairline(5)
	if plain := ansi.Strip(got); plain != strings.Repeat(hairlineGlyph, 5) {
		t.Errorf("hairline(5) = %q", plain)
	}
	if !strings.Contains(got, sgrRGB(t, 38, panelPal.rule.Dark)) {
		t.Errorf("hairline is not in the rule colour: %q", got)
	}
}

// TestChipFormatOptions: the engine's own styles are palette tokens.
func TestChipFormatOptions(t *testing.T) {
	o := panelFormatOptions()
	for _, c := range []struct {
		what string
		got  lipgloss.TerminalColor
		want lipgloss.AdaptiveColor
	}{
		{"literal", o.LiteralStyle.GetForeground(), panelPal.slate},
		{"dim", o.DimStyle.GetForeground(), panelPal.ash},
		{"error", o.ErrorStyle.GetForeground(), panelPal.alert},
	} {
		if c.got != c.want {
			t.Errorf("%s style is %v, want %+v", c.what, c.got, c.want)
		}
	}
}
