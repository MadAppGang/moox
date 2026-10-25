package main

// panel_chips.go holds the panel's drawing primitives: the status chip, the
// context badge, the session gutter and name style, and the heading hairline.
//
// They are pure and know nothing about sessions. The token table (Phase E)
// decides WHICH chip a row gets; these decide only what that chip looks like,
// and they return panelfmt.Segment values the template engine fits and joins.
// Every colour comes from theme_panel.go by name; none is spelled here.
//
// Text is padded in the Part itself and styles carry colour only (no Width or
// Padding), so the engine measures exactly the cells that will be painted.

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// The context badge's default edges, in tokens (FR6, §4.3): below ctxWarn is
// quiet, ctxWarn..ctxAlert inclusive is warm, above ctxAlert is hot — and loud
// only on a row whose session has Attention.
// panel.conf's ctx_warn / ctx_alert override them.
const (
	ctxWarn  int64 = 200_000
	ctxAlert int64 = 400_000
)

// defaultCtxThresholds are the built-in edges as the engine's type, which is
// what panel.conf parsing produces too.
var defaultCtxThresholds = panelfmt.Thresholds{Warn: ctxWarn, Alert: ctxAlert}

// The fixed geometry of the chip and the badge (§6.3, §6.5). Fixed widths are
// what keep names and the right-hand column aligned from row to row.
const (
	chipLabelCols = 4                       // the status word, left-aligned
	chipAgeCols   = 3                       // age:short, right-aligned
	chipLabelW    = 1 + chipLabelCols + 1   // " WAIT "
	chipValueW    = 1 + chipAgeCols + 1     // " 13m "
	chipWidth     = chipLabelW + chipValueW // 11
	iconCols      = panelfmt.IconMaxCols    // the status icon's slot: an emoji is 2
	ctxCountCols  = 4                       // fmtTokens never exceeds it
	ctxBadgeWidth = 1 + ctxCountCols + 1    // " 132k " = 6
	cellPad       = " "                     // one cell either side of a chip half or the badge

	// The status badge (feedback 3): " RUN " then " 3h12m ", no gap.
	badgeCodeCols    = 3                           // the status code, left-aligned
	badgeRuntimeCols = 5                           // fmtRuntime, right-aligned
	badgeLabelW      = 1 + badgeCodeCols + 1       // 5
	badgeValueW      = 1 + badgeRuntimeCols + 1    // 7
	badgeWidth       = badgeLabelW + badgeValueW   // 12; {status:code} is the label width, {runtime:badge} the value width
	ctxBarCells      = 5                           // ▰▰▱▱▱
	ctxBarWidth      = 1 + ctxBarCells             // the gap before the bar, and the bar
	ctxWidth         = ctxBadgeWidth + ctxBarWidth // {ctx}: badge and bar, 12
	ctxBarFull       = "▰"                         // a filled bar cell (U+25B0)
	ctxBarEmpty      = "▱"                         // an empty bar cell (U+25B1)
	runtimeMaxDays   = 999                         // fmtRuntime caps at "999d"
	runtimeMinuteMax = 10                          // below 10h: 3h12m; from 10h: 13h
	runtimeHourMax   = 10                          // below 10d: 6d02h; from 10d: 12d

	gutterGlyph   = "▌" // the session-colour gutter
	blankCell     = " " // the gutter of a session with no colour
	hairlineGlyph = "─" // the heading rule
	unknownMark   = "—" // "not measured", never a fake 0

	ageShortMaxDays = 99 // age:short caps at "99d"

	tokenUnitStep = 1000  // k, M, G
	tokenUnits    = "kMG" // the suffixes fmtTokens uses
	tenthsLimit   = 100   // below 10 of a unit, fmtTokens shows one decimal

	tokensCap     = 999_500_000_000 // the first count that would round past 999G
	tokensCapText = "999G"          // what fmtTokens shows at or beyond it
)

