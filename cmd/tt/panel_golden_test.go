package main

// Golden frames of the built-in panel.conf (§6.6, §8 "Golden rows"): a
// SYNTHETIC fleet — newFleet plus transcript facts set in code, never a real
// snapshot — rendered at the sidebar's 46 columns with a fixed clock, in every
// mode, dark and light, and compared byte for byte as ANSI (truecolor) with
// testdata/panel/*.golden. Regenerate with:
//
//	go test -run TestPanelGoldenFrames -update .
//
// The fleet carries every ctx tone (quiet, warm at both edges, hot, unknown,
// and loud on the Attention row dotfiles-02), a model, compactions, subagents,
// API errors, a needs text with a control character in it, and the row R5 is
// about: a blocked background session with Attention and unknown liveness,
// which sits in its HOME group (UNCLEAR, not the band) and still carries the
// loud chip.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

var updateGoldens = flag.Bool("update", false, "rewrite testdata/panel/*.golden from the current rendering")

// The theme is forced with withPanelTheme (panel_chips_test.go): truecolor and
// one background, restored afterwards, never in parallel.

// goldenFacts are the transcript facts the golden fleet adds, by session name.
type goldenFacts struct {
	ctx         int64 // 0 = not measured
	model       string
	sub, apiErr int
	compactions int
}

// The models span the ctx bar's cases (feedback 3): a 1M window by the
// built-in rules (opus-5, sonnet-5, fable-5), a 200k one (any other claude-*),
// a count past its window (claudish-06: 431k of 200k, a full bar), an
// unknown model (gpt-9: the count, no bar) and no model at all.
var goldenFleetFacts = map[string]goldenFacts{
	"magus-00":    {ctx: 248_000, model: "claude-opus-5-5", sub: 2},
	"madbench-01": {ctx: 140_000, model: "claude-sonnet-5"},
	"dotfiles-02": {ctx: 612_000, model: "claude-opus-5-20260901", sub: 3, compactions: 2},
	"mnemex-04":   {ctx: 88_000, model: "claude-haiku-4-5", apiErr: 1},
	"claudish-05": {ctx: 199_999, model: "claude-opus-5-5"}, // quiet: the last count below ctx_warn
	"claudish-06": {ctx: 431_000, model: "claude-sonnet-4-5"},
	"madbench-07": {ctx: 1_200_000, model: "claude-opus-5-5"},
	"madbench-08": {ctx: 200_000, model: "claude-fable-5"}, // warm: ctx_warn itself
	"dotfiles-09": {ctx: 57_000, model: "gpt-9", apiErr: 2},
	"magus-10":    {ctx: 31_000, model: "claude-opus-5-5", sub: 1},
	"authflow-11": {ctx: 400_000, model: "claude-opus-5-5"}, // warm: ctx_alert itself
	"claudish-14": {ctx: 400_001, model: "claude-opus-5-5"}, // hot: one past it, no Attention
	"claudish-13": {ctx: 850},
	"dotfiles-41": {ctx: 136_000, model: "claude-opus-5-5"}, // Attention + Live == nil
}

// goldenLatest is the latest assistant text the golden fleet's transcripts
// end with (revision 4), by session name — synthetic, written into a temp
// directory at test time: one line, two, one long enough that only its last
// three lines are drawn, one with markdown marks and a control character to
// sanitise. A session not named here has no transcript text.
var goldenLatest = map[string]string{
	"claudish-05": "Tests pass.",
	"claudish-06": "The parser now keeps the last three lines of the text; the golden frames were regenerated and reviewed.",
	"madbench-07": "I traced the flaky render to the resize handler: it recomputed the layout before the new width arrived. " +
		"The fix waits for the size message, then lays out once. I added a regression test that resizes twice in one frame, " +
		"and the whole suite is green again after twelve runs.",
	"magus-10":    "Pushed **feat/whole-view** — run `task tt:views` to compare.\x1b[31m",
	"authflow-11": "Waiting for the CI run to finish before merging the release branch.",
	"dotfiles-09": "Renamed the helper and updated its three call sites.",
	"claudish-15": "Nothing left to do here; the branch is merged.",
	"magus-20":    "Summary: the cache is keyed by size and mtime, so an unchanged transcript is read once.",
}

