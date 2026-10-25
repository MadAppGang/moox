package main

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pickPane is a 46×80 sidebar over the fleet, in project mode.
func pickPane(t *testing.T) (*paneModel, fleet) {
	t.Helper()
	var saved []panelMode
	stubSeam(t, &savePanelMode, func(m panelMode) error { saved = append(saved, m); return nil })
	f := newFleet(t, fleetNow)
	pm := newPaneModel(newFakePanelSource(), newPanelClock(f.now), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	pm.panel.mode = modeProject
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 80})
	pm = updated.(paneModel)
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})
	return &pm, f
}

// press sends one key to the pane and returns its plain view.
func press(t *testing.T, pm *paneModel, k string) []string {
	t.Helper()
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	if k == "esc" {
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	}
	updated, _ := pm.Update(msg)
	*pm = updated.(paneModel)
	return plain(pm.View())
}

// titles is the section titles a frame draws, in order.
func titles(lines []string) []string {
	var out []string
	for _, l := range parseFrame(lines) {
		if l.kind == lineHeading {
			out = append(out, l.heading)
		}
	}
	return out
}

// f opens the picker: every section title leads with its key in brackets, in
// the title's own colours, the count strip's words with their state letters,
// and the footer says what the keys do. In the picker a section's key shows
// only that section and a state's letter only the rows in that state; the
// picker stays open so the two combine, and the same key again turns its
// filter off. f closes the picker; with a filter on, f turns the filters off;
// esc clears them all; the header's counts stay the fleet's throughout.
func TestFPicksASectionAndAStateAndTheyCombine(t *testing.T) {
	pm, f := pickPane(t)
	all := plain(pm.View())
	sections := titles(all)
	strip := all[2]

	picking := press(t, pm, "f")
	if !pm.panel.picking {
		t.Fatal("f did not open the picker")
	}
	keyed := regexp.MustCompile(`^─{3,}  \[(\S)\] (.+?)  (\d+)  ─{3,}$`)
	keys := map[string]string{}
	for _, l := range picking {
		if m := keyed.FindStringSubmatch(l); m != nil {
			keys[m[2]] = m[1]
		}
	}
	if len(keys) != len(sections) || keys[sections[0]] != "1" || keys[sections[1]] != "2" {
		t.Fatalf("the picker's keys %v, want one per section, [1] first:\n%s", keys, strings.Join(picking, "\n"))
	}
	if s := picking[2]; !strings.Contains(s, "[N]EED") || !strings.Contains(s, "[R]UN") || !strings.Contains(s, "[I]DL") {
		t.Errorf("the strip's words do not lead with the state letters: %q", s)
	}
	if foot := picking[len(picking)-1]; !strings.Contains(foot, "1-9  section") || !strings.Contains(foot, "nriu  state") || !strings.Contains(foot, "f  done") {
		t.Errorf("the picker's footer is %q", foot)
	}
	// The badge is drawn in the title's colours, not as a key cap of its own.
	sec := pm.panel.frame().secs[0]
	painted := pm.panel.sectionTitle(&sec)
	if !strings.Contains(painted, "[1] "+sec.title) {
		t.Errorf("the badge is painted apart from its title: %q", painted)
	}

	// A section: claudish, whatever its key. The pick closes the picker: the
	// filtered list is there at once.
	only := press(t, pm, keys["claudish"])
	if pm.panel.picking || strings.Join(titles(only), ",") != "claudish" {
		t.Fatalf("picking claudish shows %v (picking %v):\n%s", titles(only), pm.panel.picking, strings.Join(only, "\n"))
	}
	if only[2] != strip {
		t.Errorf("the strip followed the filter: %q, want the fleet's %q", only[2], strip)
	}
	if foot := only[len(only)-1]; !strings.Contains(foot, "f  claudish") || !strings.Contains(foot, "esc  clear") {
		t.Errorf("the footer does not say what is shown: %q", foot)
	}

	// f with a filter on opens the picker again; the section's key again
	// turns its filter off and closes it.
	press(t, pm, "f")
	if !pm.panel.picking || pm.panel.pickSection == "" {
		t.Fatalf("f with a filter on did not reopen the picker (picking %v, section %q)", pm.panel.picking, pm.panel.pickSection)
	}
	if back := press(t, pm, keys["claudish"]); pm.panel.picking || pm.panel.anyPick() || strings.Join(back, "\n") != strings.Join(all, "\n") {
		t.Fatalf("the section's key again did not turn it off (picking %v, filters %v)", pm.panel.picking, pm.panel.anyPick())
	}

	// Combined, one f each: claudish's RUN rows only.
	press(t, pm, "f")
	press(t, pm, keys["claudish"])
	press(t, pm, "f")
	both := press(t, pm, "r")
	for _, r := range frameRows(parseFrame(both)) {
		if r.code != "RUN" && r.code != "WRK" || !strings.HasPrefix(r.name, "claudish-") {
			t.Errorf("claudish + RUN shows %s %s", r.code, r.name)
		}
	}
	if len(frameRows(parseFrame(both))) == 0 {
		t.Fatalf("claudish + RUN shows nothing:\n%s", strings.Join(both, "\n"))
	}
	if foot := both[len(both)-1]; !strings.Contains(foot, "claudish · RUN") {
		t.Errorf("the footer does not say both filters: %q", foot)
	}

	// The section's key again turns it off; the state stays: RUN rows
	// everywhere. The key finds the section though the filter hid the rest.
	press(t, pm, "f")
	run := press(t, pm, keys["claudish"])
	if pm.panel.pickSection != "" || !pm.panel.pickStateOn {
		t.Fatalf("the same section key did not turn its filter off (section %q, state on %v)", pm.panel.pickSection, pm.panel.pickStateOn)
	}
	running := 0
	for _, s := range f.snap.Sessions {
		if panelGroupOf(s) == groupActive {
			running++
		}
	}
	if got := len(frameRows(parseFrame(run))); got != running {
		t.Errorf("RUN alone shows %d rows, want the fleet's %d active sessions", got, running)
	}

	// esc in the picker closes it and keeps the filters; esc outside it
	// clears everything, and does not close the pane.
	press(t, pm, "f")
	press(t, pm, "esc")
	if pm.panel.picking || !pm.panel.anyPick() {
		t.Fatalf("esc in the picker did not just close it (picking %v, filters %v)", pm.panel.picking, pm.panel.anyPick())
	}
	cleared := press(t, pm, "esc")
	if pm.panel.anyPick() || !pm.panel.open || strings.Join(titles(cleared), ",") != strings.Join(sections, ",") {
		t.Errorf("esc did not bring the whole list back (filters %v):\n%s", pm.panel.anyPick(), strings.Join(cleared, "\n"))
	}
	// f, then f again, closes the picker and changes nothing.
	press(t, pm, "f")
	if back := press(t, pm, "f"); pm.panel.picking || strings.Join(back, "\n") != strings.Join(cleared, "\n") {
		t.Error("f f changed the view")
	}
}

