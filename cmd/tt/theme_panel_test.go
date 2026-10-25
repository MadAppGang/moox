package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The test pages (§6.1, #25) are the palette's own design pages, pinned here
// so a page cannot quietly drop out of the list the floors are held on.
var (
	panelDarkPages  = panelDarkBase.pages
	panelLightPages = panelLightBase.pages
)

func TestPanelPaletteTestPagesArePinned(t *testing.T) {
	want := map[string]string{
		"dark":  "#000000,#0B0B12,#1E1E2E,#282A36",
		"light": "#FFFFFF,#F5EFC8,#F2F2F2,#F9F6DA",
	}
	for _, half := range panelHalves {
		if got := strings.Join(half.pages, ","); got != want[half.name] {
			t.Errorf("%s pages = %s, want %s", half.name, got, want[half.name])
		}
	}
}

// panelHalf is one background of the palette, for tests that walk both.
type panelHalf struct {
	name  string
	pages []string
	pick  func(lipgloss.AdaptiveColor) string
}

var panelHalves = []panelHalf{
	{name: "dark", pages: panelDarkPages, pick: func(c lipgloss.AdaptiveColor) string { return c.Dark }},
	{name: "light", pages: panelLightPages, pick: func(c lipgloss.AdaptiveColor) string { return c.Light }},
}

// parseTestHex parses #RRGGBB, failing the test on anything else: an empty or
// malformed half must never be scored as some default colour and "pass".
func parseTestHex(t *testing.T, what, h string) {
	t.Helper()
	if len(h) != 7 || h[0] != '#' {
		t.Fatalf("%s: %q is not #RRGGBB", what, h)
	}
	if _, err := strconv.ParseUint(h[1:], 16, 32); err != nil {
		t.Fatalf("%s: %q is not #RRGGBB: %v", what, h, err)
	}
}

// TestPanelPaletteContrastMeetsTheFloors is FR1/C7: every foreground the
// panel's primitives paint clears its WCAG floor — text 4.5:1, dim chrome 3:1,
// the hairline 1.3:1 — on its own fill, or on every test page of its theme,
// in BOTH themes. Scored with contrastOnScreen: the worse of the palette hex
// and the colour termenv actually paints. The pairs come from panelContrastPairs, which is built from
// the tables the chips and the badge draw with, so the renderer's own values
// are what is scored. The measured minimums are logged (go test -v) for the
// implementation log.
func TestPanelPaletteContrastMeetsTheFloors(t *testing.T) {
	pairs := panelContrastPairs()
	// Guard against a vacuous pass: every chip tone has two halves, every ctx
	// tone at least one pair, and every hue a gutter and a tint.
	if min := 2*len(chipTones) + len(ctxTones) + 2*len(sortedHueNames()); len(pairs) < min {
		t.Fatalf("panelContrastPairs has %d pairs, want at least %d", len(pairs), min)
	}
	// The soft hot badge is text on its own tint and must be scored as such.
	hotScored := false
	for _, p := range pairs {
		hotScored = hotScored || (p.name == "ctx hot badge" && !p.onPage && p.floor == contrastText)
	}
	if !hotScored {
		t.Fatal("the ctx hot badge is not scored against its fill at the text floor")
	}
	// The plain age carries the attention cue now that the status is an
	// uncoloured icon (feedback 2): its loud tones are text on the page, and
	// scored as such; its calm tone is dim chrome. The icon itself is an
	// emoji painted in its own colours, so it has no pair.
	required := map[string]float64{
		"age " + chipHoney.String(): contrastText,
		"age " + chipCoral.String(): contrastText,
		"age " + chipDim.String():   contrastChrome,
		"ctx bar empty":             contrastChrome,
	}
	// Feedback 3: both halves of every status badge, and the name badge of
	// every /color, are text on their own fill.
	onFill := map[string]bool{}
	for _, bt := range badgeTones {
		for _, half := range []string{" label", " value"} {
			required["badge "+bt.String()+half] = contrastText
			onFill["badge "+bt.String()+half] = true
		}
	}
	for _, name := range sortedHueNames() {
		required["name badge "+name] = contrastText
		onFill["name badge "+name] = true
	}
	for name, floor := range required {
		scored := false
		for _, p := range pairs {
			scored = scored || (p.name == name && p.onPage != onFill[name] && p.floor == floor)
		}
		if !scored {
			t.Fatalf("%q is not scored (on its fill: %v) at the %.1f floor", name, onFill[name], floor)
		}
	}

	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			scored := 0
			for _, p := range pairs {
				fg := half.pick(p.fg)
				parseTestHex(t, p.name+" fg", fg)
				bgs := half.pages
				where := "worst page"
				if !p.onPage {
					bg := half.pick(p.bg)
					parseTestHex(t, p.name+" bg", bg)
					bgs = []string{bg}
					where = "on " + bg
				}
				worst, worstBg := math.Inf(1), ""
				for _, bg := range bgs {
					if r := contrastOnScreen(fg, bg); r < worst {
						worst, worstBg = r, bg
					}
					scored++
				}
				t.Logf("%-26s %s  %5.2f  (floor %.1f, %s %s)", p.name, fg, worst, p.floor, where, worstBg)
				if worst < p.floor {
					t.Errorf("%s (%s): %s on %s is %.2f:1, below the %.1f:1 floor", half.name, p.name, fg, worstBg, worst, p.floor)
				}
			}
			if scored == 0 {
				t.Fatal("no pair was scored")
			}
		})
	}
}

