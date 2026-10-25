package main

import (
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The sidebar shows whether it holds the focus. Focused, the chrome is lit:
// the active tab filled with the accent, the thick baseline under it and the
// key strip's ══ in the accent, every shortcut shown. At rest — a BlurMsg,
// sent by tmux when the pane loses the focus — the active tab keeps only its
// accent text, the baseline falls back to the rule colour, and no shortcut is
// drawn: the tabs lose their brackets and the footer is a plain ══ rule. A
// FocusMsg lights it again. Below the baseline, only the footer changes.
func TestTheSidebarLightsUpWhenItHoldsTheFocus(t *testing.T) {
	for _, dark := range []bool{true, false} {
		withPanelTheme(t, dark)
		pick := func(c lipgloss.AdaptiveColor) string {
			if dark {
				return c.Dark
			}
			return c.Light
		}
		v := panelPal.view
		accent := sgrRGB(t, 38, pick(v.tabOnBg)) + "m"
		rule := sgrRGB(t, 38, pick(panelPal.rule)) + "m"
		litTab := "1;" + sgrRGB(t, 38, pick(v.tabOnFg)) + ";" + sgrRGB(t, 48, pick(v.tabOnBg)) + "m"
		restTab := "1;" + sgrRGB(t, 38, pick(v.tabOnBg)) + ";" + sgrRGB(t, 48, pick(v.tabOffBg)) + "m"

		pm, _ := pickPane(t)
		lit := pm.View()
		for what, want := range map[string]string{"filled tab": litTab, "accent baseline": accent + "━━━", "accent ══": accent + "═══"} {
			if !strings.Contains(lit, want) {
				t.Errorf("dark=%v focused: no %s", dark, what)
			}
		}

		updated, _ := pm.Update(tea.BlurMsg{})
		*pm = updated.(paneModel)
		rest := pm.View()
		if strings.Contains(rest, litTab) || !strings.Contains(rest, restTab) {
			t.Errorf("dark=%v at rest: the active tab is still filled, or not lettered in the accent", dark)
		}
		// The other tabs dim at rest: no fill, the dimmest text, so the active
		// tab is the only filled block on the tabs line.
		offFill := sgrRGB(t, 48, pick(v.tabOffBg)) + "m"
		tabsAt := func(view string) string { return strings.SplitN(view, "\n", 2)[0] }
		if dim := sgrRGB(t, 38, pick(panelPal.ash)) + "m Status"; !strings.Contains(tabsAt(rest), dim) {
			t.Errorf("dark=%v at rest: the other tabs are not dimmed: %q", dark, tabsAt(rest))
		}
		if n := strings.Count(tabsAt(rest), offFill); n != 1 {
			t.Errorf("dark=%v at rest: %d spans on the tab fill, want the active tab's alone", dark, n)
		}
		if strings.Contains(rest, accent+"━") || strings.Contains(rest, accent+"═") {
			t.Errorf("dark=%v at rest: the baseline or the ══ is still in the accent", dark)
		}
		if !strings.Contains(rest, rule+"━━━") || !strings.Contains(rest, rule+"═══") {
			t.Errorf("dark=%v at rest: the baseline and the ══ are not in the rule colour", dark)
		}
		litLines, restLines := plain(lit), plain(rest)
		if !strings.Contains(litLines[0], "[P]roj") || !strings.Contains(litLines[len(litLines)-1], "move") {
			t.Fatalf("dark=%v focused: the shortcuts are not shown: %q / %q", dark, litLines[0], litLines[len(litLines)-1])
		}
		if strings.Contains(restLines[0], "[") || !strings.Contains(restLines[0], "Proj") {
			t.Errorf("dark=%v at rest: the tabs still show their keys: %q", dark, restLines[0])
		}
		if foot := restLines[len(restLines)-1]; strings.Trim(foot, "═") != "  connected  " || len([]rune(foot)) != panelWidth {
			t.Errorf("dark=%v at rest: the footer is %q, want the ══ rule around %q", dark, foot, panelConnected)
		}
		// Below the baseline, focus changes only the cursor row's gutter (its
		// ❯ pointer, drawn while the sidebar holds the focus) and the footer.
		ungutter := func(lines []string) string {
			out := make([]string, len(lines))
			for i, l := range lines {
				out[i] = strings.Replace(strings.Replace(l, "❯", " ", 1), "▌", " ", 1)
			}
			return strings.Join(out, "\n")
		}
		if ungutter(restLines[2:len(restLines)-1]) != ungutter(litLines[2:len(litLines)-1]) {
			t.Errorf("dark=%v: focus moved more than the tabs, the baseline and the footer", dark)
		}

		// A filtered list at rest still says what it shows, without keys.
		pm.panel.pickStateOn, pm.panel.pickState = true, stripRun
		filtered := plain(pm.View())
		if foot := filtered[len(filtered)-1]; !strings.Contains(foot, "═  RUN · connected  ═") || strings.Contains(foot, "esc") {
			t.Errorf("dark=%v at rest, filtered: the footer is %q", dark, foot)
		}
		pm.panel.pickStateOn = false

		updated, _ = pm.Update(tea.FocusMsg{})
		*pm = updated.(paneModel)
		if pm.View() != lit {
			t.Errorf("dark=%v: a FocusMsg did not light the sidebar again", dark)
		}
	}
}

// tmux never says whether a pane starts with the focus, so the sidebar asks
// once: an unfocused pane starts at rest, and a focus change that arrives
// before the answer wins over it.
func TestTheSidebarAsksTmuxWhetherItStartsFocused(t *testing.T) {
	var asked []string
	answer := "0"
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
		asked = append(asked, strings.Join(args, " "))
		return answer + "\n", nil
	})
	pm := newPaneModel(newFakePanelSource(), newPanelClock(fleetNow), "", paneOptions{Pane: "%7"})
	t.Cleanup(pm.panel.close)
	msg := pm.askFocus()()
	if len(asked) != 1 || !strings.Contains(asked[0], "-t %7 "+paneFocusFormat) {
		t.Fatalf("asked tmux %q", asked)
	}
	updated, _ := pm.Update(msg)
	if !updated.(paneModel).panel.blurred {
		t.Error("a pane tmux says is not focused did not start at rest")
	}

	answer = "1"
	updated, _ = pm.Update(tea.BlurMsg{})
	updated, _ = updated.Update(pm.askFocus()())
	if !updated.(paneModel).panel.blurred {
		t.Error("the start-up answer overrode a focus change that came first")
	}

	if (paneModel{}).askFocus() != nil {
		t.Error("a pane with no id asked tmux")
	}
}

