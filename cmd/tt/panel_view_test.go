package main

// The whole view's footer and keys (FR9, revision 4): the key strip pinned to
// the last line with at least two blank lines above it, the hidden-row count
// above that gap, the source note left out, the tab hotkeys of the sidebar,
// and text_lines.

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// The key strip is the panel's last line — the bottom row of a panel with a
// height, padded down to it — with at least two blank lines between it and
// the last line of content, and nothing after it. It names the surface's own
// keys: in the sidebar the cursor's j/k and ↵ first, then / and f; in the TUI
// its chords. No line counts hidden rows: a frame whose rows do not fit says
// so in its top-right corner instead.
// isMoreHint reports whether a plain line is a gap's "▲/▼ N more" hint.
func isMoreHint(l string) bool {
	l = strings.TrimSpace(l)
	return (strings.HasPrefix(l, "▼ ") || strings.HasPrefix(l, "▲ ")) && strings.HasSuffix(l, " more")
}

func TestTheKeyStripIsPinnedUnderTwoBlankLines(t *testing.T) {
	f := newFleet(t, fleetNow)
	keys := map[panelSurface][2]string{ // [still, scrolling]
		surfacePane: {"═══  j/k  move  ↵  go  /  filter  f  only  ═══", "═══  j/k  move  ↵  go  /  filter  f  only  ═══"},
		surfaceTUI:  {"════════════════  ^g  group  ═════════════════", "════════════════  ^g  group  ═════════════════"},
	}
	scrolled := 0
	for _, surface := range []panelSurface{surfacePane, surfaceTUI} {
		for _, mode := range panelModes {
			for _, h := range []int{0, 24, 30, 44, 59, 72, 120} {
				p := sidebar(t, f.now, f.snap, mode, h)
				p.surface = surface
				lines := plain(p.view())
				label := func() string { return strings.Join(lines, "\n") }
				if h > 0 && len(lines) != h {
					t.Errorf("%s, surface %d, %d lines: the frame is %d lines, want the whole height:\n%s", mode, surface, h, len(lines), label())
				}
				scrolls := strings.Contains(lines[0], "▼")
				want := keys[surface][0]
				if scrolls {
					scrolled++
					want = keys[surface][1]
				}
				last := len(lines) - 1
				if lines[last] != want {
					t.Errorf("%s, surface %d, %d lines: the last line is %q, want the key strip %q", mode, surface, h, lines[last], want)
					continue
				}
				// The gap may carry the "▼ N more" hint of a list that goes
				// on: it is still the gap, nothing else is drawn in it.
				gap := 0
				for i := last - 1; i >= 0 && (lines[i] == "" || isMoreHint(lines[i])); i-- {
					gap++
				}
				if gap < footerGap {
					t.Errorf("%s, surface %d, %d lines: %d blank line(s) above the key strip, want at least %d:\n%s", mode, surface, h, gap, footerGap, label())
				}
				if h == 0 && gap != footerGap {
					t.Errorf("%s, unbounded: %d blank lines above the key strip, want exactly %d", mode, gap, footerGap)
				}
				if overflowPattern.MatchString(label()) {
					t.Errorf("%s, surface %d, %d lines: a line counts hidden rows:\n%s", mode, surface, h, label())
				}
			}
		}
	}
	if scrolled == 0 {
		t.Fatal("no frame scrolled, so the scrolling key strip is not exercised")
	}

	// Narrower, the ══ fill goes first, then the labels.
	p := sidebar(t, f.now, f.snap, modeProject, 0)
	for width, want := range map[int]string{
		40: "  j/k  move  ↵  go  /  filter  f  only",
		38: " j/k  move  ↵  go  /  filter  f  only",
		37: "            j/k  ↵  /  f",
	} {
		p.width = width
		lines := plain(p.view())
		if got := lines[len(lines)-1]; got != want {
			t.Errorf("width %d: the key strip is %q, want %q", width, got, want)
		}
	}
}