// TestPanelPaletteEveryTokenHasBothHalves: lipgloss resolves a missing half of
// an AdaptiveColor to "no colour", which is how a dark-only palette silently
// turns into mush on a light terminal. Every token, derived colour and hue
// must carry a well-formed colour for both backgrounds.
func TestPanelPaletteEveryTokenHasBothHalves(t *testing.T) {
	p := panelPal
	tokens := map[string]lipgloss.AdaptiveColor{
		"ink": p.ink, "snow": p.snow, "cloud": p.cloud, "slate": p.slate, "ash": p.ash,
		"rule": p.rule, "sky": p.sky, "azure": p.azure, "moss": p.moss, "honey": p.honey,
		"coral": p.coral, "rose": p.rose, "keyBg": p.keyBg, "valBg": p.valBg,
		"chipLabel": p.chipLabel, "valFg": p.valFg,
		"honeyLabelFg": p.honeyLabelFg, "honeyLabelBg": p.honeyLabelBg, "honeyValueFg": p.honeyValueFg,
		"coralLabelFg": p.coralLabelFg, "coralLabelBg": p.coralLabelBg, "coralValueFg": p.coralValueFg,
		"busyLabel": p.busyLabel, "busyBg": p.busyBg, "failLabel": p.failLabel,
		"ctxQuietFg": p.ctxQuietFg, "ctxQuietBg": p.ctxQuietBg,
		"ctxWarmFg": p.ctxWarmFg, "ctxWarmBg": p.ctxWarmBg, "ctxHotFg": p.ctxHotFg, "ctxHotBg": p.ctxHotBg,
		"ctxLoudFg": p.ctxLoudFg,
		"alert":     p.alert,
	}
	tokens["lavender"], tokens["teal"] = p.lavender, p.teal
	for name, h := range p.hues {
		tokens["gutter "+name] = h.gutter
		tokens["tint "+name] = h.tint
		tokens["name badge fg "+name] = h.badgeFg
		tokens["name badge bg "+name] = h.badgeBg
	}
	for _, t := range badgeTones {
		c := badgeColoursFor(t)
		tokens["badge "+t.String()+" label fg"] = c.labelFg
		tokens["badge "+t.String()+" label bg"] = c.labelBg
		tokens["badge "+t.String()+" value fg"] = c.valueFg
		tokens["badge "+t.String()+" value bg"] = c.valueBg
	}
	for name, c := range tokens {
		parseTestHex(t, name+" (dark)", c.Dark)
		parseTestHex(t, name+" (light)", c.Light)
	}
	// Only the two inks may be the same on both backgrounds (snow is white
	// everywhere); any other identical pair is a colour that was not re-voiced.
	for name, c := range tokens {
		if c.Light == c.Dark && name != "snow" {
			t.Errorf("%s is %s on both backgrounds; a colour that works on one rarely works on the other", name, c.Dark)
		}
	}
}