// goldenNoStart are the sessions the golden fleet leaves without a start
// time, so the badge's "—" runtime is pinned too.
var goldenNoStart = map[string]bool{"twin-03": true, "dingo-30": true}

// goldenStartedAt is a session's start: its status time, less a spread that
// covers every runtime format (minutes, 3h12m, 13h, 6d02h and ≥ 10 days, the
// last from the statuses that are days old already).
func goldenStartedAt(i int, since time.Time) time.Time {
	spread := []time.Duration{
		3*time.Hour + 12*time.Minute, 17 * time.Minute, 13 * time.Hour, 6*24*time.Hour + 2*time.Hour,
		48 * time.Minute, 9*time.Hour + 5*time.Minute, 2*24*time.Hour + 4*time.Hour, 0,
	}
	return since.Add(-spread[i%len(spread)])
}

// goldenFleet is newFleet with the facts above, a short id on a background
// band row, and a needs text that must be sanitised before it is drawn.
func goldenFleet(t testing.TB) fleet {
	t.Helper()
	f := newFleet(t, fleetNow)
	snap := *f.snap
	snap.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
	dir := t.TempDir()
	used, spoke := 0, 0
	for i := range snap.Sessions {
		s := &snap.Sessions[i]
		if gf, ok := goldenFleetFacts[s.Name]; ok {
			used++
			facts := observer.TranscriptFacts{Model: gf.model, APIErrors: gf.apiErr, Compactions: gf.compactions}
			if s.Transcript != nil {
				facts.AgentColor, facts.ColorFound = s.Transcript.AgentColor, s.Transcript.ColorFound
			}
			if gf.ctx > 0 {
				facts.ContextTokens = ptr(gf.ctx)
			}
			if gf.sub > 0 {
				facts.Subagents, facts.SubagentsKnown = ptr(gf.sub), true
			}
			s.Transcript = &facts
		}
		if s.StatusSince != nil && !goldenNoStart[s.Name] {
			started := goldenStartedAt(i, *s.StatusSince)
			s.StartedAt = &started
		}
		if text, ok := goldenLatest[s.Name]; ok {
			spoke++
			facts := observer.TranscriptFacts{}
			if s.Transcript != nil {
				facts = *s.Transcript
			}
			facts.Path = writeTranscript(t, dir, s.SessionID,
				assistantRecord(t, text), toolUseRecord(t, "go test ./... # TOOL-INPUT"), toolResultRecord(t, "TOOL-OUTPUT"))
			s.Transcript = &facts
		}
		switch s.Name {
		case "dotfiles-02":
			s.ShortID = ptr("be9663df")
		case "twin-03":
			s.Needs = ptr("confirm the migration plan\n\tbefore it touches the prod replica (step 4 of 6)")
		}
	}
	if used != len(goldenFleetFacts) || spoke != len(goldenLatest) {
		t.Fatalf("%d of %d golden facts and %d of %d latest texts name a session in the fleet", used, len(goldenFleetFacts), spoke, len(goldenLatest))
	}
	f.snap = &snap
	return f
}

// goldenCases are the frames pinned: every mode in both themes at the
// sidebar's real 46x59, the tighter 46x40 where groups collapse, and ENDED
// shown (slate names).
type goldenCase struct {
	name   string
	mode   panelMode
	dark   bool
	height int
	ended  bool
}

func goldenCases() []goldenCase {
	var cs []goldenCase
	for _, dark := range []bool{true, false} {
		theme := map[bool]string{true: "dark", false: "light"}[dark]
		for _, m := range panelModes {
			cs = append(cs, goldenCase{name: fmt.Sprintf("%s-%s-46x59", m, theme), mode: m, dark: dark, height: 59})
		}
	}
	return append(cs,
		// Unbounded: detailFull, so every meta line is drawn.
		goldenCase{name: "project-dark-46-full", mode: modeProject, dark: true},
		goldenCase{name: "project-light-46-full", mode: modeProject},
		goldenCase{name: "project-dark-46x40", mode: modeProject, dark: true, height: 40},
		goldenCase{name: "status-dark-46x72", mode: modeStatus, dark: true, height: 72},
	)
}