// chipStyle is one tone's two pre-built half styles.
type chipStyle struct{ label, value lipgloss.Style }

// chipStyles and ctxStyles are built once from the colour tables. Styles hold
// AdaptiveColor pairs, which lipgloss resolves per render, so building them at
// init costs no correctness when the theme is decided later.
var (
	chipStyles  = buildChipStyles()
	ctxStyles   = buildCtxStyles()
	ageStyles   = buildAgeStyles()
	badgeStyles = buildBadgeStyles()
)

// buildBadgeStyles gives each status badge tone its two half styles. A loud
// badge is bold throughout, like the loud chip; a soft one is not bold at all,
// so a calm row stays calm.
func buildBadgeStyles() map[badgeTone]chipStyle {
	m := make(map[badgeTone]chipStyle, len(badgeTones))
	for _, t := range badgeTones {
		c := badgeColoursFor(t)
		m[t] = chipStyle{
			label: lipgloss.NewStyle().Foreground(c.labelFg).Background(c.labelBg).Bold(t.loud()),
			value: lipgloss.NewStyle().Foreground(c.valueFg).Background(c.valueBg).Bold(t.loud()),
		}
	}
	return m
}

// badgeStyleFor returns a tone's styles; an unknown tone draws unclear.
func badgeStyleFor(t badgeTone) chipStyle {
	if s, ok := badgeStyles[t]; ok {
		return s
	}
	return badgeStyles[badgeUnclear]
}

// buildAgeStyles gives each chip tone its plain-text age style: the loud
// tones bold in their hue, the calm ones slate (ageColourFor).
func buildAgeStyles() map[chipTone]lipgloss.Style {
	m := make(map[chipTone]lipgloss.Style, len(chipTones))
	for _, t := range chipTones {
		m[t] = lipgloss.NewStyle().Foreground(ageColourFor(t)).Bold(t.loud())
	}
	return m
}

// ageStyleFor returns a tone's age style; an unknown tone draws calm.
func ageStyleFor(t chipTone) lipgloss.Style {
	if s, ok := ageStyles[t]; ok {
		return s
	}
	return ageStyles[chipDim]
}

func buildChipStyles() map[chipTone]chipStyle {
	m := make(map[chipTone]chipStyle, len(chipTones))
	for _, t := range chipTones {
		c := chipColoursFor(t)
		m[t] = chipStyle{
			// A loud chip is bold throughout. A dim chip bolds only its age, so
			// the status word stays quiet while the number still reads.
			label: lipgloss.NewStyle().Foreground(c.labelFg).Background(c.labelBg).Bold(t.loud()),
			value: lipgloss.NewStyle().Foreground(c.valueFg).Background(c.valueBg).Bold(true),
		}
	}
	return m
}

// ctxStyle is one ctx tone's badge style and its no-fill {ctx:plain} style.
type ctxStyle struct{ badge, plain lipgloss.Style }

func buildCtxStyles() map[ctxTone]ctxStyle {
	m := make(map[ctxTone]ctxStyle, len(ctxTones))
	for _, t := range ctxTones {
		c := ctxColoursFor(t)
		loud := t == ctxLoud
		badge := lipgloss.NewStyle().Foreground(c.fg).Bold(loud)
		if c.hasFill {
			badge = badge.Background(c.bg)
		}
		m[t] = ctxStyle{badge: badge, plain: lipgloss.NewStyle().Foreground(c.plainFg).Bold(loud)}
	}
	return m
}

// chipStyleFor returns a tone's styles; an unknown tone draws dim.
func chipStyleFor(t chipTone) chipStyle {
	if s, ok := chipStyles[t]; ok {
		return s
	}
	return chipStyles[chipDim]
}

// ---------------------------------------------------------------------------
// Status chip
// ---------------------------------------------------------------------------