// preTemplateAccents are the literal accents the panel drew before Phase E
// moved panelAccents onto sessionHue. Pinned here, not read from panelAccents
// (which is now derived from the hue table, so comparing the two would prove
// nothing): the gutter colours must stay exactly these.
var preTemplateAccents = map[string]lipgloss.AdaptiveColor{
	"blue":   {Light: "#1D4ED8", Dark: "#5F87FF"},
	"green":  {Light: "#116B36", Dark: "#5FD75F"},
	"cyan":   {Light: "#0E6C77", Dark: "#5FD7D7"},
	"purple": {Light: "#6A28B5", Dark: "#AF87FF"},
	"red":    {Light: "#C41E3A", Dark: "#FF5F5F"},
	"pink":   {Light: "#B5179E", Dark: "#FF87D7"},
	"orange": {Light: "#A2521A", Dark: "#FFAF5F"},
	"yellow": {Light: "#8A6A00", Dark: "#FFD75F"},
}

// TestPanelPaletteHueTableIsComplete pins the session-hue table to the eight
// /color names, keeps sortedHueNames honest, and holds the gutters (and so
// panelAccents, derived from them) equal to the accents the panel drew before
// Phase E, so moving panelAccents onto sessionHue changed no gutter colour.
func TestPanelPaletteHueTableIsComplete(t *testing.T) {
	names := sortedHueNames()
	var keys []string
	for k := range panelPal.hues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append([]string(nil), names...)
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("hue table has %v, sortedHueNames has %v", keys, want)
	}
	if len(panelAccents) != len(names) {
		t.Fatalf("panelAccents has %d colours, the hue table %d", len(panelAccents), len(names))
	}
	for _, name := range names {
		h := sessionHue(name)
		if !h.known() {
			t.Fatalf("sessionHue(%q) is unknown", name)
		}
		if h.gutter != preTemplateAccents[name] {
			t.Errorf("gutter %s = %+v, the pre-template accent is %+v", name, h.gutter, preTemplateAccents[name])
		}
		if panelAccents[name] != preTemplateAccents[name] {
			t.Errorf("panelAccents[%s] = %+v, the pre-template accent is %+v", name, panelAccents[name], preTemplateAccents[name])
		}
	}
	for _, in := range []string{"  Blue ", "BLUE"} {
		if got := sessionHue(in); got.gutter != sessionHue("blue").gutter {
			t.Errorf("sessionHue(%q) did not normalise to blue", in)
		}
	}
	for _, in := range []string{"", "teal", "#5F87FF", "blue-ish"} {
		if sessionHue(in).known() {
			t.Errorf("sessionHue(%q) is known; an unrecognised colour must be no colour", in)
		}
	}
}