// goldenFrame renders one case on a fresh panel: the frame memo is keyed on
// the snapshot, not the theme, so a panel is never reused across themes.
func goldenFrame(t *testing.T, f fleet, c goldenCase) string {
	t.Helper()
	withPanelTheme(t, c.dark)
	p := sidebar(t, f.now, f.snap, c.mode, c.height)
	return p.view() + "\n"
}

func TestPanelGoldenFrames(t *testing.T) {
	f := goldenFleet(t)
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			got := goldenFrame(t, f, c)
			path := filepath.Join("testdata", "panel", c.name+".golden")
			if *updateGoldens {
				must(t, os.MkdirAll(filepath.Dir(path), 0o755))
				must(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("%s differs from its golden; the frame is now:\n%s\n(plain)\n%s",
					path, got, strings.Join(plain(got), "\n"))
			}
		})
	}
}

// goldenOriginViolations reports every row and heading of a painted frame that
// the synthetic fleet did not produce: a row naming a session outside names,
// or a heading outside titles.
func goldenOriginViolations(frame string, names, titles map[string]bool) (bad []string, rows int) {
	for _, l := range parseFrame(plain(strings.TrimSuffix(frame, "\n"))) {
		switch l.kind {
		case lineRow:
			rows++
			if !names[l.name] {
				bad = append(bad, fmt.Sprintf("row %q names %q", l.text, l.name))
			}
		case lineHeading:
			if !titles[l.heading] {
				bad = append(bad, fmt.Sprintf("heading %q", l.text))
			}
		}
	}
	return bad, rows
}