// At rest, where the keys were, the footer says the sidebar is connected to
// the observer — not a time, which over a quiet fleet would read as stale.
// Offline, the word goes and the panel's own note says what is wrong; focused,
// the keys come back.
func TestTheSidebarAtRestSaysItIsConnected(t *testing.T) {
	pm, _ := pickPane(t)
	updated, _ := pm.Update(tea.BlurMsg{})
	*pm = updated.(paneModel)
	foot := func() string { v := plain(pm.View()); return v[len(v)-1] }

	if f := foot(); strings.Trim(f, "═ ") != panelConnected || strings.Contains(f, "updated") {
		t.Errorf("at rest with a live feed the footer is %q", f)
	}

	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: observer.Event{Type: observer.EventResync, Reason: noteObserverNotRunning}})
	if f := foot(); strings.Contains(f, panelConnected) {
		t.Errorf("offline, the footer still says connected: %q", f)
	}

	updated, _ = pm.Update(tea.FocusMsg{})
	*pm = updated.(paneModel)
	if f := foot(); strings.Contains(f, panelConnected) {
		t.Errorf("focused, the footer is %q, want no connection word", f)
	}
}

// A key or a click reaches only the focused pane, so either lights a sidebar
// at rest even when tmux's focus-in never came; the wheel does not, tmux
// scrolls a pane without selecting it. A lit sidebar that tmux says lost the
// focus goes back to rest.
func TestAKeyOrAClickLightsTheSidebar(t *testing.T) {
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")},
		tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 1, Y: 30},
	} {
		pm, _ := pickPane(t)
		pm.panel.blurred = true
		updated, _ := pm.Update(msg)
		if updated.(paneModel).panel.blurred {
			t.Errorf("%T left the sidebar at rest", msg)
		}
	}
	pm, _ := pickPane(t)
	pm.panel.blurred = true
	updated, _ := pm.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if !updated.(paneModel).panel.blurred {
		t.Error("the wheel lit a sidebar tmux did not select")
	}
	updated, _ = updated.Update(paneFocusCheck{focused: false})
	lit := updated.(paneModel)
	lit.panel.blurred = false
	updated, _ = lit.Update(paneFocusCheck{focused: false})
	if !updated.(paneModel).panel.blurred {
		t.Error("a sidebar tmux says lost the focus stayed lit")
	}
}