// The picker and the filter's prompt take the keys, so the cursor is hidden
// while either is open; a cursor key closes the picker and moves the cursor
// over what the pick kept (the user, 2026-10-06: "when navigation is not
// working we should hide the selection marker").
func TestTheCursorHidesWhileThePickerIsOpen(t *testing.T) {
	withPanelTheme(t, true)
	pm, _ := pickPane(t)
	pm.panel.blurred = false
	if row := cursorRow(t, pm.View()); row == "" {
		t.Fatal("a focused sidebar draws no cursor")
	}
	press(t, pm, "f")
	if row := cursorRow(t, pm.View()); row != "" {
		t.Errorf("the open picker still draws the cursor on %q", row)
	}
	press(t, pm, "1")
	if row := cursorRow(t, pm.View()); row == "" {
		t.Error("after a pick the cursor is not back")
	}
	pm.View()
	nav := pm.panel.memo.nav
	if len(nav) < 2 {
		t.Fatalf("the first section draws %d rows, want 2 or more", len(nav))
	}
	press(t, pm, "j")
	if s, _ := pm.panel.cursorSession(); s.Name != nav[1].s.Name {
		t.Errorf("j after f 1 left the cursor on %s, want %s", s.Name, nav[1].s.Name)
	}
	press(t, pm, "f")
	press(t, pm, "k")
	if s, _ := pm.panel.cursorSession(); pm.panel.picking || s.Name != nav[0].s.Name {
		t.Errorf("k in the picker: picking %v, cursor on %s, want it closed and on %s", pm.panel.picking, s.Name, nav[0].s.Name)
	}
	press(t, pm, "/")
	if row := cursorRow(t, pm.View()); row != "" {
		t.Errorf("the filter's prompt still draws the cursor on %q", row)
	}
}

// While the picker is open its keys are its own: g, q and the tab keys do not
// regroup, quit or switch.
// A sidebar with no snapshot yet (still waiting, or its observer went away)
// takes the picker's keys without crashing: it has no sections to key. This
// panicked in the live sidebar on 2026-10-06.
func TestThePickerWithNoSnapshotDoesNotCrash(t *testing.T) {
	pm := newPaneModel(newFakePanelSource(), newPanelClock(fleetNow), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 40})
	pm = updated.(paneModel)
	for _, k := range []string{"f", "1", "f", "r", "j"} {
		updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		pm = updated.(paneModel)
	}
	pm.View()
}

func TestThePickerTakesTheNextKey(t *testing.T) {
	pm, _ := pickPane(t)
	press(t, pm, "f")
	for _, k := range []string{"g", "q", "p"} {
		updated, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		*pm = updated.(paneModel)
		if cmd != nil || pm.panel.mode != modeProject || !pm.panel.open {
			t.Errorf("%q in the picker acted: mode %s, open %v", k, pm.panel.mode, pm.panel.open)
		}
		pm.panel.picking = true
	}
}