// statusChip is the two-half chip that opens every row: ` WAIT ` then ` 13m `,
// always chipWidth (11) columns so names align. There is no divider glyph; the
// halves are two fills, as in madbench's splitBadge. label is cut or padded to
// 4 columns, age (normally fmtAgeShort's output) right-aligned in 3.
//
// It is rigid: never shrunk, never dropped, so a row's status survives any
// width the engine is asked to fit.
func statusChip(label, age string, tone chipTone) panelfmt.Segment {
	s := chipStyleFor(tone)
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: cellPad + alignLeft(label, chipLabelCols) + cellPad, Style: s.label},
		{Text: cellPad + alignRight(age, chipAgeCols) + cellPad, Style: s.value},
	}}
}

// statusChipLabel is the chip's label half alone ({status:noage}): 6 columns.
func statusChipLabel(label string, tone chipTone) panelfmt.Segment {
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: cellPad + alignLeft(label, chipLabelCols) + cellPad, Style: chipStyleFor(tone).label},
	}}
}

// statusIconCell is {status:icon}: the glyph in a fixed iconCols (2) slot, so
// names align whether a status draws a two-column emoji or a one-column
// glyph from panel.conf (padded on the right). It is unstyled: an emoji
// carries its own colour, and a configured text glyph takes the terminal's.
// Attention is said by the age beside it and by the band, not by the icon.
// Like the chip it is rigid, never shrunk and never dropped.
func statusIconCell(glyph string) panelfmt.Segment {
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: alignLeft(glyph, iconCols), Style: lipgloss.NewStyle()}}}
}

// ---------------------------------------------------------------------------
// Status badge (feedback 3)
// ---------------------------------------------------------------------------

// statusBadge is {status:badge}: the madbench split badge — a three-letter
// status code on the label half (" RUN ", 5 columns) and the session's
// runtime on the value half (" 3h12m ", 7 columns, right-aligned), no divider
// and no gap. Always badgeWidth (12) columns, so names align; rigid, never
// shrunk and never dropped. code is cut or padded to 3 columns, runtime
// (fmtRuntime's output) to 5.
func statusBadge(code, runtime string, tone badgeTone) panelfmt.Segment {
	s := badgeStyleFor(tone)
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: cellPad + alignLeft(code, badgeCodeCols) + cellPad, Style: s.label},
		{Text: cellPad + alignRight(runtime, badgeRuntimeCols) + cellPad, Style: s.value},
	}}
}

// statusCodeBadge is {status:code} (feedback 4, layout L2): the three-letter
// code alone as its own badge, " BLK " (badgeLabelW, 5 columns). It is the
// design sheet's status family A: a loud tone (Attention) is the SOLID hue
// with its contrast ink, bold — the loud badge's value style — and every
// other tone the soft tint of its own hue (the split badge's label half).
// Rigid, like the split badge.
func statusCodeBadge(code string, tone badgeTone) panelfmt.Segment {
	s := badgeStyleFor(tone)
	style := s.label
	if tone.loud() {
		style = s.value
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: cellPad + alignLeft(code, badgeCodeCols) + cellPad, Style: style},
	}}
}

// runtimeBadge is {runtime:badge} (feedback 4, layout L2): the runtime
// right-aligned in a fixed badgeValueW (7) columns, so the names after it
// align. A loud tone paints it exactly as the split badge's loud value half
// (the solid hue, bold); every other tone gets the one neutral value fill with
// body text, so the runtime reads as a value, not as a second status.
// runtime is fmtRuntime's output ("—" when there is no start time). Rigid.
func runtimeBadge(runtime string, tone badgeTone) panelfmt.Segment {
	style := runtimeNeutralStyle
	if tone.loud() {
		style = badgeStyleFor(tone).value
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: cellPad + alignRight(runtime, badgeRuntimeCols) + cellPad, Style: style},
	}}
}

