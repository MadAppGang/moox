package main

// theme_panel.go is the session panel's palette: the base tokens of
// architecture §6.1, the colours DERIVED from them (chip halves, ctx badge
// tones, session tints), and the colour math that derives them.
//
// It is the only file in the panel allowed to spell a colour. Every other file
// names a token from panelPal, chipColoursFor or ctxColoursFor — never a hex —
// so a retune is a one-file change and the contrast tests always measure the
// colours the renderer actually paints (panelContrastPairs is built from the
// same tables the primitives in panel_chips.go draw with).
//
// TWO BACKGROUNDS, ONE PASS. Each base is a plain struct of hex strings. The
// derivation runs once per base at package init, and the halves are zipped
// into lipgloss.AdaptiveColor pairs, which lipgloss resolves at render time
// from SetHasDarkBackground (resolveTheme, theme.go). There is no colour math
// per frame.
//
// The light half is not the dark half inverted (the madbench approach): neon
// reads as brightness on black and as glare on white, so each hue drops to its
// deep sibling, and the greys invert because they encode distance from the page.
//
// The design is calm by default. Saturated FILLS are reserved for sessions
// with Attention only: the value half of a loud status badge (or chip) — which
// is also what the L2 row's {status:code} and {runtime:badge} paint on an
// Attention row — and the ctx badge of an Attention row above ctx_alert. Every other row's status
// badge and a coloured session's name badge are SOFT tints near the page. Above ctx_alert without Attention the
// badge is a soft coral tint like the green and honey tiers, so a solid fill
// always means "act now". panelFillTones lists the fills so a test can hold
// every other one to "near the page".

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	colorful "github.com/lucasb-eyer/go-colorful"
)

// panelBase is one background's base tokens (§6.1), as #RRGGBB.
type panelBase struct {
	light bool

	ink       string // ink on bright fills
	snow      string // ink on deep fills
	cloud     string // body: names, the why line
	slate     string // dim: meta, counts, age, template literals
	ash       string // dimmest: "—", notes, the footer
	rule      string // the hairline "─" (decorative)
	sky       string // the title
	azure     string // the busy tint
	moss      string // the ctx quiet hue
	honey     string // the ctx warm hue
	ask       string // ASK: the loud waiting badge, {waiting}, soft ASK (violet, chosen 2026-10-21; its own token so it can differ from honey)
	coral     string // blocked (loud), alerts, the ctx hot hue and loud fill
	rose      string // api errors (dim), the fail label tint
	lavender  string // the idle status badge (a soft slate-lavender)
	teal      string // the shell status badge
	keyBg     string // dim chip, label half
	valBg     string // dim chip, value half
	chipLabel string // dim chip status word
	valFg     string // dim chip age

	// view is the whole-view chrome (FR9): tabs, the count strip, section
	// titles, the latest-text lines, the key strip, and the RUN and IDL codes.
	view viewHex

	// hues maps a /color name to the session's full hue: the gutter "▌", and
	// the heading accent in colour mode. The names are the eight a transcript's
	// agent-color record can carry (§6.2).
	hues map[string]string

	// pages are the terminal backgrounds this half is designed for (§6.1,
	// #25). A colour drawn on the page is lifted until it clears its floor on
	// every one of them, and the contrast tests score against the same list.
	pages []string
}

// viewHex is the whole view's chrome (FR9, revision 4), as #RRGGBB. The values
// are the design sheet's (moox/cmd/tt/scripts/view-options.zsh, `task tt:views`):
// buildPanelTheme lifts a text colour only where it misses its floor, so the
// sheet and the panel agree wherever the sheet already reads.
type viewHex struct {
	// The grouping tabs: the active one filled with the accent (its fill is
	// also the thick baseline under it), the others a dim fill.
	tabOnFg, tabOnBg, tabOffFg, tabOffBg string
	// A section title's label badge and its count badge.
	titleFg, titleBg, countFg, countBg string
	// latest is the dim text under a row (the latest assistant text, or why a
	// session waits), on the page.
	latest string
	// The key strip's key half (a shade stronger) and label half.
	keyFg, keyBg, keyLabelFg, keyLabelBg string
	// The RUN and IDL status codes: green and blue (FR9).
	runFg, runBg, idleFg, idleBg string
	// strip is the count strip's split badges, by state.
	strip [stripStates]splitHex
	// cursorBg is the sidebar cursor's fill under the highlighted row: a
	// faint tint of the accent, so the row reads as selected and every text
	// on it still reads (scored below).
	cursorBg string
}

// splitHex is one count-strip badge: the number half and the label half.
type splitHex struct{ numFg, numBg, labelFg, labelBg string }

// stripState is one count of the header's strip, in drawing order.
type stripState int

const (
	stripNeed    stripState = iota // NEED: the sessions that need you (the number half is solid red)
	stripRun                       // RUN: active
	stripIdle                      // IDL: idle
	stripUnclear                   // ???: unclear
	stripStates                    // how many there are
)

// panelDarkBase is madbench's dark palette, with ash and the rule raised and a
// dedicated chip-label grey, each where a measured WCAG floor failed (§6.1).
var panelDarkBase = panelBase{
	ink:       "#0B0B12",
	snow:      "#FFFFFF",
	cloud:     "#CDD6F4",
	slate:     "#7A7F9A",
	ash:       "#6E7290",
	rule:      "#3C3F51",
	sky:       "#5AC8FF",
	azure:     "#89B4FA",
	moss:      "#8FD9A8",
	honey:     "#FFB454",
	ask:       "#A78BFA",
	coral:     "#FF4D5E",
	rose:      "#D98A92",
	lavender:  "#A3A7D1",
	teal:      "#5CC8BA",
	keyBg:     "#22252E",
	valBg:     "#2E323D",
	chipLabel: "#8A8FAA",
	valFg:     "#9FA6C2",
	hues: map[string]string{
		"red":    "#FF5F5F",
		"orange": "#FFAF5F",
		"yellow": "#FFD75F",
		"green":  "#5FD75F",
		"cyan":   "#5FD7D7",
		"blue":   "#5F87FF",
		"purple": "#AF87FF",
		"pink":   "#FF87D7",
	},
	// The design sheet's dark values (moox/cmd/tt/scripts/view-options.zsh).
	view: viewHex{
		tabOnFg: "#0B0B12", tabOnBg: "#5AC8FF", tabOffFg: "#9FA6C2", tabOffBg: "#2A2C38",
		titleFg: "#CDD6F4", titleBg: "#2E323D", countFg: "#CDD6F4", countBg: "#3C3F51",
		latest: "#5E6280",
		keyFg:  "#9FA6C2", keyBg: "#34374A", keyLabelFg: "#6E7290", keyLabelBg: "#262837",
		runFg: "#8FD9A8", runBg: "#1E3326", idleFg: "#89B4FA", idleBg: "#263241",
		strip: [stripStates]splitHex{
			stripNeed:    {numFg: "#0B0B12", numBg: "#FF4D5E", labelFg: "#FF8A93", labelBg: "#4D171C"},
			stripRun:     {numFg: "#C8F0D4", numBg: "#2E5A3C", labelFg: "#8FD9A8", labelBg: "#1C2E23"},
			stripIdle:    {numFg: "#C4D7FC", numBg: "#34496B", labelFg: "#89B4FA", labelBg: "#1E2838"},
			stripUnclear: {numFg: "#CDD6F4", numBg: "#3C3F51", labelFg: "#9FA6C2", labelBg: "#2A2C38"},
		},
		cursorBg: "#22384E",
	},
	// Black, madbench's page, Catppuccin Mocha, Dracula.
	pages: []string{"#000000", "#0B0B12", "#1E1E2E", "#282A36"},
}