// TestPanelPaletteMatchesTheSpecTables cross-checks the derived colours
// against the values architecture §6.2/§6.3/§6.5 were measured with. The spec
// tables were computed with per-channel truncation; go-colorful rounds, so
// each channel may differ by one step and no more. A larger drift means a
// recipe changed without the spec (and its measurements) being revisited.
func TestPanelPaletteMatchesTheSpecTables(t *testing.T) {
	p := panelPal
	cases := []struct {
		name string
		got  lipgloss.AdaptiveColor
		want lipgloss.AdaptiveColor
	}{
		{"loud ask label bg", p.honeyLabelBg, lipgloss.AdaptiveColor{Dark: "#4B3F70", Light: "#E5D8F8"}},
		{"loud ask label fg", p.honeyLabelFg, lipgloss.AdaptiveColor{Dark: "#FFFFFF", Light: "#441987"}},
		{"loud coral label bg", p.coralLabelBg, lipgloss.AdaptiveColor{Dark: "#72222A", Light: "#F2D6D6"}},
		{"loud coral label fg", p.coralLabelFg, lipgloss.AdaptiveColor{Dark: "#FFFFFF", Light: "#721111"}},
		{"loud honey value fg", p.honeyValueFg, lipgloss.AdaptiveColor{Dark: "#0B0B12", Light: "#FFFFFF"}},
		{"loud coral value fg", p.coralValueFg, lipgloss.AdaptiveColor{Dark: "#0B0B12", Light: "#FFFFFF"}},
		{"busy fill", p.busyBg, lipgloss.AdaptiveColor{Dark: "#30394A", Light: "#CED6ED"}},
		{"ctx quiet", p.ctxQuietFg, lipgloss.AdaptiveColor{Dark: "#88BEA3", Light: "#036649"}},
		{"ctx quiet fill", p.ctxQuietBg, lipgloss.AdaptiveColor{Dark: "#313E3F", Light: "#E0EEEA"}},
		{"ctx warm", p.ctxWarmFg, lipgloss.AdaptiveColor{Dark: "#FFB454", Light: "#6D300A"}},
		{"ctx warm fill", p.ctxWarmBg, lipgloss.AdaptiveColor{Dark: "#4C3619", Light: "#E9D8CE"}},
		{"ctx loud", p.ctxLoudFg, lipgloss.AdaptiveColor{Dark: "#0B0B12", Light: "#FFFFFF"}},
		// The soft hot tier (FR6 revised after the first real render), pinned
		// at the values its contrast was measured with.
		{"ctx hot", p.ctxHotFg, lipgloss.AdaptiveColor{Dark: "#FF5464", Light: "#8B1515"}},
		{"ctx hot fill", p.ctxHotBg, lipgloss.AdaptiveColor{Dark: "#4D171C", Light: "#F1D2D2"}},
	}
	tints := map[string]lipgloss.AdaptiveColor{
		"red": {Dark: "#EB8E9A", Light: "#921E37"}, "orange": {Dark: "#EBBE9A", Light: "#7A4320"},
		"yellow": {Dark: "#EBD69A", Light: "#69530E"}, "green": {Dark: "#8BD69A", Light: "#145434"},
		"cyan": {Dark: "#8BD6E2", Light: "#125561"}, "blue": {Dark: "#8BA6FA", Light: "#1D40A5"},
		"purple": {Dark: "#BBA6FA", Light: "#53258D"}, "pink": {Dark: "#EBA6E2", Light: "#87197D"},
	}
	for name, want := range tints {
		cases = append(cases, struct {
			name string
			got  lipgloss.AdaptiveColor
			want lipgloss.AdaptiveColor
		}{"tint " + name, sessionHue(name).tint, want})
	}
	for _, c := range cases {
		for _, half := range panelHalves {
			got, want := half.pick(c.got), half.pick(c.want)
			if d := hexChannelDistance(t, got, want); d > 1 {
				t.Errorf("%s (%s) = %s, spec says %s (%d steps off in one channel)", c.name, half.name, got, want, d)
			}
		}
	}
}

// hexChannelDistance is the largest per-channel difference of two #RRGGBB.
func hexChannelDistance(t *testing.T, a, b string) int {
	t.Helper()
	parseTestHex(t, "a", a)
	parseTestHex(t, "b", b)
	worst := 0
	for i := 1; i < 7; i += 2 {
		x, _ := strconv.ParseUint(a[i:i+2], 16, 8)
		y, _ := strconv.ParseUint(b[i:i+2], 16, 8)
		if d := int(x) - int(y); d > worst {
			worst = d
		} else if -d > worst {
			worst = -d
		}
	}
	return worst
}

