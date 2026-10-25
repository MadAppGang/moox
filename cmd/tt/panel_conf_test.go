package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

// confFixture points panelConfPath at a file in a temp dir for one test and
// counts the reads, so "an unchanged stamp is not re-read" can be asserted.
type confFixture struct {
	path  string
	reads *int
}

func newConfFixture(t *testing.T) confFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tt", panelConfName)
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	reads := new(int)
	stubSeam(t, &panelConfPath, func() string { return path })
	original := readPanelConf
	stubSeam(t, &readPanelConf, func(p string) ([]byte, error) { *reads++; return original(p) })
	return confFixture{path: path, reads: reads}
}

// write replaces the file and moves its mtime on, so a same-second rewrite of
// the same size is still a new stamp.
func (c confFixture) write(t *testing.T, body string, at time.Time) {
	t.Helper()
	must(t, os.WriteFile(c.path, []byte(body), 0o644))
	must(t, os.Chtimes(c.path, at, at))
}

// confSnapshot is one idle session, enough to see a row's shape.
func confSnapshot(now time.Time) *observer.Snapshot {
	since := now.Add(-3 * time.Minute)
	used := int64(150_000)
	return &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{
			SessionID: "s1", Name: "alpha", Status: observer.StatusIdle, Live: ptr(true), StatusSince: &since,
			Transcript: &observer.TranscriptFacts{ContextTokens: &used},
		}},
	}
}