// panelLightBase is the same vocabulary re-voiced for a light page.
var panelLightBase = panelBase{
	light:     true,
	ink:       "#10121C",
	snow:      "#FFFFFF",
	cloud:     "#1E2030",
	slate:     "#4E5364",
	ash:       "#737889",
	rule:      "#C9CED9",
	sky:       "#0369A1",
	azure:     "#1D4ED8",
	moss:      "#047857",
	honey:     "#92400E",
	ask:       "#6D28D9",
	coral:     "#B91C1C",
	rose:      "#BE123C",
	lavender:  "#50558A",
	teal:      "#0F766E",
	keyBg:     "#E2E6F0",
	valBg:     "#D3D8E6",
	chipLabel: "#5E6376",
	valFg:     "#4E5364",
	hues: map[string]string{
		"red":    "#C41E3A",
		"orange": "#A2521A",
		"yellow": "#8A6A00",
		"green":  "#116B36",
		"cyan":   "#0E6C77",
		"blue":   "#1D4ED8",
		"purple": "#6A28B5",
		"pink":   "#B5179E",
	},
	// The design sheet's light values (moox/cmd/tt/scripts/view-options.zsh).
	view: viewHex{
		tabOnFg: "#FFFFFF", tabOnBg: "#0369A1", tabOffFg: "#4E5364", tabOffBg: "#E2E6F0",
		titleFg: "#1E2030", titleBg: "#E2E6F0", countFg: "#1E2030", countBg: "#D3D8E6",
		latest: "#8A8FA3",
		keyFg:  "#4E5364", keyBg: "#D8DCE6", keyLabelFg: "#8A8FA3", keyLabelBg: "#E9ECF3",
		runFg: "#116B36", runBg: "#D6EEDD", idleFg: "#1D4ED8", idleBg: "#DCE6FF",
		strip: [stripStates]splitHex{
			stripNeed:    {numFg: "#FFFFFF", numBg: "#B91C1C", labelFg: "#8B1515", labelBg: "#F6D5D5"},
			stripRun:     {numFg: "#0E5B2E", numBg: "#C2E3CD", labelFg: "#116B36", labelBg: "#E4F2E8"},
			stripIdle:    {numFg: "#173EAD", numBg: "#C9D6F7", labelFg: "#1D4ED8", labelBg: "#E6ECFB"},
			stripUnclear: {numFg: "#1E2030", numBg: "#D3D8E6", labelFg: "#4E5364", labelBg: "#E6E9F0"},
		},
		cursorBg: "#BFDDF2",
	},
	// White, cream, grey, and #F9F6DA: the background measured on this
	// machine's real terminal.
	pages: []string{"#FFFFFF", "#F5EFC8", "#F2F2F2", "#F9F6DA"},
}

// The recipes: how far each derived colour travels from its base. They are the
// palette's knobs, named so a retune reads as a design decision rather than a
// magic number. "mix(a, b, t)" moves a toward b by t, per sRGB channel.
const (
	// Loud chip label half (madbench quietHalf). Dark: snow on the hue sunk
	// toward black. Light: the hue deepened, on the hue raised toward white.
	loudLabelFillDark  = 0.55 // darken(hue, .55)
	loudLabelTextLight = 0.38 // darken(hue, .38)
	loudLabelFillLight = 0.82 // tint(hue, .82)

	// Dim chip tints. The busy fill is keyBg with a trace of azure, so a busy
	// row reads "alive" without competing with the loud chips. The busy and
	// fail labels start as a half-way mix and are then lifted to the text floor
	// against their own fill (liftToContrast): measured, not assumed.
	busyFillMixDark  = 0.14 // mix(keyBg, azure, .14)
	busyFillMixLight = 0.10 // mix(keyBg, azure, .10): azure is deep here, so less of it
	busyLabelMix     = 0.50 // mix(chipLabel, azure, .50), then lifted
	failLabelMix     = 0.50 // mix(chipLabel, rose, .50), then lifted

	// Ctx badge, dark: soft fills near the page, text in the tone's hue.
	ctxQuietFillDark = 0.14 // mix(keyBg, moss, .14)
	ctxQuietTextDark = 0.30 // mix(moss, slate, .30)
	ctxWarmFillDark  = 0.70 // darken(honey, .70); the text is honey itself
	ctxHotFillDark   = 0.70 // darken(coral, .70); the text is alert, lifted on it

	// Ctx badge, light: the hue deepened for text, tinted toward white for fill.
	ctxQuietTextLight = 0.15 // darken(moss, .15)
	ctxQuietFillLight = 0.88 // tint(moss, .88)
	ctxWarmTextLight  = 0.25 // darken(honey, .25)
	ctxWarmFillLight  = 0.80 // tint(honey, .80)
	ctxHotTextLight   = 0.25 // darken(coral, .25), lifted on its fill
	ctxHotFillLight   = 0.80 // tint(coral, .80)

	// Session name tint: the hue moved toward the body colour, so a name keeps
	// its session's colour identity but reads as text, not as a signal.
	nameTintDark  = 0.40 // mix(hue, cloud, .40)
	nameTintLight = 0.30 // mix(hue, cloud, .30)

	// Soft status badge ({status:badge} on a row without Attention): the
	// status hue as text on a sunk tint of the same hue. The value half (the
	// runtime) sits on a slightly stronger tint than the label half, so the
	// split reads without a divider, and both stay near the page (the calm
	// budget). Text starts from the hue (dark) or the hue deepened (light)
	// and is lifted to the text floor on its own fill: measured.
	softLabelFillDark  = 0.80 // darken(hue, .80)
	softValueFillDark  = 0.70 // darken(hue, .70)
	softLabelFillLight = 0.86 // tint(hue, .86)
	softValueFillLight = 0.76 // tint(hue, .76)
	softTextLight      = 0.20 // darken(hue, .20), then lifted

	// Name badge ({name:badge} on a session with a /color): the name tint as
	// text on the hue sunk toward the page (madbench's idBadge left half).
	nameBadgeFillDark  = 0.74 // darken(hue, .74)
	nameBadgeFillLight = 0.84 // tint(hue, .84)
	nameBadgeTextLight = 0.15 // darken(hue, .15), then lifted

	// liftSteps is the resolution of liftToContrast's walk from a colour to its
	// extreme: 2% per step.
	liftSteps = 50
)