// TestPanelPaletteColourMath pins the helpers: mix endpoints and midpoint,
// darken/tint as mixes toward black and white, WCAG's own reference values,
// and inkFor measuring rather than assuming on the light theme.
func TestPanelPaletteColourMath(t *testing.T) {
	eq := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %s, want %s", what, got, want)
		}
	}
	eq("mix t=0", mixHex("#FF4D5E", "#000000", 0), "#FF4D5E")
	eq("mix t=1", mixHex("#FF4D5E", "#10121C", 1), "#10121C")
	eq("mix t=.5", mixHex("#000000", "#FFFFFF", 0.5), "#808080")
	eq("darken", darkenHex("#FFFFFF", 0.5), mixHex("#FFFFFF", "#000000", 0.5))
	eq("tint", tintHex("#000000", 0.5), mixHex("#000000", "#FFFFFF", 0.5))

	approx := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 0.01 {
			t.Errorf("%s = %.3f, want %.3f", what, got, want)
		}
	}
	approx("black on white", contrastHex("#000000", "#FFFFFF"), 21)
	approx("symmetric", contrastHex("#FFFFFF", "#000000"), 21)
	approx("same colour", contrastHex("#7A7F9A", "#7A7F9A"), 1)
	approx("#767676 on white (the AA grey)", contrastHex("#767676", "#FFFFFF"), 4.54)

	// Dark: the near-black ink, always. Light: whichever reads better.
	eq("dark inkFor coral", panelDarkBase.inkFor(panelDarkBase.coral), panelDarkBase.ink)
	eq("light inkFor deep coral", panelLightBase.inkFor(panelLightBase.coral), panelLightBase.snow)
	eq("light inkFor pale keyBg", panelLightBase.inkFor(panelLightBase.keyBg), panelLightBase.ink)

	// liftToContrast returns the first colour that clears, and the start
	// colour itself when it already does.
	if got := liftToContrast("#8A8FAA", "#FFFFFF", 4.5, "#22252E"); got != "#8A8FAA" {
		t.Errorf("lift of an already-clear colour moved it to %s", got)
	}
	lifted := liftToContrast("#50546A", "#FFFFFF", 4.5, "#22252E", "#000000")
	if r := contrastOnScreen(lifted, "#22252E"); r < 4.5 {
		t.Errorf("lifted %s is %.2f:1, below the floor it was lifted to", lifted, r)
	}
}

// TestPanelPaletteBusyAndFailLabelsWereLifted: the busy label's starting mix
// is below the text floor on its tinted fill (§6.3: "#83A1D8 measures 4.45 and
// must be lightened"); the palette must have lifted it rather than shipped
// it. This fails if the lift step is removed, independently of the floors.
func TestPanelPaletteBusyAndFailLabelsWereLifted(t *testing.T) {
	for _, b := range []panelBase{panelDarkBase, panelLightBase} {
		th := buildPanelTheme(b)
		for _, c := range []struct {
			name, label, start, bg string
		}{
			{"busy", th.busyLabel, mixHex(b.chipLabel, b.azure, busyLabelMix), th.busyBg},
			{"fail", th.failLabel, mixHex(b.chipLabel, b.rose, failLabelMix), b.keyBg},
		} {
			if r := contrastOnScreen(c.label, c.bg); r < contrastText {
				t.Errorf("light=%v %s label %s is %.2f:1 on %s", b.light, c.name, c.label, r, c.bg)
			}
			if contrastOnScreen(c.start, c.bg) >= contrastText && c.label != c.start {
				t.Errorf("light=%v %s label moved from %s to %s though it already cleared", b.light, c.name, c.start, c.label)
			}
			t.Logf("light=%v %s: start %s (%.2f) → %s (%.2f) on %s", b.light, c.name,
				c.start, contrastOnScreen(c.start, c.bg), c.label, contrastOnScreen(c.label, c.bg), c.bg)
		}
	}
}

