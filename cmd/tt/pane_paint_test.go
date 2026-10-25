package main

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The painter writes a frame's lines the first time, nothing for an equal
// frame, only the changed line for a frame that differs in one, and erases
// what a shorter frame leaves below it.
func TestThePainterWritesOnlyTheLinesThatChanged(t *testing.T) {
	var out bytes.Buffer
	fp := &framePainter{out: &out}
	fp.resize(10, 5)
	out.Reset()

	if !fp.paint("one\ntwo\nthree") {
		t.Fatal("the first frame wrote nothing")
	}
	for i, l := range []string{"one", "two", "three"} {
		if !strings.Contains(out.String(), ansi.CursorPosition(1, i+1)+l) {
			t.Errorf("the first frame did not write line %d, %q: %q", i+1, l, out.String())
		}
	}

	out.Reset()
	if fp.paint("one\ntwo\nthree") || out.Len() != 0 {
		t.Errorf("an unchanged frame wrote %q", out.String())
	}

	out.Reset()
	fp.paint("one\nTWO\nthree")
	if got := out.String(); !strings.Contains(got, ansi.CursorPosition(1, 2)+"TWO") ||
		strings.Contains(got, "one") || strings.Contains(got, "three") {
		t.Errorf("one changed line wrote %q, want line 2 alone", got)
	}

	out.Reset()
	fp.paint("one")
	if got := out.String(); !strings.Contains(got, ansi.CursorPosition(1, 2)+ansi.EraseScreenBelow) {
		t.Errorf("a shorter frame did not erase below it: %q", got)
	}

	// A resize repaints everything.
	fp.resize(10, 5)
	out.Reset()
	fp.paint("one")
	if !strings.Contains(out.String(), "one") {
		t.Errorf("after a resize the frame was not repainted: %q", out.String())
	}
}

// A key is painted in the same Update that handles it — no renderer tick to
// wait for — and only the lines it changed are written.
func TestTheSidebarPaintsAKeyAtOnce(t *testing.T) {
	withPanelTheme(t, true)
	inner, _ := pickPane(t)
	inner.panel.blurred = false
	var out bytes.Buffer
	fp := &framePainter{out: &out}
	var m tea.Model = paintedPane{pane: *inner, painter: fp}
	m, _ = m.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 80})
	full := out.Len()
	if full == 0 {
		t.Fatal("the first frame painted nothing")
	}
	out.Reset()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	moved := out.Len()
	if moved == 0 {
		t.Fatal("j painted nothing in its own Update")
	}
	if moved*2 > full {
		t.Errorf("j rewrote %d of the frame's %d bytes, want only the lines it changed", moved, full)
	}
}

// Runes that arrive in one read, as a held j does, are each their own key:
// "jjj" moves the cursor three rows.
func TestRunesThatArriveTogetherAreEachAKey(t *testing.T) {
	pm, _ := pickPane(t)
	pm.View()
	nav := pm.panel.memo.nav
	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jjj")})
	*pm = updated.(paneModel)
	if s, _ := pm.panel.cursorSession(); s.Name != nav[3].s.Name {
		t.Errorf("jjj put the cursor on %s, want %s, three rows down", s.Name, nav[3].s.Name)
	}
	// In the filter's prompt they stay text.
	press(t, pm, "/")
	updated, _ = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mag")})
	*pm = updated.(paneModel)
	if pm.panel.filter != "mag" {
		t.Errorf("typing mag into the prompt in one read gave the filter %q", pm.panel.filter)
	}
}

// The painter follows the pane's mouse wish, writing the mode only when it
// changes.
func TestThePainterFollowsTheMouse(t *testing.T) {
	var out bytes.Buffer
	fp := &framePainter{out: &out}
	fp.setMouse(true)
	fp.setMouse(true)
	if got := strings.Count(out.String(), paintMouseOn); got != 1 {
		t.Errorf("mouse on twice wrote the mode %d times, want once", got)
	}
	out.Reset()
	fp.leave()
	if !strings.Contains(out.String(), paintMouseOff) || !strings.Contains(out.String(), paintLeave) {
		t.Errorf("leave did not put the mouse and the screen back: %q", out.String())
	}
}