// The WCAG 2.1 floors (FR1, §8). Text — every status word, age, name, badge
// count and body line — clears 4.5:1. Dim chrome (meta, the gutter, the "—")
// clears 3:1. The hairline is decoration and only has to be visible.
const (
	contrastText   = 4.5
	contrastChrome = 3.0
	contrastRule   = 1.3
	// contrastVisible is a decoration drawn on a FILL rather than on the page
	// (the │ between two dim tabs): it carries no text and only has to be
	// seen against the fill it divides.
	contrastVisible = 1.15
	// contrastCursor is the cursor's fill against the page: a whole row, so
	// a lower ratio than a hairline's still reads, but the faintest page
	// (#282A36 under the dark fill) is the one it has to clear.
	contrastCursor = 1.15
)

// panelTheme is one background's full colour set: the base tokens plus every
// colour derived from them.
type panelTheme struct {
	panelBase

	honeyLabelFg, honeyLabelBg, honeyValueFg string
	coralLabelFg, coralLabelBg, coralValueFg string
	busyLabel, busyBg, failLabel             string
	ctxQuietFg, ctxQuietBg                   string
	ctxWarmFg, ctxWarmBg                     string
	ctxHotFg, ctxHotBg                       string
	ctxLoudFg                                string
	alert                                    string            // coral as TEXT, lifted to the text floor on the pages
	pageRule                                 string            // the rule, lifted to its floor on the pages
	tints                                    map[string]string // /color → name tint
	// soft is each soft status badge's colours, by the base hue it is built
	// from (softHueNames); nameBadges is each /color's name badge.
	soft       map[string]chipHex
	nameBadges map[string]fillHex
}

// chipHex is a two-half badge as hex: label text and fill, value text and fill.
type chipHex struct{ labelFg, labelBg, valueFg, valueBg string }

// fillHex is text on a fill, as hex.
type fillHex struct{ fg, bg string }

// softHueNames are the base hues a soft status badge is built from.
var softHueNames = []string{"azure", "moss", "lavender", "teal", "ash", "rose", "ask", "coral"}

// softHue is the base hue a soft badge of that name is built from.
func (b panelBase) softHue(name string) string {
	switch name {
	case "azure":
		return b.azure
	case "moss":
		return b.moss
	case "lavender":
		return b.lavender
	case "teal":
		return b.teal
	case "ash":
		return b.ash
	case "rose":
		return b.rose
	case "ask":
		return b.ask
	case "coral":
		return b.coral
	}
	panic("theme_panel: no soft hue " + name)
}

// softBadgeHex builds one soft status badge from a hue (see the soft* knobs).
func (b panelBase) softBadgeHex(hue string) chipHex {
	var c chipHex
	text := hue
	if b.light {
		c.labelBg, c.valueBg = tintHex(hue, softLabelFillLight), tintHex(hue, softValueFillLight)
		text = darkenHex(hue, softTextLight)
	} else {
		c.labelBg, c.valueBg = darkenHex(hue, softLabelFillDark), darkenHex(hue, softValueFillDark)
	}
	c.labelFg = liftToContrast(text, b.textExtreme(), contrastText, c.labelBg)
	c.valueFg = liftToContrast(text, b.textExtreme(), contrastText, c.valueBg)
	return c
}

// nameBadgeHex builds one /color's name badge: its name tint (or, on light,
// the hue deepened) lifted to the text floor on the hue's sunk fill.
func (b panelBase) nameBadgeHex(hue, tint string) fillHex {
	if b.light {
		bg := tintHex(hue, nameBadgeFillLight)
		return fillHex{fg: liftToContrast(darkenHex(hue, nameBadgeTextLight), b.ink, contrastText, bg), bg: bg}
	}
	bg := darkenHex(hue, nameBadgeFillDark)
	return fillHex{fg: liftToContrast(tint, b.snow, contrastText, bg), bg: bg}
}

// buildPanelTheme derives one background's colours from its base. It panics
// on a malformed hex: the bases are constants in this file, so a bad one is a
// bug that the package's own init — and therefore every test — trips over.
func buildPanelTheme(b panelBase) panelTheme {
	t := panelTheme{panelBase: b, tints: make(map[string]string, len(b.hues))}

	if b.light {
		t.honeyLabelFg, t.honeyLabelBg = darkenHex(b.ask, loudLabelTextLight), tintHex(b.ask, loudLabelFillLight)
		t.coralLabelFg, t.coralLabelBg = darkenHex(b.coral, loudLabelTextLight), tintHex(b.coral, loudLabelFillLight)
		t.ctxQuietFg, t.ctxQuietBg = darkenHex(b.moss, ctxQuietTextLight), tintHex(b.moss, ctxQuietFillLight)
		t.ctxWarmFg, t.ctxWarmBg = darkenHex(b.honey, ctxWarmTextLight), tintHex(b.honey, ctxWarmFillLight)
	} else {
		t.honeyLabelFg, t.honeyLabelBg = b.snow, darkenHex(b.ask, loudLabelFillDark)
		t.coralLabelFg, t.coralLabelBg = b.snow, darkenHex(b.coral, loudLabelFillDark)
		t.ctxQuietFg, t.ctxQuietBg = mixHex(b.moss, b.slate, ctxQuietTextDark), mixHex(b.keyBg, b.moss, ctxQuietFillDark)
		t.ctxWarmFg, t.ctxWarmBg = b.honey, darkenHex(b.honey, ctxWarmFillDark)
	}
	t.honeyValueFg = b.inkFor(b.ask)
	t.coralValueFg = b.inkFor(b.coral)
	t.ctxLoudFg = b.inkFor(b.coral)

	busyFill := busyFillMixDark
	if b.light {
		busyFill = busyFillMixLight
	}
	t.busyBg = mixHex(b.keyBg, b.azure, busyFill)
	t.busyLabel = liftToContrast(mixHex(b.chipLabel, b.azure, busyLabelMix), b.textExtreme(), contrastText, t.busyBg)
	t.failLabel = liftToContrast(mixHex(b.chipLabel, b.rose, failLabelMix), b.textExtreme(), contrastText, b.keyBg)

	// Coral is tuned as a FILL (ink on it measures 6.05). As text on the page
	// it lands a hair under 4.5 on the lightest dark page (4.39 on #282A36), so
	// text alerts get their own token, lifted just far enough. The rule is
	// decoration, but it must still be visible on every page.
	t.alert = liftToContrast(b.coral, b.textExtreme(), contrastText, b.pages...)
	t.pageRule = liftToContrast(b.rule, b.textExtreme(), contrastRule, b.pages...)

	// The hot ctx badge is the warm tier's construction in coral: the hue as
	// text on the hue sunk toward the page. The text starts from alert on dark
	// (coral on its own sunk fill measures 4.45) and from the deepened hue on
	// light, and is lifted to the text floor against its fill: measured.
	hotText := t.alert
	if b.light {
		t.ctxHotBg, hotText = tintHex(b.coral, ctxHotFillLight), darkenHex(b.coral, ctxHotTextLight)
	} else {
		t.ctxHotBg = darkenHex(b.coral, ctxHotFillDark)
	}
	t.ctxHotFg = liftToContrast(hotText, b.textExtreme(), contrastText, t.ctxHotBg)

	tint := nameTintDark
	if b.light {
		tint = nameTintLight
	}
	for name, hue := range b.hues {
		t.tints[name] = mixHex(hue, b.cloud, tint)
	}

	t.soft = make(map[string]chipHex, len(softHueNames))
	for _, name := range softHueNames {
		t.soft[name] = b.softBadgeHex(b.softHue(name))
	}
	t.nameBadges = make(map[string]fillHex, len(b.hues))
	for name, hue := range b.hues {
		t.nameBadges[name] = b.nameBadgeHex(hue, t.tints[name])
	}
	t.view = b.liftView()
	return t
}