// runtimeNeutralStyle is a calm row's runtime badge: body text on the neutral
// value fill (runtimeNeutralColours).
var runtimeNeutralStyle = func() lipgloss.Style {
	c := runtimeNeutralColours()
	return lipgloss.NewStyle().Foreground(c.fg).Background(c.bg)
}()

// fmtRuntime is how long a session has been running, in at most 5 columns:
// 42s, 17m, 3h12m below ten hours, 13h below a day, 6d02h below ten days,
// 12d beyond, capped at 999d. A negative duration is "no start time" and is
// "—", never a fake 0.
func fmtRuntime(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d < 0:
		return unknownMark
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < runtimeMinuteMax*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h" + twoDigits(int(d/time.Minute)%60) + "m"
	case d < day:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d < runtimeHourMax*day:
		return strconv.Itoa(int(d/day)) + "d" + twoDigits(int(d/time.Hour)%24) + "h"
	case d < (runtimeMaxDays+1)*day:
		return strconv.Itoa(int(d/day)) + "d"
	}
	return strconv.Itoa(runtimeMaxDays) + "d"
}

// twoDigits is n zero-padded to two digits (n is 0..59).
func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// ---------------------------------------------------------------------------
// Name badge (feedback 3)
// ---------------------------------------------------------------------------

// nameBadge is {name:badge}: a session with a /color shows its name on a soft
// badge of that hue (" name ", the tint on the hue's sunk fill); a session
// without one shows " name" in the body colour with no fill, and an ENDED row
// (ended) " name" in slate. The leading cell is always there, so the name's
// first letter sits in the same column whether or not the row is coloured.
func nameBadge(name string, h panelHue, ended bool) panelfmt.Segment {
	switch {
	case ended:
		return panelfmt.Segment{Parts: []panelfmt.Part{{Text: cellPad + name, Style: endedNameStyle}}}
	case !h.known():
		return panelfmt.Segment{Parts: []panelfmt.Part{{Text: cellPad + name, Style: panelNameBodyStyle}}}
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: cellPad + name + cellPad, Style: nameBadgeStyle(h)}}}
}

// nameBadgeStyle is a hue's name badge style.
func nameBadgeStyle(h panelHue) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(h.badgeFg).Background(h.badgeBg)
}

// fmtAgeShort is age:short (§3, #20), at most 3 columns: Ns under a minute, Nm
// under an hour, Nh under a day, Nd under 100 days, and 99d beyond. A negative
// duration is panelAge's "no statusSince" and renders as "—".
func fmtAgeShort(d time.Duration) string {
	switch {
	case d < 0:
		return unknownMark
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d < (ageShortMaxDays+1)*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strconv.Itoa(ageShortMaxDays) + "d"
}

// ---------------------------------------------------------------------------
// Context badge
// ---------------------------------------------------------------------------

// ctxToneOf classifies a measured context size against the thresholds. nil is
// "not measured" and is never read as zero. It never returns ctxLoud: that
// takes the row's Attention too (ctxRowTone).
func ctxToneOf(used *int64, th panelfmt.Thresholds) ctxTone {
	switch {
	case used == nil:
		return ctxUnknown
	case *used < th.Warn:
		return ctxQuiet
	case *used <= th.Alert:
		return ctxWarm
	}
	return ctxHot
}

// ctxRowTone is the tone a row's badge is drawn in: ctxToneOf, except that a
// hot count on a row whose session has Attention goes loud. A solid fill thus
// always means "act now"; a big context alone never shouts over BLCK/WAIT.
func ctxRowTone(used *int64, th panelfmt.Thresholds, attention bool) ctxTone {
	tone := ctxToneOf(used, th)
	if tone == ctxHot && attention {
		return ctxLoud
	}
	return tone
}

// ctxBadge is {ctx}: the used-token count right-aligned in 4 columns with one
// cell of padding either side, filled in its tone (§6.5). Unknown usage is "—"
// in ash with no fill, in the same 6 columns so the column still aligns. Only
// the loud tone is bold. The caller picks the tone (ctxRowTone), as it picks
// a status chip's.
func ctxBadge(used *int64, tone ctxTone) panelfmt.Segment {
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: cellPad + alignRight(ctxCount(used), ctxCountCols) + cellPad, Style: ctxStyleFor(tone).badge},
	}}
}