// Privacy (#17): the committed goldens are generated from the SYNTHETIC fleet
// and nothing else. Every file under testdata/panel is either the built-in
// panel.conf or one of goldenCases — each of which TestPanelGoldenFrames
// compares byte for byte with a rendering of goldenFleet — and, independently
// of that renderer, every row a golden draws names a session of the fleet and
// every heading is a group the fleet makes. A frame regenerated from a real
// `tt sessions --json` snapshot, or a stray golden nobody renders, fails here.
// (The observer package's denylist walk over the same tree only catches a
// pasted JSON record; it cannot see session names in a rendered frame.)
func TestEveryPanelGoldenIsRenderedFromTheSyntheticFleet(t *testing.T) {
	f := goldenFleet(t)
	names := map[string]bool{}
	for _, s := range f.snap.Sessions {
		names[s.Name] = true
	}
	titles := map[string]bool{}
	for _, mode := range panelModes {
		p := sidebar(t, f.now, f.snap, mode, 0)
		fr := p.frame()
		for _, s := range fr.secs {
			titles[s.title] = true
		}
	}
	known := map[string]bool{"default-panel.conf.golden": true}
	for _, c := range goldenCases() {
		known[c.name+".golden"] = true
	}

	dir := filepath.Join("testdata", "panel")
	entries, err := os.ReadDir(dir)
	must(t, err)
	checked := 0
	for _, e := range entries {
		if !known[e.Name()] {
			t.Errorf("%s/%s is not a golden this test renders, so nothing proves it synthetic", dir, e.Name())
			continue
		}
		if e.Name() == "default-panel.conf.golden" {
			continue // the built-in text, compared with defaultPanelConf
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		must(t, err)
		bad, rows := goldenOriginViolations(string(data), names, titles)
		for _, b := range bad {
			t.Errorf("%s: not from the synthetic fleet: %s", e.Name(), b)
		}
		if rows == 0 {
			t.Errorf("%s draws no row this check could read", e.Name())
		}
		checked++
	}
	if checked != len(goldenCases()) {
		t.Errorf("checked %d frames, want the %d golden cases", checked, len(goldenCases()))
	}

	// Negative controls: a real-looking row and a real-looking group, pasted
	// into a synthetic frame, are both caught.
	frame := goldenFrame(t, f, goldenCase{name: "project-dark-46x59", mode: modeProject, dark: true, height: 59})
	for _, forged := range []string{
		"  IDL      1d    acme-billing-prod           —",
		"──────────────  acme-billing  3  ──────────────",
	} {
		if bad, _ := goldenOriginViolations(frame+forged+"\n", names, titles); len(bad) == 0 {
			t.Errorf("the forged line %q passed the origin check", forged)
		}
	}
}

// Every golden line fits the sidebar, whatever the colours: the ANSI is
// styling only, never width.
func TestPanelGoldenFramesFitTheSidebar(t *testing.T) {
	f := goldenFleet(t)
	for _, c := range goldenCases() {
		frame := goldenFrame(t, f, c)
		lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
		if c.height > 0 && len(lines) > c.height {
			t.Errorf("%s: %d lines on a %d-line sidebar", c.name, len(lines), c.height)
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > panelWidth {
				t.Errorf("%s line %d is %d columns: %q", c.name, i, w, l)
			}
		}
	}
}

// R5: loudness follows s.Attention on EVERY row. The blocked background
// session whose liveness is unknown is UNCLEAR — it sits in its home group,
// drawn from the row template, not in the band — and it carries the loud
// coral BLK badge all the same. Band membership only chose the template.
func TestAnAttentionRowWithUnknownLivenessGetsTheLoudBadgeOutsideTheBand(t *testing.T) {
	f := goldenFleet(t)
	var hazy observer.Session
	for _, s := range f.snap.Sessions {
		if s.Attention && s.Live == nil {
			hazy = s
		}
	}
	if hazy.Name != "dotfiles-41" {
		t.Fatalf("the fixture's Attention + Live=nil row is %q, want dotfiles-41", hazy.Name)
	}
	for _, dark := range []bool{true, false} {
		for _, mode := range panelModes {
			withPanelTheme(t, dark)
			p := sidebar(t, f.now, f.snap, mode, 0)
			view := p.view()
			lines := strings.Split(view, "\n")
			fl := parseFrame(plain(view))

			row := -1
			for i, l := range fl {
				if l.kind == lineRow && l.name == hazy.Name {
					row = i
				}
			}
			if row < 0 {
				t.Fatalf("%s: %s is not drawn:\n%s", mode, hazy.Name, strings.Join(plain(view), "\n"))
			}
			if _, _, _, band := renderedRow(t, p, hazy.Name); band {
				t.Errorf("%s: %s was drawn in the band; its grouping must not change", mode, hazy.Name)
			}
			runtime := fmtRuntime(panelRuntime(hazy, f.now))
			if got := lines[row]; !strings.Contains(got, paintedRuntime(runtime, badgeBlk)) || fl[row].code != "BLK" {
				t.Errorf("%s dark=%v: %s does not carry the loud coral BLK badge:\n%q", mode, dark, hazy.Name, got)
			}
			// Under it: why it waits (it needs you, band or not), and no
			// internal id (pane, job id) anywhere.
			_, meta, why, _ := renderedRow(t, p, hazy.Name)
			if len(why) == 0 || !strings.Contains(why[0], "waiting ") || strings.Contains(meta+strings.Join(why, " "), "%") ||
				strings.Contains(meta+strings.Join(why, " "), "bg ") {
				t.Errorf("%s: %s draws %q and meta %q, want why it waits and no internal id", mode, hazy.Name, why, meta)
			}
		}
	}
}

// cellBackgrounds is the background of every display column of an ANSI line,
// read from its SGR sequences: "r;g;b" for a truecolor fill, "" for none.
func cellBackgrounds(line string) []string {
	var cols []string
	bg := ""
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && line[j] != 'm' {
				j++
			}
			params := strings.Split(line[i+2:min(j, len(line))], ";")
			for k := 0; k < len(params); k++ {
				switch params[k] {
				case "", "0", "49":
					bg = ""
				case "38", "48":
					switch {
					case k+4 < len(params) && params[k+1] == "2":
						if params[k] == "48" {
							bg = strings.Join(params[k+2:k+5], ";")
						}
						k += 4
					case k+2 < len(params) && params[k+1] == "5":
						if params[k] == "48" {
							bg = "5:" + params[k+2]
						}
						k += 2
					}
				}
			}
			i = j + 1
			continue
		}
		r, n := utf8.DecodeRuneInString(line[i:])
		for w := ansi.StringWidth(string(r)); w > 0; w-- {
			cols = append(cols, bg)
		}
		i += n
	}
	return cols
}