// liftView is the design sheet's chrome with every text colour held to its
// floor: the tabs, the section titles, the count strip and the RUN/IDL codes
// are text (4.5:1 on their own fill); the latest-text lines and the key strip
// are FR1's dim chrome (3:1, on the page and on their fills). A colour that
// already clears its floor is returned unchanged — liftToContrast's first step
// is the colour itself — so the sheet is the panel wherever the sheet reads.
func (b panelBase) liftView() viewHex {
	v := b.view
	text := func(fg *string, bg string) { *fg = b.liftOn(*fg, bg, contrastText) }
	chrome := func(fg *string, bg string) { *fg = b.liftOn(*fg, bg, contrastChrome) }
	text(&v.tabOnFg, v.tabOnBg)
	text(&v.tabOffFg, v.tabOffBg)
	text(&v.titleFg, v.titleBg)
	text(&v.countFg, v.countBg)
	text(&v.runFg, v.runBg)
	text(&v.idleFg, v.idleBg)
	for i := range v.strip {
		text(&v.strip[i].numFg, v.strip[i].numBg)
		text(&v.strip[i].labelFg, v.strip[i].labelBg)
	}
	chrome(&v.keyFg, v.keyBg)
	chrome(&v.keyLabelFg, v.keyLabelBg)
	v.latest = liftToContrast(v.latest, b.textExtreme(), contrastChrome, b.pages...)
	return v
}

// liftOn lifts fg until it clears floor on bg, toward whichever of snow and
// ink reads better on that fill: ink rides a bright fill (the active tab, the
// solid NEED half), snow a deep one.
func (b panelBase) liftOn(fg, bg string, floor float64) string {
	extreme := b.snow
	if contrastHex(b.ink, bg) > contrastHex(b.snow, bg) {
		extreme = b.ink
	}
	return liftToContrast(fg, extreme, floor, bg)
}

// inkFor is the text colour that rides ON a fill. The dark theme's fills are
// uniformly bright, so its near-black ink is always right. A light theme's
// fills are not uniform — deep hues want white, pale chrome would swallow it —
// so it MEASURES which of snow and ink reads better on this fill instead of
// classifying the fill by lightness (madbench's badgeInk lesson: HSL lightness
// calls a saturated blue "light" and hands it black text at 3.6:1).
func (b panelBase) inkFor(fill string) string {
	if !b.light {
		return b.ink
	}
	if contrastHex(b.snow, fill) >= contrastHex(b.ink, fill) {
		return b.snow
	}
	return b.ink
}

// textExtreme is the colour text moves toward to gain contrast on this
// theme's fills: snow on dark, ink on light.
func (b panelBase) textExtreme() string {
	if b.light {
		return b.ink
	}
	return b.snow
}

// ---------------------------------------------------------------------------
// Colour math (sRGB per channel, so it agrees with madbench and with
// lipgloss v2's Darken; contrast is WCAG 2.1 over linear RGB).
// ---------------------------------------------------------------------------

// mustHex parses a #RRGGBB palette constant.
func mustHex(h string) colorful.Color {
	c, err := colorful.Hex(h)
	if err != nil {
		panic(fmt.Sprintf("theme_panel: bad palette colour %q: %v", h, err))
	}
	return c
}

// hexOf formats a colour back to the #RRGGBB form the palette tables use.
// go-colorful rounds each channel to the nearest step.
func hexOf(c colorful.Color) string { return strings.ToUpper(c.Clamped().Hex()) }

// mixHex moves a toward b by t (0 = a, 1 = b), per sRGB channel.
func mixHex(a, b string, t float64) string {
	return hexOf(mustHex(a).BlendRgb(mustHex(b), t))
}

// darkenHex sinks c toward black by t: mix(c, #000, t).
func darkenHex(c string, t float64) string { return mixHex(c, "#000000", t) }

// tintHex raises c toward white by t: mix(c, #FFF, t). It is proportional, so
// the hue survives — lipgloss's additive Lighten would bleach it to white.
func tintHex(c string, t float64) string { return mixHex(c, "#FFFFFF", t) }