// The scroll area: rows that do not fit move under a still header and
// footer; the top-right corner says where the window is (▲ rows above, ▼
// rows below, and how much has been seen). j and k move the cursor, and the
// window follows it: still while the cursor's row is in view, then a row at a
// time, stopping at either end; a new grouping starts at its top.
func TestRowsThatDoNotFitScrollWithJAndK(t *testing.T) {
	f := newFleet(t, fleetNow)
	pm := newPaneModel(newFakePanelSource(), newPanelClock(f.now), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 30})
	pm = updated.(paneModel)
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})
	// frame is the view with every gutter blanked: the cursor's ❯ pointer
	// stands in a row's gutter, so frames compare by where the list is, not
	// by which row is selected.
	frame := func() []string {
		lines := plain(pm.View())
		for i, l := range lines {
			for _, g := range []string{"❯", "▌"} {
				if strings.HasPrefix(l, g) {
					lines[i] = " " + l[len(g):]
				}
			}
		}
		return lines
	}
	press := func(k string) []string {
		updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		pm = updated.(paneModel)
		return frame()
	}
	top := frame()
	if !strings.HasSuffix(top[0], "▼") && !regexp.MustCompile(`▼ +\d+%$`).MatchString(top[0]) {
		t.Fatalf("a frame whose rows do not fit has no ▼ in its corner: %q", top[0])
	}
	if strings.Contains(top[0], "▲") {
		t.Errorf("at the top the corner says there are rows above: %q", top[0])
	}
	if k := press("k"); strings.Join(k, "\n") != strings.Join(top, "\n") {
		t.Errorf("k at the top moved the list")
	}
	if still := press("j"); pm.panel.scroll != 0 || strings.Join(still, "\n") != strings.Join(top, "\n") {
		t.Errorf("j onto a row in view moved the window (scroll %d)", pm.panel.scroll)
	}
	var down []string
	for range 60 {
		if down = press("j"); pm.panel.scroll > 0 {
			break
		}
	}
	if pm.panel.scroll == 0 {
		t.Fatalf("the cursor walked 60 rows and the window never followed it")
	}
	if s, _ := pm.panel.cursorSession(); !strings.Contains(strings.Join(down, "\n"), s.Name) {
		t.Errorf("the window scrolled without bringing the cursor's %s into view", s.Name)
	}
	// The header stays put; only its gap may now say "▲ N more".
	for i := 1; i < headerLines+headerGap; i++ {
		if down[i] != top[i] && !(top[i] == "" && isMoreHint(down[i])) {
			t.Errorf("the header's line %d moved with the rows: %q → %q", i, top[i], down[i])
		}
	}
	if !strings.Contains(down[0], "▲") {
		t.Errorf("after j the corner does not say there are rows above: %q", down[0])
	}
	var last []string
	for range 200 {
		last = press("j")
	}
	if strings.Contains(last[0], "▼") || !strings.Contains(last[0], "100%") {
		t.Errorf("at the bottom the corner is %q, want no ▼ and 100%%", last[0])
	}
	for range 200 {
		last = press("k")
	}
	if strings.Contains(last[0], "▲") || pm.panel.scroll != 0 {
		t.Errorf("back at the first row the corner is %q (scroll %d), want the top", last[0], pm.panel.scroll)
	}
	// The arrows scroll too, and a new grouping starts at its top.
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyDown})
	pm = updated.(paneModel)
	press("t")
	if pm.panel.scroll != 0 {
		t.Errorf("a new grouping kept the scroll offset %d", pm.panel.scroll)
	}
}