// fillSGR is a palette fill in one theme as the SGR fragment cellBackgrounds
// reads: the PAINTED value.
func fillSGR(c lipgloss.AdaptiveColor, dark bool) string {
	hex := c.Light
	if dark {
		hex = c.Dark
	}
	var r, g, b uint8
	fmt.Sscanf(paintedHex(hex), "#%02X%02X%02X", &r, &g, &b)
	return fmt.Sprintf("%d;%d;%d", r, g, b)
}

// loudFills is the calm budget's saturated fills (panelFillTones().loud) in
// one theme, painted.
func loudFills(dark bool) map[string]bool {
	out := map[string]bool{}
	for _, c := range panelFillTones().loud {
		out[fillSGR(c, dark)] = true
	}
	return out
}

// quietFills is every soft fill the panel may paint (panelFillTones().quiet)
// in one theme, painted.
func quietFills(dark bool) map[string]bool {
	out := map[string]bool{}
	for _, c := range panelFillTones().quiet {
		out[fillSGR(c, dark)] = true
	}
	return out
}

// calmCounts are the allowed saturated cells a frame spent: loud status code
// badges, loud runtime badges, loud ctx badges, (opt-in only) loud chip cells,
// and the header's two saturated fills (the active tab, NEED's number).
type calmCounts struct{ codes, values, badges, chips, header int }

// headerFills is the header's saturated fills in one theme, painted.
func headerFills(dark bool) map[string]bool {
	out := map[string]bool{}
	for _, c := range panelFillTones().header {
		out[fillSGR(c, dark)] = true
	}
	return out
}

// calmBudgetViolations checks one painted frame against the calm budget (C2,
// §6.5, FR6 as revised, feedback 4's L2 row). A saturated fill may sit only on
// a row whose session has Attention, and there only on
//   - its status code badge (columns 1-5),
//   - its runtime badge (columns 7-13), and
//   - its ctx badge at the right edge when the count is above ctx_alert.
//
// Every other fill must be one of the palette's soft fills — the soft status
// badges, the neutral runtime fill, a colour name badge, a ctx tint — and a
// fill the palette does not know is a violation too. Only a panel.conf that
// opts back into {status:chip} (chipsOptIn) may also fill an Attention row's
// chip columns, which then open the row where frameRow's geometry does not
// apply, so those rows are named by chipRowName.
func calmBudgetViolations(frame string, dark, chipsOptIn bool, byName map[string]observer.Session, ctx map[string]int64) (bad []string, n calmCounts) {
	loud, quiet, header := loudFills(dark), quietFills(dark), headerFills(dark)
	raw := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	fl := parseFrame(plain(strings.TrimSuffix(frame, "\n")))
	if chipsOptIn {
		for i := range fl {
			if name := chipRowName(fl[i].text); name != "" {
				fl[i].kind, fl[i].name = lineRow, name
			}
		}
	}
	ctxStart := panelWidth - ctxBadgeWidth
	if chipsOptIn {
		ctxStart = panelWidth - ctxWidth // chipPanelConf draws {ctx}: badge, gap, bar
	}
	for i, line := range raw {
		for col, bg := range cellBackgrounds(line) {
			switch {
			case bg == "" || quiet[bg]:
				continue
			case header[bg] && i < headerLines && !loud[bg]:
				// The active tab and NEED's number: the header's, only.
				n.header++
				continue
			case header[bg] && i < headerLines && fl[i].kind != lineRow:
				// NEED's number shares the loud coral; on the header it is
				// the strip's, not a row's.
				n.header++
				continue
			case !loud[bg]:
				bad = append(bad, fmt.Sprintf("line %d col %d: fill %s is not a palette fill: %q", i, col, bg, fl[i].text))
				continue
			}
			s, known := byName[fl[i].name]
			switch {
			case fl[i].kind != lineRow || !known:
				bad = append(bad, fmt.Sprintf("line %d col %d is not a session row: %q", i, col, fl[i].text))
			case !s.Attention:
				bad = append(bad, fmt.Sprintf("line %d col %d (%s, no attention, ctx %d): %q", i, col, s.Name, ctx[s.Name], fl[i].text))
			case chipsOptIn && col >= 1 && col <= chipWidth:
				n.chips++
			case !chipsOptIn && col >= frameLabelStart && col < frameLabelEnd:
				n.codes++
			case !chipsOptIn && col >= frameValueStart && col < frameBadgeEnd:
				n.values++
			case col >= ctxStart && col < ctxStart+ctxBadgeWidth && ctx[s.Name] > ctxAlert:
				n.badges++
			default:
				bad = append(bad, fmt.Sprintf("line %d col %d (%s, attention, ctx %d): %q", i, col, s.Name, ctx[s.Name], fl[i].text))
			}
		}
	}
	return bad, n
}