func TestTheBuiltInPanelConfIsTheDocumentedOne(t *testing.T) {
	path := filepath.Join("testdata", "panel", "default-panel.conf.golden")
	if *updateGoldens {
		must(t, os.WriteFile(path, []byte(defaultPanelConf), 0o644))
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if defaultPanelConf != string(want) {
		t.Errorf("defaultPanelConf differs from %s:\n%s", path, defaultPanelConf)
	}
	if panelDefaultSet.Ctx != defaultCtxThresholds {
		t.Errorf("built-in thresholds %+v, want %+v", panelDefaultSet.Ctx, defaultCtxThresholds)
	}
}

func TestThePanelConfPathFollowsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	if got := defaultPanelConfPath(); got != "/xdg/conf/tt/panel.conf" {
		t.Errorf("XDG path = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	home, _ := os.UserHomeDir()
	if got := defaultPanelConfPath(); got != filepath.Join(home, ".config", "tt", "panel.conf") {
		t.Errorf("a relative XDG_CONFIG_HOME was honoured: %q", got)
	}
}

// R6: a panel.conf that exists before the panel is built shapes its FIRST
// frame — not the second, after an update happened to reload it.
func TestTheFirstFrameUsesAPanelConfThatAlreadyExists(t *testing.T) {
	c := newConfFixture(t)
	c.write(t, "row = \"{name}{>}{status:word}\"\nmeta = \"\"\n", fleetNow)

	p := newPanel(nil, newPanelClock(fleetNow))
	p.width, p.now, p.mode = panelWidth, fleetNow, modeStatus
	p.snap = confSnapshot(fleetNow) // no update ran: only construction loaded the file
	lines := plain(p.frameText())
	if !contains(lines, "alpha"+strings.Repeat(" ", panelWidth-len("alpha")-len("idle"))+"idle") {
		t.Errorf("the first frame does not use the file's row template:\n%s", strings.Join(lines, "\n"))
	}
	if *c.reads != 1 {
		t.Errorf("construction read the file %d times, want 1", *c.reads)
	}
}

// frameText renders an unopened panel's frame (view() draws nothing while closed).
func (p panel) frameText() string { return strings.Join(p.lines(), "\n") }

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// Editing the file changes the next frame; an untouched file is stat'ed and
// never re-read; a broken file keeps the built-in format and says why on a
// line of its own; fixing it clears the note.
func TestAnEditedPanelConfIsFollowedAndABrokenOneIsSaid(t *testing.T) {
	c := newConfFixture(t)
	p, _, _ := openTestPanel(t, fleetNow)
	p.mode = modeStatus
	deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
	builtin := plain(p.view())
	if r, ok := frameRow(parseFrame(builtin), "alpha"); !ok || r.ctx != "150k" {
		t.Fatalf("with no file the built-in row is drawn: %+v\n%s", r, strings.Join(builtin, "\n"))
	}
	if *c.reads != 0 {
		t.Fatalf("an absent file was read %d times", *c.reads)
	}

	// 1. A good file: applied on the next update, and the generation moved.
	gen := p.confGen()
	c.write(t, "row = \"{name} [{status:word}]\"\nctx_warn = 100k\n", fleetNow.Add(time.Second))
	p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	if p.confGen() == gen {
		t.Fatal("a changed file did not bump the config generation")
	}
	if got := plain(p.view()); !contains(got, "alpha [idle]") {
		t.Fatalf("the edited row template was not applied:\n%s", strings.Join(got, "\n"))
	}
	if p.confSet().Ctx.Warn != 100_000 {
		t.Errorf("ctx_warn from the file = %d", p.confSet().Ctx.Warn)
	}

	// 2. Untouched: stat'ed on every update, read never again.
	reads := *c.reads
	for i := 0; i < 5; i++ {
		p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	}
	if *c.reads != reads {
		t.Errorf("an unchanged file was re-read %d times", *c.reads-reads)
	}

	// 3. Broken: the built-in format, and the first problem on its own line
	// above the footer, counted in the reserve.
	c.write(t, "row = \"{name\"\n", fleetNow.Add(2*time.Second))
	p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	broken := plain(p.view())
	if _, ok := frameRow(parseFrame(broken), "alpha"); !ok {
		t.Errorf("a broken file did not fall back to the built-in row:\n%s", strings.Join(broken, "\n"))
	}
	note := p.configNote()
	if !strings.HasPrefix(note, "panel.conf:1:") || !strings.HasSuffix(note, panelConfFallback) {
		t.Errorf("the note is %q", note)
	}
	if !contains(broken, panelTruncate(note, panelWidth)) {
		t.Errorf("the note is not drawn:\n%s", strings.Join(broken, "\n"))
	}
	if p.reserveLines() != 1 {
		t.Errorf("reserveLines = %d with a config note, want 1 (the note)", p.reserveLines())
	}

	// 4. Fixed: the note is gone.
	c.write(t, "meta = \"\"\n", fleetNow.Add(3*time.Second))
	p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	if p.configNote() != "" || strings.Contains(p.view(), "panel.conf") {
		t.Errorf("fixing the file did not clear the note: %q", p.configNote())
	}

	// 5. Removed: back to the built-in format.
	must(t, os.Remove(c.path))
	p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	if p.confSet() != panelDefaultSet {
		t.Error("a removed file did not restore the built-in format")
	}
}

// A soft problem keeps the file: the unknown token is drawn in place as
// {raw?} and named in the note.
func TestAnUnknownTokenIsDrawnInPlaceAndNamedInTheNote(t *testing.T) {
	c := newConfFixture(t)
	c.write(t, "row = \"{name} {foo} {ctx:pct}\"\n", fleetNow)
	p, _, _ := openTestPanel(t, fleetNow)
	p.mode = modeStatus
	deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
	lines := plain(p.view())
	if !contains(lines, "alpha {foo?} {ctx:pct?}") {
		t.Errorf("unknown tokens are not drawn in place:\n%s", strings.Join(lines, "\n"))
	}
	if note := p.configNote(); !strings.HasPrefix(note, "panel.conf:1:") || strings.HasSuffix(note, panelConfFallback) {
		t.Errorf("a soft problem's note is %q", note)
	}
}

// Opening the panel follows the file too: an edit made while it was closed
// shows on the first frame after reopening.
func TestReopeningThePanelFollowsAnEditMadeWhileItWasClosed(t *testing.T) {
	c := newConfFixture(t)
	p, _, _ := openTestPanel(t, fleetNow)
	p.mode = modeStatus
	p.toggle() // closed
	c.write(t, "row = \"<{name}>\"\n", fleetNow)
	p.toggle() // open: start() follows the file
	deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
	if got := plain(p.view()); !contains(got, "<alpha>") {
		t.Errorf("reopening did not follow the file:\n%s", strings.Join(got, "\n"))
	}
}

// A file too large or unreadable is the built-in format and a note, and the
// panel is never taller than its height for having one.
func TestAnOversizedOrUnreadablePanelConfFallsBackAndStillFits(t *testing.T) {
	c := newConfFixture(t)
	c.write(t, "# "+strings.Repeat("x", 70<<10)+"\n", fleetNow)
	p, _, _ := openTestPanel(t, fleetNow)
	if note := p.configNote(); !strings.HasPrefix(note, "panel.conf: ") || !strings.HasSuffix(note, panelConfFallback) {
		t.Errorf("an oversized file's note is %q", note)
	}
	deliver(t, p, snapshotEvent(fitPopulation(fleetNow)))
	for h := 1; h <= 30; h++ {
		p.height = h
		if n := strings.Count(p.view(), "\n") + 1; n > h {
			t.Errorf("height %d with a config note: %d lines", h, n)
		}
	}

	must(t, os.Remove(c.path))
	must(t, os.Mkdir(c.path, 0o755)) // a directory where the file should be
	p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	if note := p.configNote(); !strings.HasPrefix(note, "panel.conf: ") || !strings.HasSuffix(note, panelConfFallback) {
		t.Errorf("an unreadable file's note is %q", note)
	}
	if p.confSet() != panelDefaultSet {
		t.Error("an unreadable file did not leave the built-in format")
	}
}

// The frame memo is keyed on the config generation: the same snapshot,
// clock and size draw again when only the format changed.
func TestAChangedFormatRedrawsAnOtherwiseUnchangedFrame(t *testing.T) {
	c := newConfFixture(t)
	p, _, _ := openTestPanel(t, fleetNow)
	p.mode = modeStatus
	deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
	before := p.view()
	c.write(t, "row = \"{name}!\"\n", fleetNow)
	p.followConfig() // nothing else moves: not the clock, not the snapshot
	if after := p.view(); after == before || !strings.Contains(after, "alpha!") {
		t.Errorf("the memoised frame survived a format change:\n%s", after)
	}
}

// A row template with nothing to say for a session does not make the session
// vanish: the built-in row draws it.
func TestARowTemplateThatSaysNothingFallsBackToTheBuiltInRow(t *testing.T) {
	c := newConfFixture(t)
	c.write(t, "row = \"{kind}\"\n", fleetNow)
	p, _, _ := openTestPanel(t, fleetNow)
	p.mode = modeStatus
	deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
	if _, ok := frameRow(parseFrame(plain(p.view())), "alpha"); !ok {
		t.Errorf("an interactive session under row = {kind} vanished:\n%s", p.view())
	}
}

// headLine's fallback rests on the built-in head templates always drawing:
// their gutter and chip are never Empty, whatever the session — even one with
// no field set at all — and at any width. Should a built-in template ever
// report no line, headLine draws the row's bare name rather than nothing.
func TestTheBuiltInHeadTemplatesAlwaysDraw(t *testing.T) {
	rc := &rowContext{now: fleetNow, ctx: panelDefaultSet.Ctx, project: func(string) string { return "" }}
	for _, s := range []observer.Session{
		{},
		{SessionID: "x"},
		{SessionID: "y", Status: observer.StatusBlocked, Attention: true},
	} {
		for _, band := range []bool{false, true} {
			r := panelRow{s: s, class: panelGroupOf(s), band: band}
			for _, w := range []int{panelWidth, 12, 1} {
				tpl := panelDefaultSet.Row
				if band {
					tpl = panelDefaultSet.AttnRow
				}
				if _, ok := tpl.Line(w, rowResolver(&r, rc)); !ok {
					t.Errorf("the built-in head (band %v) draws no line for %+v at %d columns", band, s, w)
				}
			}
		}
	}

	// The last resort: a template with nothing to say, as both the user's
	// and the built-in one, still leaves the session its name.
	empty := parsePanelConfForTest(t, "row = \"{kind}\"\n").Row
	r := panelRow{s: observer.Session{SessionID: "z", Name: "row-z"}}
	if got := headLine(empty, empty, panelWidth, rowResolver(&r, rc), rowName(r.s)); got != "row-z" {
		t.Errorf("with no template drawing, the head is %q, want the name", got)
	}
}

// parsePanelConfForTest parses src as a user's panel.conf, failing on any
// problem.
func parsePanelConfForTest(t *testing.T, src string) *panelfmt.Set {
	t.Helper()
	set := panelfmt.ParseConfig([]byte(src), panelRegistry, panelFormatOptions(), panelDefaultSet)
	if len(set.Problems) > 0 {
		t.Fatalf("%q: %v", src, set.Problems)
	}
	return set
}
