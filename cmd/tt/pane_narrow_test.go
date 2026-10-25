package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// narrowPane is a sidebar of the given size over the fleet, its tmux calls
// recorded: what it asked tmux to set, and @tt_obs_width answering width.
func narrowPane(t *testing.T, w, h int, obsWidth string) (*paneModel, *[]string) {
	t.Helper()
	var calls []string
	stubSeam(t, &tmuxSet, func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil })
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) { return obsWidth + "\n", nil })
	f := newFleet(t, fleetNow)
	pm := newPaneModel(newFakePanelSource(), newPanelClock(f.now), "", paneOptions{Pane: "%9"})
	t.Cleanup(pm.panel.close)
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: w, Height: h})
	pm = updated.(paneModel)
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})
	return &pm, &calls
}

// run executes a command and, for a batch, every command in it.
func run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			run(c)
		}
	}
}

// Below paneMinWidth the sidebar draws "make me bigger" and a widen button
// instead of a frame cut to nothing — at every width from one column up, never
// wider than the pane, centred, and with the button whatever the height.
func TestATooNarrowSidebarAsksToBeMadeBigger(t *testing.T) {
	if paneMinWidth != 36 {
		t.Fatalf("paneMinWidth = %d; a row needs 17 + 12 + 1 + 6 = 36", paneMinWidth)
	}
	for w := 1; w < paneMinWidth; w++ {
		for _, h := range []int{3, 10, 40} {
			pm, _ := narrowPane(t, w, h, "46")
			view := pm.View()
			lines := strings.Split(view, "\n")
			label := fmt.Sprintf("%dx%d", w, h)
			if len(lines) > h {
				t.Errorf("%s: %d lines", label, len(lines))
			}
			for i, l := range lines {
				if sw := ansi.StringWidth(l); sw > w {
					t.Errorf("%s: line %d is %d wide: %q", label, i, sw, ansi.Strip(l))
				}
			}
			plain := ansi.Strip(view)
			if !strings.Contains(plain, paneWidenKey) {
				t.Errorf("%s: no widen button:\n%s", label, plain)
			}
			if w >= len(paneNarrowText) && h >= 10 && !strings.Contains(plain, paneNarrowText) {
				t.Errorf("%s: does not say %q:\n%s", label, paneNarrowText, plain)
			}
			if strings.Contains(plain, "NEEDS YOU") {
				t.Errorf("%s: draws the panel anyway:\n%s", label, plain)
			}
		}
	}
	// At its minimum the panel itself is drawn.
	pm, _ := narrowPane(t, paneMinWidth, 40, "46")
	if !strings.Contains(ansi.Strip(pm.View()), "NEEDS YOU") || strings.Contains(ansi.Strip(pm.View()), paneNarrowText) {
		t.Errorf("at %d columns the panel is not drawn:\n%s", paneMinWidth, ansi.Strip(pm.View()))
	}
}

// The button: w, or a click anywhere in the narrow pane, asks tmux to resize
// this pane to @tt_obs_width (46 when unset or unusable); mouse reporting is
// on while the narrow state shows; at a usable width w does nothing.
func TestTheWidenButtonResizesThePaneToTheSidebarsWidth(t *testing.T) {
	pm, calls := narrowPane(t, 1, 40, "50")
	if !pm.mouse {
		t.Error("the narrow state did not take the mouse for its button")
	}
	_, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	run(cmd)
	if got := strings.Join(*calls, " | "); !strings.Contains(got, "resize-pane -t %9 -x 50") {
		t.Errorf("w asked tmux %q, want resize-pane -t %%9 -x 50", got)
	}

	pm, calls = narrowPane(t, 10, 20, "")
	_, cmd = pm.Update(tea.MouseMsg{X: 3, Y: 17, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	run(cmd)
	if got := strings.Join(*calls, " | "); !strings.Contains(got, "resize-pane -t %9 -x 46") {
		t.Errorf("a click asked tmux %q, want resize-pane -t %%9 -x 46 (the default)", got)
	}

	pm, calls = narrowPane(t, panelWidth, 40, "46")
	*calls = nil
	_, cmd = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	run(cmd)
	if len(*calls) != 0 {
		t.Errorf("w at a usable width asked tmux %v", *calls)
	}
}