// chipRowName is the session a {status:chip} row names: the text after the
// gutter and the 11-column chip, up to the spring. "" for any other line.
func chipRowName(text string) string {
	c := displayCells(text)
	if len(c) <= chipWidth+2 || (c[0] != "▌" && c[0] != " ") || c[chipWidth+1] != " " {
		return ""
	}
	rest := strings.Join(c[chipWidth+2:], "")
	if i := strings.Index(rest, "  "); i >= 0 {
		rest = rest[:i]
	}
	name, _, _ := strings.Cut(rest, " · ")
	return name
}

// C2: saturated backgrounds appear only on Attention rows — their status code
// badge, their runtime badge, and their ctx badge above ctx_alert — in every
// golden frame; every other fill is a soft palette tint. The check is shown to
// catch a loud fill on a calm row's runtime or code, a loud fill on an
// Attention row's name, a loud ctx badge on an Attention row below ctx_alert,
// a loud ctx badge on a row without Attention, and a fill the palette does not
// know. A panel.conf that opts back into {status:chip} gets the loud chips on
// its Attention rows, and nowhere else.
func TestPanelGoldenFramesSpendTheCalmBudgetOnlyWhereItIsAllowed(t *testing.T) {
	f := goldenFleet(t)
	byName := map[string]observer.Session{}
	for _, s := range f.snap.Sessions {
		byName[s.Name] = s
	}
	ctx := map[string]int64{}
	for name, gf := range goldenFleetFacts {
		ctx[name] = gf.ctx
	}
	var total calmCounts
	for _, c := range goldenCases() {
		bad, n := calmBudgetViolations(goldenFrame(t, f, c), c.dark, false, byName, ctx)
		for _, b := range bad {
			t.Errorf("%s: a fill outside the calm budget: %s", c.name, b)
		}
		if n.chips != 0 {
			t.Errorf("%s: %d chip cells were counted with no chip in the template", c.name, n.chips)
		}
		total.codes += n.codes
		total.values += n.values
		total.badges += n.badges
		total.header += n.header
	}
	if total.codes == 0 || total.values == 0 || total.badges == 0 || total.header == 0 {
		t.Fatalf("the golden frames paint %d loud code, %d loud runtime, %d loud ctx and %d header cells; the budget is not exercised",
			total.codes, total.values, total.badges, total.header)
	}

	c := goldenCase{name: "status-dark-46x59", mode: modeStatus, dark: true, height: 59}
	withPanelTheme(t, true)
	frame := goldenFrame(t, f, c)
	flags := func(what, from, to string) {
		t.Helper()
		if !strings.Contains(frame, from) {
			t.Fatalf("control %s: its cell is not in the frame", what)
		}
		if bad, _ := calmBudgetViolations(strings.Replace(frame, from, to, 1), true, false, byName, ctx); len(bad) == 0 {
			t.Errorf("control %s passed the calm budget check", what)
		}
	}
	runtimeOf := func(name string) string { return fmtRuntime(panelRuntime(byName[name], f.now)) }

	// A calm row's neutral runtime badge, repainted on the saturated coral.
	calm := byName["claudish-15"]
	if calm.Attention {
		t.Fatal("control claudish-15 must be calm")
	}
	calmRuntime := cellPad + alignRight(runtimeOf(calm.Name), badgeRuntimeCols) + cellPad
	neutral := paintSegment(runtimeBadge(runtimeOf(calm.Name), badgeIdle))
	flags("loud fill on a calm runtime badge", neutral, runtimeNeutralStyle.Background(panelPal.coral).Render(calmRuntime))

	// A calm row's soft status code, repainted solid.
	flags("loud fill on a calm status code", paintSegment(statusCodeBadge(statusCode(calm), badgeIdle)),
		badgeStyleFor(badgeIdle).label.Background(panelPal.coral).Render(cellPad+alignLeft(statusCode(calm), badgeCodeCols)+cellPad))

	// An Attention row's colour name badge (a soft tint) repainted solid: the
	// row may shout only in its status, runtime and hot ctx badges.
	named := byName["mnemex-04"]
	hue := sessionHue(panelAgentColor(named))
	if !named.Attention || !hue.known() {
		t.Fatalf("control mnemex-04 must be an Attention row with a colour: %+v", named)
	}
	flags("loud fill on an Attention row's name", paintSegment(nameBadge(named.Name, hue, false)),
		nameBadgeStyle(hue).Background(panelPal.coral).Render(cellPad+named.Name+cellPad))

	// An Attention row's ctx badge BELOW ctx_alert repainted solid: a big
	// context is the only ctx state that may shout, even on a loud row.
	low := byName["madbench-01"]
	lowUsed := ctx[low.Name]
	if !low.Attention || lowUsed > ctxAlert {
		t.Fatalf("control madbench-01 must be an Attention row at or below ctx_alert: %+v, ctx %d", low.Attention, lowUsed)
	}
	flags("a loud ctx badge on an Attention row below ctx_alert",
		paintSegment(ctxBadge(&lowUsed, ctxRowTone(&lowUsed, defaultCtxThresholds, true))), paintSegment(ctxBadge(&lowUsed, ctxLoud)))

	// The active tab's accent on a row: a header fill is the header's only.
	flags("the active tab's fill on a row", neutral,
		runtimeNeutralStyle.Background(panelPal.view.tabOnBg).Render(calmRuntime))

	// A soft fill the palette does not have.
	flags("an unknown fill", neutral,
		runtimeNeutralStyle.Background(lipgloss.AdaptiveColor{Dark: "#123456", Light: "#123456"}).Render(calmRuntime))

	// A busy row one past ctx_alert: the soft hot badge is in the frame, and
	// the same count repainted as the solid loud badge breaks the budget.
	hot := byName["claudish-14"]
	if hot.Attention || ctx[hot.Name] <= ctxAlert {
		t.Fatalf("the control row must be above ctx_alert without Attention: %+v, ctx %d", hot.Attention, ctx[hot.Name])
	}
	used := ctx[hot.Name]
	flags("a loud ctx badge without Attention", paintSegment(ctxBadge(&used, ctxHot)), paintSegment(ctxBadge(&used, ctxLoud)))

	// Opting back into the chip: the loud chips are on Attention rows only,
	// and the same frame read as the built-in row (no opt-in) is flagged.
	conf := newConfFixture(t)
	conf.write(t, chipPanelConf, f.now)
	chips := 0
	for _, c := range goldenCases() {
		frame := goldenFrame(t, f, c)
		bad, n := calmBudgetViolations(frame, c.dark, true, byName, ctx)
		for _, b := range bad {
			t.Errorf("%s with {status:chip}: a fill outside the calm budget: %s", c.name, b)
		}
		chips += n.chips
		if n.chips > 0 {
			if bad, _ := calmBudgetViolations(frame, c.dark, false, byName, ctx); len(bad) == 0 {
				t.Errorf("%s: loud chips passed the check of a frame that did not opt into them", c.name)
			}
		}
	}
	if chips == 0 {
		t.Fatal("the {status:chip} frames paint no loud chip cell; the opt-in is not exercised")
	}
}

// chipPanelConf is the chip look of the first rebuild, which a user can opt
// back into with {status:chip}.
const chipPanelConf = `row      = "{gutter}{status:chip} {name:colour} · {group}{>}{ctx}"
attn_row = "{gutter}{status:chip} {name:colour}{>}{ctx}"
`

// paintSegment is a segment as the engine paints it: each part in its style.
func paintSegment(seg panelfmt.Segment) string {
	var b strings.Builder
	for _, p := range seg.Parts {
		b.WriteString(p.Style.Render(p.Text))
	}
	return b.String()
}