// ctxPlain is {ctx:plain}: the bare count in the tone's colour, no fill and no
// padding — for one-line templates that want the number without the block.
func ctxPlain(used *int64, tone ctxTone) panelfmt.Segment {
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: ctxCount(used), Style: ctxStyleFor(tone).plain},
	}}
}

// ctxBarFilled is how many of the bar's cells a count fills against a model's
// window: the share rounded half-up to fifths, clamped to [0, ctxBarCells],
// so a count past the window is a full bar, never an overflow.
func ctxBarFilled(used, window int64) int {
	if used <= 0 || window <= 0 {
		return 0
	}
	if used >= window {
		return ctxBarCells
	}
	return int((used*ctxBarCells*2 + window) / (window * 2))
}

// ctxBar is the context bar: ctxBarCells cells, the filled ones in the
// count's tier colour (the tone's plain colour), the empty ones in ash. No
// fill: the bar is text. Two parts, so the tier colour stops where the share
// does.
func ctxBar(used, window int64, tone ctxTone) []panelfmt.Part {
	n := ctxBarFilled(used, window)
	var parts []panelfmt.Part
	if n > 0 {
		parts = append(parts, panelfmt.Part{Text: strings.Repeat(ctxBarFull, n), Style: ctxBarFullStyle(tone)})
	}
	if n < ctxBarCells {
		parts = append(parts, panelfmt.Part{Text: strings.Repeat(ctxBarEmpty, ctxBarCells-n), Style: ctxBarEmptyStyle})
	}
	return parts
}

// ctxBarFullStyle is a tone's filled-cell style: its plain colour, not bold.
func ctxBarFullStyle(t ctxTone) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(ctxColoursFor(t).plainFg)
}

// ctxBarEmptyStyle paints the empty cells.
var ctxBarEmptyStyle = lipgloss.NewStyle().Foreground(ctxBarEmptyColour())

// ctxWithBar is {ctx}'s built-in form: the badge, a gap, and the bar filled
// against the model's window. When the window is not known (no rule matches
// the model, or no model) or the count was not measured, the bar's columns
// are blank, so the counts of every row still line up. window is 0 when not
// known.
func ctxWithBar(used *int64, window int64, tone ctxTone) panelfmt.Segment {
	seg := ctxBadge(used, tone)
	if used == nil || window <= 0 {
		seg.Parts = append(seg.Parts, panelfmt.Part{Text: strings.Repeat(" ", ctxBarWidth), Style: lipgloss.NewStyle()})
		return seg
	}
	seg.Parts = append(seg.Parts, panelfmt.Part{Text: cellPad, Style: lipgloss.NewStyle()})
	seg.Parts = append(seg.Parts, ctxBar(*used, window, tone)...)
	return seg
}

// ctxStyleFor returns a tone's styles; an unknown tone draws as not measured.
func ctxStyleFor(t ctxTone) ctxStyle {
	if s, ok := ctxStyles[t]; ok {
		return s
	}
	return ctxStyles[ctxUnknown]
}

// ctxCount is the badge's text: fmtTokens, or "—" when not measured.
func ctxCount(used *int64) string {
	if used == nil {
		return unknownMark
	}
	return fmtTokens(*used)
}