// The filter: / opens its prompt on the last line, typed keys narrow the rows
// to the sessions whose name, tmux session or folder holds the text (letters
// that are keys elsewhere — q, g, the tab keys — type), backspace erases,
// enter keeps the filter, esc clears it; a kept filter is cleared by esc
// before esc closes anything. The header's counts stay the fleet's.
func TestSlashFiltersTheRows(t *testing.T) {
	var saved []panelMode
	stubSeam(t, &savePanelMode, func(m panelMode) error { saved = append(saved, m); return nil })
	f := newFleet(t, fleetNow)
	pm := newPaneModel(newFakePanelSource(), newPanelClock(f.now), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 59})
	pm = updated.(paneModel)
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})
	send := func(msg tea.KeyMsg) []string {
		updated, cmd := pm.Update(msg)
		pm = updated.(paneModel)
		if cmd != nil {
			t.Fatalf("%v returned a command (quit?)", msg)
		}
		return plain(pm.View())
	}
	typeText := func(s string) []string {
		return send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	}
	strip := plain(pm.View())[2]
	names := func(lines []string) []string {
		var out []string
		for _, r := range frameRows(parseFrame(lines)) {
			out = append(out, r.name)
		}
		return out
	}

	lines := send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if got := lines[len(lines)-1]; !strings.HasPrefix(got, " /  ▏") || !strings.HasSuffix(got, "enter  keep  esc  clear") {
		t.Errorf("/ does not open the prompt on the last line: %q", got)
	}
	for _, k := range []string{"m", "a", "g", "u", "s"} { // g and m are keys outside the prompt
		lines = typeText(k)
	}
	if pm.panel.mode != modeProject || len(saved) != 0 {
		t.Errorf("typing into the filter ran a key: mode %s, saved %v", pm.panel.mode, saved)
	}
	got := names(lines)
	if len(got) == 0 {
		t.Fatalf("the filter magus matches nothing:\n%s", strings.Join(lines, "\n"))
	}
	// Exactly the live sessions the filter matches, by name, tmux session or
	// folder, and at least one of them by name.
	var want []string
	byName := false
	for _, s := range filterSessions(f.snap.Sessions, "magus") {
		if panelGroupOf(s) != groupEnded {
			want = append(want, s.Name)
			byName = byName || strings.Contains(s.Name, "magus")
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !byName || !slices.Equal(got, want) {
		t.Errorf("the filter magus shows %v, want %v", got, want)
	}
	if lines[2] != strip {
		t.Errorf("the header's counts follow the filter: %q, want the fleet's %q", lines[2], strip)
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "/  magus▏") {
		t.Errorf("the prompt does not show the text: %q", last)
	}

	send(tea.KeyMsg{Type: tea.KeyBackspace})
	send(tea.KeyMsg{Type: tea.KeyBackspace})
	lines = typeText("zz")
	if !strings.Contains(strings.Join(lines, "\n"), "no session matches /magzz") {
		t.Errorf("a filter with no match does not say so:\n%s", strings.Join(lines, "\n"))
	}
	send(tea.KeyMsg{Type: tea.KeyBackspace})
	send(tea.KeyMsg{Type: tea.KeyBackspace})
	lines = send(tea.KeyMsg{Type: tea.KeyEnter})
	if pm.panel.filtering || pm.panel.filter != "mag" {
		t.Fatalf("enter did not keep the filter: filtering %v, %q", pm.panel.filtering, pm.panel.filter)
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "/  mag ") || strings.Contains(last, "▏") || !strings.HasSuffix(last, "esc  clear") {
		t.Errorf("a kept filter's line is %q", last)
	}
	// Kept, the keys are keys again: g groups by status.
	typeText("g")
	if pm.panel.mode != modeStatus {
		t.Errorf("g after enter did not group: %s", pm.panel.mode)
	}
	// esc clears a kept filter — and does not close the pane.
	lines = send(tea.KeyMsg{Type: tea.KeyEsc})
	if pm.panel.filter != "" || len(names(lines)) < len(got) {
		t.Errorf("esc did not clear the filter (%q, %d rows)", pm.panel.filter, len(names(lines)))
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "/  filter") {
		t.Errorf("after esc the key strip is not back: %q", last)
	}
	// esc inside the prompt clears it too.
	typeText("/")
	typeText("x")
	send(tea.KeyMsg{Type: tea.KeyEsc})
	if pm.panel.filtering || pm.panel.filter != "" {
		t.Errorf("esc in the prompt left filtering %v, %q", pm.panel.filtering, pm.panel.filter)
	}
	if got := filterSessions(f.snap.Sessions, "  "); len(got) != len(f.snap.Sessions) {
		t.Errorf("a blank filter keeps %d of %d sessions", len(got), len(f.snap.Sessions))
	}
}

// The note that only names the source — "via tt observe (pid N)", or a
// snapshot's date — is not drawn; a note that something is wrong is, dim.
func TestOnlyANoteThatSaysSomethingIsWrongIsDrawn(t *testing.T) {
	withPanelTheme(t, true)
	f := newFleet(t, fleetNow)
	p := sidebar(t, f.now, f.snap, modeProject, 0)
	for _, quiet := range []string{"via tt observe (pid 9)", "snapshot", "snapshot of 2026-09-25 15:02 UTC"} {
		p.note = quiet
		if view := ansi.Strip(p.view()); strings.Contains(view, quiet) {
			t.Errorf("the source note %q is drawn", quiet)
		}
	}
	for _, loud := range []string{noteObserverStopped, "watch degraded: kqueue", "refresh failed: EOF", "grouping not saved: read-only"} {
		p.note = loud
		view := p.view()
		if !strings.Contains(view, panelNoteStyle.Render(loud)) {
			t.Errorf("the note %q is not drawn dim:\n%s", loud, ansi.Strip(view))
		}
	}
	// Before any snapshot too.
	p.snap, p.note = nil, "via tt observe (pid 9)"
	if view := ansi.Strip(p.view()); strings.Contains(view, "via tt observe") || !strings.Contains(view, "waiting for the observer") {
		t.Errorf("the waiting frame draws the source note, or not what it waits for:\n%s", view)
	}
}

// The sidebar's tab hotkeys: the letter each tab shows in brackets, in either
// case, goes straight to that grouping and is saved like g's cycle; g still
// cycles, and s (the start button's key), q, d and r are not tab keys. In the
// TUI the same letters are search characters: they never change the grouping,
// and neither the tabs nor the help line name them.
func TestTheTabHotkeysGoStraightToAGrouping(t *testing.T) {
	var saved []panelMode
	stubSeam(t, &savePanelMode, func(m panelMode) error { saved = append(saved, m); return nil })
	now := fleetNow

	pm := newPaneModel(newFakePanelSource(), newPanelClock(now), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	press := func(k string) {
		updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		pm = updated.(paneModel)
	}
	for _, c := range []struct {
		key  string
		want panelMode
	}{
		{"t", modeStatus}, {"C", modeColour}, {"p", modeProject}, {"M", modeTmux},
		{"P", modeProject}, {"T", modeStatus}, {"m", modeTmux}, {"c", modeColour},
	} {
		n := len(saved)
		press(c.key)
		if pm.panel.mode != c.want || len(saved) != n+1 || saved[n] != c.want {
			t.Errorf("%s: mode %s, saved %v; want %s, saved once", c.key, pm.panel.mode, saved[n:], c.want)
		}
	}
	press("g") // from colour, the cycle wraps to project
	if pm.panel.mode != modeProject {
		t.Errorf("g after colour: %s, want project", pm.panel.mode)
	}
	for _, k := range []string{"s", "S", "q", "g", "d", "r", "x", "enter"} {
		if _, ok := panelModeForKey(k); ok {
			t.Errorf("%q is a tab hotkey", k)
		}
	}
	// Each tab's bracketed letter is its key.
	for _, tab := range panelTabs {
		if got := tabText(tab.label, tab.hot, true); !strings.Contains(strings.ToLower(got), "["+tab.key()+"]") {
			t.Errorf("tab %s shows %q, want its key %s in brackets", tab.label, got, tab.key())
		}
	}

	// The TUI: p types into the search box and the grouping stays.
	m := newTestModel()
	m.panel.clock = newPanelClock(now)
	m, _ = applyMsg(m, tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	before, n := m.panel.mode, len(saved)
	for _, k := range []string{"p", "t", "m", "c"} {
		m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
	if m.panel.mode != before || len(saved) != n {
		t.Errorf("typing p t m c in the TUI changed the grouping to %s (saved %v)", m.panel.mode, saved[n:])
	}
	help := ansi.Strip(m.footer())
	m.panel.update(panelEventMsg{gen: m.panel.gen, ev: snapshotEvent(&observer.Snapshot{ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{}})})
	panelView := ansi.Strip(m.panel.view())
	for _, bad := range []string{"[P]", "[t]", "[m]", "[C]"} {
		if strings.Contains(help, bad) || strings.Contains(panelView, bad) {
			t.Errorf("the TUI names the sidebar's tab key %s:\n%s\n%s", bad, help, panelView)
		}
	}
	m.panel.close()
}

// text_lines = 0 draws no line under any row: a one-line panel stays one line.
func TestTextLinesZeroKeepsEveryRowToOneLine(t *testing.T) {
	f := newFleet(t, fleetNow)
	withLatestText(t, &f)
	conf := newConfFixture(t)
	conf.write(t, "text_lines = 0\nmeta = \"\"\n", time.Now())
	p := sidebar(t, f.now, f.snap, modeProject, 0)
	for _, l := range parseFrame(plain(p.view())) {
		if l.kind == lineIndented {
			t.Errorf("a line under a row with text_lines = 0: %q", l.text)
		}
	}
	conf.write(t, "text_lines = 1\n", time.Now().Add(time.Second))
	p.followConfig()
	c := *f.snap
	deliver(t, p, snapshotEvent(&c))
	fl := parseFrame(plain(p.view()))
	for _, r := range frameRows(fl) {
		if under, _ := bandLines(fl, r.name); len(under) > 2 { // one text line, and a folded row's group
			t.Errorf("%s draws %d lines under it with text_lines = 1: %q", r.name, len(under), under)
		}
	}
}