// TestPanelPaletteCalmFillsStayNearThePage is the palette half of the calm
// budget (C2): the fills a NON-attention row can carry (dim chips, the busy
// tint, the quiet, warm and hot ctx badges) sit close to the page, while the
// loud fills really are saturated. The frame-level half — loud fills appear
// only on Attention rows' chips and ctx badges — is Phase F's golden test
// over panelFillTones.
func TestPanelPaletteCalmFillsStayNearThePage(t *testing.T) {
	const (
		calmCeiling = 2.0 // a calm fill reads as tinted background
		loudFloor   = 3.0 // a loud fill reads as a solid block
	)
	fills := panelFillTones()
	if len(fills.loud) == 0 || len(fills.quiet) == 0 {
		t.Fatalf("panelFillTones: %d loud, %d quiet; both must be non-empty", len(fills.loud), len(fills.quiet))
	}
	loud := map[lipgloss.AdaptiveColor]bool{}
	for _, c := range fills.loud {
		loud[c] = true
	}
	for _, c := range fills.quiet {
		if loud[c] {
			t.Errorf("%+v is listed as both loud and quiet", c)
		}
	}
	// The loud set is exactly the two attention hues (coral doubles as the
	// loud ctx fill of an Attention row).
	if want := []lipgloss.AdaptiveColor{panelPal.ask, panelPal.coral}; len(fills.loud) != len(want) || !loud[want[0]] || !loud[want[1]] {
		t.Errorf("loud fills = %+v, want exactly ask and coral", fills.loud)
	}

	calm := []struct {
		name string
		c    lipgloss.AdaptiveColor
	}{
		{"keyBg", panelPal.keyBg}, {"valBg", panelPal.valBg}, {"busy fill", panelPal.busyBg},
		{"ctx quiet fill", panelPal.ctxQuietBg}, {"ctx warm fill", panelPal.ctxWarmBg},
		{"ctx hot fill", panelPal.ctxHotBg},
	}
	// Feedback 3: every half of a soft status badge (every tone but the two
	// loud ones) and every name badge is a calm fill too.
	for _, bt := range badgeTones {
		if bt.loud() {
			continue
		}
		c := badgeColoursFor(bt)
		calm = append(calm, struct {
			name string
			c    lipgloss.AdaptiveColor
		}{"badge " + bt.String() + " label", c.labelBg}, struct {
			name string
			c    lipgloss.AdaptiveColor
		}{"badge " + bt.String() + " value", c.valueBg})
	}
	for _, name := range sortedHueNames() {
		calm = append(calm, struct {
			name string
			c    lipgloss.AdaptiveColor
		}{"name badge " + name, panelPal.hues[name].badgeBg})
	}
	for _, half := range panelHalves {
		for _, f := range calm {
			for _, page := range half.pages {
				if r := contrastHex(half.pick(f.c), page); r > calmCeiling {
					t.Errorf("%s %s %s is %.2f:1 against page %s; a calm fill must stay ≤ %.1f", half.name, f.name, half.pick(f.c), r, page, calmCeiling)
				}
			}
		}
		for _, c := range fills.loud {
			for _, page := range half.pages {
				if r := contrastHex(half.pick(c), page); r < loudFloor {
					t.Errorf("%s loud fill %s is only %.2f:1 against page %s", half.name, half.pick(c), r, page)
				}
			}
		}
	}
}

// TestPanelContrastTable logs the measured minimums as a Markdown table, for
// the implementation log (go test -run PanelContrastTable -v).
func TestPanelContrastTable(t *testing.T) {
	var b strings.Builder
	b.WriteString("| pair | floor | dark fg | dark min | light fg | light min |\n|---|---|---|---|---|---|\n")
	for _, p := range panelContrastPairs() {
		row := []string{p.name, fmt.Sprintf("%.1f", p.floor)}
		for _, half := range panelHalves {
			fg := half.pick(p.fg)
			bgs := half.pages
			if !p.onPage {
				bgs = []string{half.pick(p.bg)}
			}
			worst := math.Inf(1)
			for _, bg := range bgs {
				worst = math.Min(worst, contrastOnScreen(fg, bg))
			}
			row = append(row, fg, fmt.Sprintf("%.2f", worst))
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	t.Log("\n" + b.String())
}