// fmtTokens renders a token count in at most 4 columns: the number itself
// below 1000 (850), one decimal below ten units (1.2k, 1.2M), whole units
// otherwise (31k, 248k, 12M). Rounding is half-up, and a value that rounds to
// 1000 of a unit moves to the next one (999_500 is 1.0M, not 1000k). A
// negative count cannot be measured and is shown as 0.
func fmtTokens(n int64) string {
	if n < 0 {
		n = 0
	}
	if n < tokenUnitStep {
		return strconv.FormatInt(n, 10)
	}
	// Beyond the last unit: no transcript gets here (transcriptfs caps
	// plausible counts at 100M), but the 4-column bound must hold for any
	// input, and capping first keeps n*10 below overflow.
	if n >= tokensCap {
		return tokensCapText
	}
	unit := int64(1)
	for _, suffix := range tokenUnits {
		unit *= tokenUnitStep
		if tenths := (n*10 + unit/2) / unit; tenths < tenthsLimit {
			return strconv.FormatInt(tenths/10, 10) + "." + strconv.FormatInt(tenths%10, 10) + string(suffix)
		}
		if whole := (n + unit/2) / unit; whole < tokenUnitStep {
			return strconv.FormatInt(whole, 10) + string(suffix)
		}
	}
	return tokensCapText // unreachable below tokensCap; kept for the width bound
}

// ---------------------------------------------------------------------------
// Session colour, and chrome
// ---------------------------------------------------------------------------

// gutterCell is {gutter}: glyph (normally "▌") in the session's hue, or a
// blank cell when it has no colour. It is always exactly one column and is
// never Empty, so turning a colour on or off never shifts a row. A glyph that
// is not exactly one cell wide falls back to the default.
func gutterCell(h panelHue, glyph string) panelfmt.Segment {
	if ansi.StringWidth(glyph) != 1 {
		glyph = gutterGlyph
	}
	if !h.known() {
		return panelfmt.Segment{Parts: []panelfmt.Part{{Text: blankCell, Style: lipgloss.NewStyle()}}}
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: glyph, Style: lipgloss.NewStyle().Foreground(h.gutter)},
	}}
}

// nameStyle is the session name's style: the soft tint of its hue, or the
// body colour when it has no colour. ENDED rows use endedNameStyle instead,
// keeping the gutter but receding the name.
func nameStyle(h panelHue) lipgloss.Style {
	if !h.known() {
		return panelNameBodyStyle
	}
	return lipgloss.NewStyle().Foreground(h.tint)
}

// The name's two fixed styles.
var (
	panelNameBodyStyle = lipgloss.NewStyle().Foreground(panelPal.cloud)
	endedNameStyle     = lipgloss.NewStyle().Foreground(panelPal.slate)
	hairlineStyle      = lipgloss.NewStyle().Foreground(panelPal.rule)
)

// hairline is n cells of the heading rule, in the rule colour; "" for n ≤ 0.
func hairline(n int) string {
	if n <= 0 {
		return ""
	}
	return hairlineStyle.Render(strings.Repeat(hairlineGlyph, n))
}

// panelFormatOptions are the styles the template engine draws with itself:
// literals in slate (R7), the `dim` option in ash, and an unknown {foo?} in
// the alert coral.
func panelFormatOptions() panelfmt.Options {
	return panelfmt.Options{
		LiteralStyle: lipgloss.NewStyle().Foreground(panelPal.slate),
		DimStyle:     lipgloss.NewStyle().Foreground(panelPal.ash),
		ErrorStyle:   lipgloss.NewStyle().Foreground(panelPal.alert),
	}
}

// ---------------------------------------------------------------------------
// Fixed-width text
// ---------------------------------------------------------------------------

// alignLeft cuts s to n display columns and pads it on the right to exactly n.
func alignLeft(s string, n int) string {
	s = ansi.Truncate(s, n, "")
	return s + strings.Repeat(" ", n-ansi.StringWidth(s))
}

// alignRight cuts s to n display columns and pads it on the left to exactly n.
func alignRight(s string, n int) string {
	s = ansi.Truncate(s, n, "")
	return strings.Repeat(" ", n-ansi.StringWidth(s)) + s
}