// relLuminanceHex is WCAG 2.1 relative luminance.
func relLuminanceHex(h string) float64 {
	r, g, b := mustHex(h).LinearRgb()
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// contrastHex is the WCAG 2.1 contrast ratio between two colours, 1..21. (Not
// named contrastRatio: statusline.go owns that name for its [3]float64 form.)
func contrastHex(a, b string) float64 {
	la, lb := relLuminanceHex(a), relLuminanceHex(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// paintedHex is the colour a truecolor terminal is actually sent for h.
// termenv (v0.16, RGBColor.Sequence) parses the hex into floats and emits
// uint8(v*255), which truncates the float error: #6E7290 goes out as
// 110;113;144. So a channel can paint one step darker than the palette says.
func paintedHex(h string) string {
	c := mustHex(h)
	return fmt.Sprintf("#%02X%02X%02X", uint8(c.R*255), uint8(c.G*255), uint8(c.B*255))
}

// contrastOnScreen is the worse of the palette's contrast and the painted
// colours' contrast. Every floor in this file and its tests is held on it, so
// termenv's one-step drift can never carry a pair under its floor.
func contrastOnScreen(fg, bg string) float64 {
	return min(contrastHex(fg, bg), contrastHex(paintedHex(fg), paintedHex(bg)))
}

// liftToContrast walks fg toward extreme in liftSteps until it clears floor
// against every bg, and returns the first colour that does — the start colour
// itself when it already clears. It measures the hex it will return, painted,
// so the floor holds on screen.
func liftToContrast(fg, extreme string, floor float64, bgs ...string) string {
	for i := 0; i <= liftSteps; i++ {
		c := mixHex(fg, extreme, float64(i)/liftSteps)
		clears := true
		for _, bg := range bgs {
			if contrastOnScreen(c, bg) < floor {
				clears = false
				break
			}
		}
		if clears {
			return c
		}
	}
	return extreme
}

// ---------------------------------------------------------------------------
// The published palette: every token as a light/dark pair.
// ---------------------------------------------------------------------------

// panelPalette is every colour the panel draws with, as AdaptiveColor pairs.
type panelPalette struct {
	ink, snow, cloud, slate, ash, rule   lipgloss.AdaptiveColor
	sky, azure, moss, honey, coral, rose lipgloss.AdaptiveColor
	lavender, teal                       lipgloss.AdaptiveColor
	keyBg, valBg, chipLabel, valFg       lipgloss.AdaptiveColor

	honeyLabelFg, honeyLabelBg, honeyValueFg lipgloss.AdaptiveColor
	coralLabelFg, coralLabelBg, coralValueFg lipgloss.AdaptiveColor
	busyLabel, busyBg, failLabel             lipgloss.AdaptiveColor
	ctxQuietFg, ctxQuietBg                   lipgloss.AdaptiveColor
	ctxWarmFg, ctxWarmBg                     lipgloss.AdaptiveColor
	ctxHotFg, ctxHotBg                       lipgloss.AdaptiveColor
	ctxLoudFg                                lipgloss.AdaptiveColor
	alert                                    lipgloss.AdaptiveColor // coral as text: NEEDS YOU, alerts, {foo?}, hot and loud {ctx:plain}
	ask                                      lipgloss.AdaptiveColor // ASK's one colour: the loud waiting fill and {waiting}

	// view is the whole view's chrome (FR9).
	view panelView

	// soft is each soft status badge, by softHueNames.
	soft map[string]chipColours

	hues map[string]panelHue
}

// panelHue is a session's colour: the full hue for the gutter (and the heading
// accent in colour mode) and the soft tint for its name. The zero value is "no
// colour", which is what an unknown /color name, or a hex agentColor, gets.
type panelHue struct {
	name   string
	gutter lipgloss.AdaptiveColor
	tint   lipgloss.AdaptiveColor
	// badgeFg on badgeBg is the name badge ({name:badge}).
	badgeFg, badgeBg lipgloss.AdaptiveColor
}

// known reports whether the session has a colour at all.
func (h panelHue) known() bool { return h.name != "" }

// newPanelPalette zips the dark and light themes into pairs.
func newPanelPalette(dark, light panelTheme) panelPalette {
	pair := func(f func(panelTheme) string) lipgloss.AdaptiveColor {
		return lipgloss.AdaptiveColor{Light: f(light), Dark: f(dark)}
	}
	p := panelPalette{
		ink:       pair(func(t panelTheme) string { return t.ink }),
		snow:      pair(func(t panelTheme) string { return t.snow }),
		cloud:     pair(func(t panelTheme) string { return t.cloud }),
		slate:     pair(func(t panelTheme) string { return t.slate }),
		ash:       pair(func(t panelTheme) string { return t.ash }),
		rule:      pair(func(t panelTheme) string { return t.pageRule }),
		sky:       pair(func(t panelTheme) string { return t.sky }),
		azure:     pair(func(t panelTheme) string { return t.azure }),
		moss:      pair(func(t panelTheme) string { return t.moss }),
		honey:     pair(func(t panelTheme) string { return t.honey }),
		coral:     pair(func(t panelTheme) string { return t.coral }),
		rose:      pair(func(t panelTheme) string { return t.rose }),
		lavender:  pair(func(t panelTheme) string { return t.lavender }),
		teal:      pair(func(t panelTheme) string { return t.teal }),
		keyBg:     pair(func(t panelTheme) string { return t.keyBg }),
		valBg:     pair(func(t panelTheme) string { return t.valBg }),
		chipLabel: pair(func(t panelTheme) string { return t.chipLabel }),
		valFg:     pair(func(t panelTheme) string { return t.valFg }),

		honeyLabelFg: pair(func(t panelTheme) string { return t.honeyLabelFg }),
		honeyLabelBg: pair(func(t panelTheme) string { return t.honeyLabelBg }),
		honeyValueFg: pair(func(t panelTheme) string { return t.honeyValueFg }),
		coralLabelFg: pair(func(t panelTheme) string { return t.coralLabelFg }),
		coralLabelBg: pair(func(t panelTheme) string { return t.coralLabelBg }),
		coralValueFg: pair(func(t panelTheme) string { return t.coralValueFg }),
		busyLabel:    pair(func(t panelTheme) string { return t.busyLabel }),
		busyBg:       pair(func(t panelTheme) string { return t.busyBg }),
		failLabel:    pair(func(t panelTheme) string { return t.failLabel }),
		ctxQuietFg:   pair(func(t panelTheme) string { return t.ctxQuietFg }),
		ctxQuietBg:   pair(func(t panelTheme) string { return t.ctxQuietBg }),
		ctxWarmFg:    pair(func(t panelTheme) string { return t.ctxWarmFg }),
		ctxWarmBg:    pair(func(t panelTheme) string { return t.ctxWarmBg }),
		ctxHotFg:     pair(func(t panelTheme) string { return t.ctxHotFg }),
		ctxHotBg:     pair(func(t panelTheme) string { return t.ctxHotBg }),
		ctxLoudFg:    pair(func(t panelTheme) string { return t.ctxLoudFg }),
		alert:        pair(func(t panelTheme) string { return t.alert }),
		ask:          pair(func(t panelTheme) string { return t.ask }),
		view:         newPanelView(dark.view, light.view),

		soft: make(map[string]chipColours, len(softHueNames)),
		hues: make(map[string]panelHue, len(dark.hues)),
	}
	ad := func(l, d string) lipgloss.AdaptiveColor { return lipgloss.AdaptiveColor{Light: l, Dark: d} }
	for _, name := range softHueNames {
		l, d := light.soft[name], dark.soft[name]
		p.soft[name] = chipColours{
			labelFg: ad(l.labelFg, d.labelFg), labelBg: ad(l.labelBg, d.labelBg),
			valueFg: ad(l.valueFg, d.valueFg), valueBg: ad(l.valueBg, d.valueBg),
		}
	}
	for name := range dark.hues {
		p.hues[name] = panelHue{
			name:    name,
			gutter:  ad(light.hues[name], dark.hues[name]),
			tint:    ad(light.tints[name], dark.tints[name]),
			badgeFg: ad(light.nameBadges[name].fg, dark.nameBadges[name].fg),
			badgeBg: ad(light.nameBadges[name].bg, dark.nameBadges[name].bg),
		}
	}
	return p
}

// panelView is viewHex as light/dark pairs.
type panelView struct {
	tabOnFg, tabOnBg, tabOffFg, tabOffBg lipgloss.AdaptiveColor
	titleFg, titleBg, countFg, countBg   lipgloss.AdaptiveColor
	latest                               lipgloss.AdaptiveColor
	keyFg, keyBg, keyLabelFg, keyLabelBg lipgloss.AdaptiveColor
	runFg, runBg, idleFg, idleBg         lipgloss.AdaptiveColor
	cursorBg                             lipgloss.AdaptiveColor
	// strip is each count badge: the number half as label*, the label half
	// as value* (chipColours' two halves).
	strip [stripStates]chipColours
}

// newPanelView zips the two themes' chrome.
func newPanelView(d, l viewHex) panelView {
	ad := func(lt, dk string) lipgloss.AdaptiveColor { return lipgloss.AdaptiveColor{Light: lt, Dark: dk} }
	v := panelView{
		tabOnFg: ad(l.tabOnFg, d.tabOnFg), tabOnBg: ad(l.tabOnBg, d.tabOnBg),
		tabOffFg: ad(l.tabOffFg, d.tabOffFg), tabOffBg: ad(l.tabOffBg, d.tabOffBg),
		titleFg: ad(l.titleFg, d.titleFg), titleBg: ad(l.titleBg, d.titleBg),
		countFg: ad(l.countFg, d.countFg), countBg: ad(l.countBg, d.countBg),
		latest: ad(l.latest, d.latest),
		keyFg:  ad(l.keyFg, d.keyFg), keyBg: ad(l.keyBg, d.keyBg),
		keyLabelFg: ad(l.keyLabelFg, d.keyLabelFg), keyLabelBg: ad(l.keyLabelBg, d.keyLabelBg),
		runFg: ad(l.runFg, d.runFg), runBg: ad(l.runBg, d.runBg),
		idleFg: ad(l.idleFg, d.idleFg), idleBg: ad(l.idleBg, d.idleBg),
		cursorBg: ad(l.cursorBg, d.cursorBg),
	}
	for i := range v.strip {
		ls, ds := l.strip[i], d.strip[i]
		v.strip[i] = chipColours{
			labelFg: ad(ls.numFg, ds.numFg), labelBg: ad(ls.numBg, ds.numBg),
			valueFg: ad(ls.labelFg, ds.labelFg), valueBg: ad(ls.labelBg, ds.labelBg),
		}
	}
	return v
}

// panelPal is the panel's palette, derived once at package init.
var panelPal = newPanelPalette(buildPanelTheme(panelDarkBase), buildPanelTheme(panelLightBase))

// sessionHue maps a /color name to its hue. Matching is case-insensitive and
// trims space, as panelAccents' callers do; anything else — an unknown name, a
// hex value — is the zero panelHue, "no colour", never a guessed hue.
func sessionHue(colour string) panelHue {
	return panelPal.hues[strings.ToLower(strings.TrimSpace(colour))]
}

// ---------------------------------------------------------------------------
// Chip and badge colour tables. panel_chips.go draws from these, and
// panelContrastPairs / panelFillTones enumerate them, so the tests score
// exactly what is painted.
// ---------------------------------------------------------------------------

// chipTone is a status chip's look. The two loud tones are for sessions with
// Attention only (R5); every other row gets one of the dim tones.
type chipTone int

const (
	chipDim   chipTone = iota // idle, shell, unknown, done: the calm default
	chipBusy                  // busy, working: dim, with an azure-tinted label
	chipFail                  // failed: dim, with a rose-tinted label
	chipHoney                 // LOUD: waiting, or any Attention that is not blocked
	chipCoral                 // LOUD: blocked
)

// chipTones lists every tone, for the tables and the tests.
var chipTones = []chipTone{chipDim, chipBusy, chipFail, chipHoney, chipCoral}

// loud reports whether the tone is a saturated, attention chip.
func (t chipTone) loud() bool { return t == chipHoney || t == chipCoral }

// String names the tone in test output.
func (t chipTone) String() string {
	switch t {
	case chipDim:
		return "dim"
	case chipBusy:
		return "busy"
	case chipFail:
		return "fail"
	case chipHoney:
		return "loud honey"
	case chipCoral:
		return "loud coral"
	}
	return fmt.Sprintf("chipTone(%d)", int(t))
}

// chipColours is one tone's four colours: the label half and the value half.
type chipColours struct {
	labelFg, labelBg, valueFg, valueBg lipgloss.AdaptiveColor
}

// chipColoursFor returns a tone's colours. An unknown tone draws dim: the calm
// choice is the safe one for a value this table does not know.
func chipColoursFor(t chipTone) chipColours {
	p := panelPal
	dimValue := chipColours{valueFg: p.valFg, valueBg: p.valBg}
	switch t {
	case chipBusy:
		dimValue.labelFg, dimValue.labelBg = p.busyLabel, p.busyBg
		return dimValue
	case chipFail:
		dimValue.labelFg, dimValue.labelBg = p.failLabel, p.keyBg
		return dimValue
	case chipHoney:
		return chipColours{labelFg: p.honeyLabelFg, labelBg: p.honeyLabelBg, valueFg: p.honeyValueFg, valueBg: p.ask}
	case chipCoral:
		return chipColours{labelFg: p.coralLabelFg, labelBg: p.coralLabelBg, valueFg: p.coralValueFg, valueBg: p.coral}
	}
	dimValue.labelFg, dimValue.labelBg = p.chipLabel, p.keyBg
	return dimValue
}

// ageColourFor is the plain-text age's colour for a chip tone: on an
// Attention row the loud chip's hue as text (honey; blocked in the alert
// coral, which clears the text floor where raw coral does not), else slate.
func ageColourFor(t chipTone) lipgloss.AdaptiveColor {
	switch t {
	case chipHoney:
		return panelPal.ask
	case chipCoral:
		return panelPal.alert
	}
	return panelPal.slate
}

// badgeTone is a {status:badge}'s look (feedback 3): the madbench-style
// split badge of a three-letter status code and the session's runtime. Only
// the two loud tones — for sessions with Attention — carry a saturated fill
// (their value half); every other tone is SOFT but coloured: the status hue on
// a sunk tint of the same hue, so a row says "running" or "idle" by colour
// without competing with the rows that need you.
type badgeTone int

const (
	badgeRun     badgeTone = iota // busy, working: soft azure
	badgeIdle                     // idle: soft slate-lavender
	badgeShell                    // shell: soft teal
	badgeUnclear                  // unknown, or a status this build has never seen: soft ash
	badgeEnded                    // done: the dim chip's greys
	badgeFail                     // failed: soft rose
	badgeAskSoft                  // waiting under {status:badge:quiet}: soft honey
	badgeBlkSoft                  // blocked under {status:badge:quiet}: soft coral
	badgeAsk                      // LOUD: Attention that is not blocked (honey)
	badgeBlk                      // LOUD: blocked with Attention (coral)
)

// badgeTones lists every tone, for the tables and the tests.
var badgeTones = []badgeTone{badgeRun, badgeIdle, badgeShell, badgeUnclear, badgeEnded, badgeFail, badgeAskSoft, badgeBlkSoft, badgeAsk, badgeBlk}

// loud reports whether the tone is an attention badge with a solid value half.
func (t badgeTone) loud() bool { return t == badgeAsk || t == badgeBlk }

// String names the tone in test output.
func (t badgeTone) String() string {
	switch t {
	case badgeRun:
		return "run"
	case badgeIdle:
		return "idle"
	case badgeShell:
		return "shell"
	case badgeUnclear:
		return "unclear"
	case badgeEnded:
		return "ended"
	case badgeFail:
		return "fail"
	case badgeAskSoft:
		return "soft ask"
	case badgeBlkSoft:
		return "soft blocked"
	case badgeAsk:
		return "loud ask"
	case badgeBlk:
		return "loud blocked"
	}
	return fmt.Sprintf("badgeTone(%d)", int(t))
}

// badgeColoursFor returns a tone's colours. The loud tones are the loud
// chip's (label a quiet tint, value the solid hue with its contrast ink); the
// ended tone is the dim chip's greys; the rest are soft badges of their hue.
// An unknown tone draws as unclear.
func badgeColoursFor(t badgeTone) chipColours {
	p := panelPal
	switch t {
	case badgeAsk:
		return chipColoursFor(chipHoney)
	case badgeBlk:
		return chipColoursFor(chipCoral)
	case badgeEnded:
		return chipColoursFor(chipDim)
	case badgeRun:
		// FR9: RUN is green. The code half is the design sheet's; the split
		// badge's value half ({status:badge}) is the soft green a notch stronger.
		return chipColours{labelFg: p.view.runFg, labelBg: p.view.runBg, valueFg: p.soft["moss"].valueFg, valueBg: p.soft["moss"].valueBg}
	case badgeIdle:
		// FR9: IDL is blue, built the same way from azure.
		return chipColours{labelFg: p.view.idleFg, labelBg: p.view.idleBg, valueFg: p.soft["azure"].valueFg, valueBg: p.soft["azure"].valueBg}
	case badgeShell:
		return p.soft["teal"]
	case badgeFail:
		return p.soft["rose"]
	case badgeAskSoft:
		return p.soft["ask"]
	case badgeBlkSoft:
		return p.soft["coral"]
	}
	return p.soft["ash"]
}

// fillColours is text on one fill, as light/dark pairs.
type fillColours struct{ fg, bg lipgloss.AdaptiveColor }

// runtimeNeutralColours is {runtime:badge} on a row without Attention
// (feedback 4, layout L2): the body colour on the dim chip's value fill — one
// neutral fill for every calm status, so the runtime reads as a value beside
// the status badge's hue, never as a second status. A loud row's runtime badge
// is the loud badge's value half instead (badgeColoursFor).
func runtimeNeutralColours() fillColours {
	return fillColours{fg: panelPal.cloud, bg: panelPal.valBg}
}

// ctxTone is the context badge's tone, set by absolute thresholds (§4.3) and,
// for the one solid tone, by the row's Attention (FR6 as revised).
type ctxTone int

const (
	ctxUnknown ctxTone = iota // no measured usage: "—" in ash, no fill
	ctxQuiet                  // below ctx_warn: soft green
	ctxWarm                   // ctx_warn..ctx_alert: soft honey
	ctxHot                    // above ctx_alert: soft coral
	ctxLoud                   // above ctx_alert on an Attention row: solid coral — the one ctx state that shouts
)

// ctxTones lists every tone, for the tables and the tests.
var ctxTones = []ctxTone{ctxUnknown, ctxQuiet, ctxWarm, ctxHot, ctxLoud}

// String names the tone in test output.
func (t ctxTone) String() string {
	switch t {
	case ctxUnknown:
		return "unknown"
	case ctxQuiet:
		return "quiet"
	case ctxWarm:
		return "warm"
	case ctxHot:
		return "hot"
	case ctxLoud:
		return "loud"
	}
	return fmt.Sprintf("ctxTone(%d)", int(t))
}

// ctxColours is one tone's badge colours. plainFg is the tone's colour with no
// fill, for {ctx:plain}; hasFill is false for the unknown tone, which draws
// its "—" on the page.
type ctxColours struct {
	fg, bg  lipgloss.AdaptiveColor
	plainFg lipgloss.AdaptiveColor
	hasFill bool
}

// ctxColoursFor returns a tone's colours.
func ctxColoursFor(t ctxTone) ctxColours {
	p := panelPal
	switch t {
	case ctxQuiet:
		return ctxColours{fg: p.ctxQuietFg, bg: p.ctxQuietBg, plainFg: p.ctxQuietFg, hasFill: true}
	case ctxWarm:
		return ctxColours{fg: p.ctxWarmFg, bg: p.ctxWarmBg, plainFg: p.ctxWarmFg, hasFill: true}
	case ctxHot:
		return ctxColours{fg: p.ctxHotFg, bg: p.ctxHotBg, plainFg: p.alert, hasFill: true}
	case ctxLoud:
		return ctxColours{fg: p.ctxLoudFg, bg: p.coral, plainFg: p.alert, hasFill: true}
	}
	return ctxColours{fg: p.ash, plainFg: p.ash}
}

// ---------------------------------------------------------------------------
// What the tests measure.
// ---------------------------------------------------------------------------

// panelContrastPair is one foreground the panel paints and what it is painted
// on. onPage means the terminal's own background: the test scores it against
// every test page of each theme. Otherwise bg is the fill it sits on.
type panelContrastPair struct {
	name   string
	fg, bg lipgloss.AdaptiveColor
	onPage bool
	floor  float64
}

// panelContrastPairs lists every foreground/background pair the panel's
// primitives draw, built from the same tables they draw with.
func panelContrastPairs() []panelContrastPair {
	p := panelPal
	var out []panelContrastPair
	fill := func(name string, fg, bg lipgloss.AdaptiveColor, floor float64) {
		out = append(out, panelContrastPair{name: name, fg: fg, bg: bg, floor: floor})
	}
	page := func(name string, fg lipgloss.AdaptiveColor, floor float64) {
		out = append(out, panelContrastPair{name: name, fg: fg, onPage: true, floor: floor})
	}

	// Every chip half is text: the status word and the age (#13).
	for _, t := range chipTones {
		c := chipColoursFor(t)
		fill("chip "+t.String()+" label", c.labelFg, c.labelBg, contrastText)
		fill("chip "+t.String()+" value", c.valueFg, c.valueBg, contrastText)
	}
	// The plain age is text on the page. The loud ones say "needs you" and
	// clear the text floor; the calm one is slate, dim chrome like the meta.
	for _, t := range chipTones {
		floor := contrastChrome
		if t.loud() {
			floor = contrastText
		}
		page("age "+t.String(), ageColourFor(t), floor)
	}
	// Both halves of every status badge are text on their fill (feedback 3).
	for _, t := range badgeTones {
		c := badgeColoursFor(t)
		fill("badge "+t.String()+" label", c.labelFg, c.labelBg, contrastText)
		fill("badge "+t.String()+" value", c.valueFg, c.valueBg, contrastText)
	}
	// The calm runtime badge is text on its neutral fill (feedback 4). The
	// loud one, and {status:code}, reuse badge halves scored just above.
	rn := runtimeNeutralColours()
	fill("runtime badge neutral", rn.fg, rn.bg, contrastText)
	// The ctx bar's empty cells are dim chrome on the page; its filled cells
	// are the tone's plain colour, scored below as "ctx <tone> plain".
	page("ctx bar empty", ctxBarEmptyColour(), contrastChrome)
	// The badge count is text, on its fill and — for {ctx:plain} — on the page.
	for _, t := range ctxTones {
		c := ctxColoursFor(t)
		if c.hasFill {
			fill("ctx "+t.String()+" badge", c.fg, c.bg, contrastText)
			page("ctx "+t.String()+" plain", c.plainFg, contrastText)
		} else {
			page("ctx "+t.String()+" dash", c.fg, contrastChrome)
		}
	}

	page("cloud (body)", p.cloud, contrastText)
	page("sky (title)", p.sky, contrastText)
	page("alert (coral text)", p.alert, contrastText)
	page("rose (api err)", p.rose, contrastText)
	page("slate (meta)", p.slate, contrastChrome)
	page("ash (notes)", p.ash, contrastChrome)
	page("rule (hairline)", p.rule, contrastRule)

	// The whole view's chrome (FR9). The tabs, titles and count strip are
	// text; the latest-text lines and the key strip are FR1's dim chrome. The
	// fades, the baseline, the title rules and the K4 fill are the rule
	// colour on the page (scored just above); the thick baseline stretch is the
	// active tab's fill on the page, and the tab divider is the rule on the
	// dim tab fill — decorations, held only to being visible.
	v := p.view
	fill("tab active", v.tabOnFg, v.tabOnBg, contrastText)
	fill("tab", v.tabOffFg, v.tabOffBg, contrastText)
	fill("tab divider", p.rule, v.tabOffBg, contrastVisible)
	page("baseline under the active tab", v.tabOnBg, contrastRule)
	fill("tab active, sidebar at rest", v.tabOnBg, v.tabOffBg, contrastText)
	// The cursor's fill runs under the highlighted row's plain text: the
	// name and the why are text, the meta dim chrome. The ash and latest
	// greys are drawn in slate on the cursor (cursorLine), so slate is the
	// dimmest text the fill has to carry. The fill itself has to stand out
	// from the page, or the cursor cannot be found.
	fill("cursor: name", p.cloud, v.cursorBg, contrastText)
	fill("cursor: meta, dash and latest", p.slate, v.cursorBg, contrastChrome)
	page("cursor fill", v.cursorBg, contrastCursor)
	fill("section title label", v.titleFg, v.titleBg, contrastText)
	fill("section title count", v.countFg, v.countBg, contrastText)
	for st := stripState(0); st < stripStates; st++ {
		c := v.strip[st]
		fill("strip "+st.String()+" number", c.labelFg, c.labelBg, contrastText)
		fill("strip "+st.String()+" label", c.valueFg, c.valueBg, contrastText)
	}
	page("latest text", v.latest, contrastChrome)
	fill("key strip key", v.keyFg, v.keyBg, contrastChrome)
	fill("key strip label", v.keyLabelFg, v.keyLabelBg, contrastChrome)

	for _, name := range sortedHueNames() {
		h := p.hues[name]
		page("gutter "+name, h.gutter, contrastChrome)
		page("name tint "+name, h.tint, contrastText)
		fill("name badge "+name, h.badgeFg, h.badgeBg, contrastText)
	}
	return out
}

// ctxBarEmptyColour is the colour of the context bar's empty cells (▱).
func ctxBarEmptyColour() lipgloss.AdaptiveColor { return panelPal.ash }

// panelFills are the background fills the panel paints, split by loudness.
// Loud fills are saturated and allowed only on an Attention row: its status
// badge's (or chip's) value half, and its ctx badge above ctx_alert (the calm
// budget, §6.5, FR6 as revised); every other fill — the soft status badges,
// the name badges and the hot badge's coral tint included — is quiet.
//
// header are the two saturated fills the whole view's header spends (FR9):
// the active tab and the NEED count's number half. They are allowed on the
// header's lines only, never on a row.
type panelFills struct {
	loud, quiet, header []lipgloss.AdaptiveColor
}

// panelFillTones enumerates the fills from the chip and badge tables.
func panelFillTones() panelFills {
	var f panelFills
	seen := map[lipgloss.AdaptiveColor]bool{}
	add := func(c lipgloss.AdaptiveColor, loud bool) {
		if seen[c] {
			return
		}
		seen[c] = true
		if loud {
			f.loud = append(f.loud, c)
		} else {
			f.quiet = append(f.quiet, c)
		}
	}
	for _, t := range chipTones {
		c := chipColoursFor(t)
		add(c.labelBg, false) // even a loud chip's label half is a quiet tint
		add(c.valueBg, t.loud())
	}
	for _, t := range ctxTones {
		if c := ctxColoursFor(t); c.hasFill {
			add(c.bg, t == ctxLoud)
		}
	}
	for _, t := range badgeTones {
		c := badgeColoursFor(t)
		add(c.labelBg, false)
		add(c.valueBg, t.loud())
	}
	add(runtimeNeutralColours().bg, false)
	for _, name := range sortedHueNames() {
		add(panelPal.hues[name].badgeBg, false)
	}
	v := panelPal.view
	f.header = []lipgloss.AdaptiveColor{v.tabOnBg, v.strip[stripNeed].labelBg}
	for _, c := range []lipgloss.AdaptiveColor{v.tabOffBg, v.titleBg, v.countBg, v.keyBg, v.keyLabelBg, v.runBg, v.idleBg, v.cursorBg} {
		add(c, false)
	}
	for st := stripState(0); st < stripStates; st++ {
		if st != stripNeed {
			add(v.strip[st].labelBg, false)
		}
		add(v.strip[st].valueBg, false)
	}
	return f
}

// String names a strip state in test output.
func (s stripState) String() string {
	switch s {
	case stripNeed:
		return "need"
	case stripRun:
		return "run"
	case stripIdle:
		return "idle"
	case stripUnclear:
		return "unclear"
	}
	return fmt.Sprintf("stripState(%d)", int(s))
}

// sortedHueNames is the hue table's keys in a fixed order, for stable output.
func sortedHueNames() []string {
	return []string{"red", "orange", "yellow", "green", "cyan", "blue", "purple", "pink"}
}
