package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// click sends a left click at (x, y) to the pane.
func click(t *testing.T, pm *paneModel, x, y int) {
	t.Helper()
	updated, _ := pm.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	*pm = updated.(paneModel)
}

// colOf is the display column of the first occurrence of word in line.
func colOf(t *testing.T, line, word string) int {
	t.Helper()
	i := strings.Index(line, word)
	if i < 0 {
		t.Fatalf("%q is not on the line %q", word, line)
	}
	return len([]rune(line[:i]))
}

// A click on a tab switches to its grouping; a click on a count in the strip
// shows only that state and a second click turns it off; a click on a row
// puts the cursor on it.
func TestClicksActOnTabsCountsAndRows(t *testing.T) {
	pm, _ := pickPane(t)
	lines := plain(pm.View())

	click(t, pm, colOf(t, lines[0], "S[t]atus")+2, 0)
	if pm.panel.mode != modeStatus {
		t.Fatalf("a click on Status left the grouping %s", pm.panel.mode)
	}
	click(t, pm, colOf(t, lines[0], "olour"), 0)
	if pm.panel.mode != modeColour {
		t.Fatalf("a click on Colour left the grouping %s", pm.panel.mode)
	}

	lines = plain(pm.View())
	click(t, pm, colOf(t, lines[2], "RUN"), 2)
	if !pm.panel.pickStateOn || pm.panel.pickState != stripRun {
		t.Fatalf("a click on RUN did not show only RUN (on %v, state %v)", pm.panel.pickStateOn, pm.panel.pickState)
	}
	for _, r := range frameRows(parseFrame(plain(pm.View()))) {
		if r.code != "RUN" && r.code != "WRK" {
			t.Errorf("RUN alone shows a %s row, %s", r.code, r.name)
		}
	}
	lines = plain(pm.View())
	click(t, pm, colOf(t, lines[2], "RUN"), 2)
	if pm.panel.pickStateOn {
		t.Fatal("a second click on RUN did not turn the filter off")
	}

	pm.View()
	nav := pm.panel.memo.nav
	target := nav[2]
	y := pm.panel.memo.bodyTop + target.first - pm.panel.memo.bodyOff
	click(t, pm, 20, y)
	if s, _ := pm.panel.cursorSession(); s.Name != target.s.Name {
		t.Errorf("a click on %s's row put the cursor on %s", target.s.Name, s.Name)
	}
	// A click on the row's text line under it picks the same row.
	if target.last > target.first {
		click(t, pm, 20, 0) // away, on the tabs' gap: nothing
		click(t, pm, 20, y+1)
		if s, _ := pm.panel.cursorSession(); s.Name != target.s.Name {
			t.Errorf("a click under %s's row put the cursor on %s", target.s.Name, s.Name)
		}
	}
}

// The wheel scrolls the list, three lines a notch, within its range.
func TestTheWheelScrollsTheList(t *testing.T) {
	pm := newPaneModel(newFakePanelSource(), newPanelClock(fleetNow), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	f := newFleet(t, fleetNow)
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 30})
	pm = updated.(paneModel)
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})
	pm.View()
	if pm.panel.memo.scrollMax == 0 {
		t.Fatal("the fleet fits in 30 lines; the wheel has nothing to scroll")
	}
	wheel := func(b tea.MouseButton) {
		updated, _ := pm.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: b})
		pm = updated.(paneModel)
	}
	hint := func(arrow string) bool {
		for _, l := range plain(pm.View()) {
			if strings.HasPrefix(strings.TrimSpace(l), arrow+" ") && strings.HasSuffix(l, " more") {
				return true
			}
		}
		return false
	}
	if !hint("▼") || hint("▲") {
		t.Errorf("at the top: ▼ more %v (want true), ▲ more %v (want false)", hint("▼"), hint("▲"))
	}
	wheel(tea.MouseButtonWheelDown)
	if pm.panel.scroll != panelWheelLines {
		t.Errorf("one notch down scrolled to %d, want %d", pm.panel.scroll, panelWheelLines)
	}
	if !hint("▲") {
		t.Error("scrolled down, nothing says there are rows above")
	}
	wheel(tea.MouseButtonWheelUp)
	wheel(tea.MouseButtonWheelUp)
	if pm.panel.scroll != 0 {
		t.Errorf("two notches up from 3 scrolled to %d, want the top", pm.panel.scroll)
	}
}
