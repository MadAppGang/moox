package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// recorder is a bubbletea model that keeps every message and quits after n
// of them have arrived (the program's own start-up messages aside).
type recorder struct {
	want int
	got  []tea.Msg
}

func (r *recorder) Init() tea.Cmd { return nil }

func (r *recorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.WindowSizeMsg:
		return r, nil
	}
	r.got = append(r.got, msg)
	if len(r.got) >= r.want {
		return r, tea.Quit
	}
	return r, nil
}

func (r *recorder) View() string { return "" }

// parsedMsgs runs a real bubbletea program over input and returns the first n
// messages its input parser delivered: what tt's Update sees on a terminal.
func parsedMsgs(t *testing.T, input string, n int) []tea.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := &recorder{want: n}
	p := tea.NewProgram(r, tea.WithInput(strings.NewReader(input)), tea.WithOutput(io.Discard),
		tea.WithContext(ctx), tea.WithoutSignalHandler())
	if _, err := p.Run(); err != nil {
		t.Fatalf("the program over %q ended with %v after %d messages", input, err, len(r.got))
	}
	return r.got
}

// The terminal's reports arrive through bubbletea's real input parser as
// messages themeReport recognises — dark (997;1), light (997;2) — and nothing
// else is mistaken for one: a key, or another CSI sequence.
func TestTheBubbleteaInputParserDeliversAThemeReport(t *testing.T) {
	msgs := parsedMsgs(t, "\x1b[?997;2nx\x1b[?997;1n\x1b[?998;1n", 4)
	want := []struct{ dark, ok bool }{{false, true}, {false, false}, {true, true}, {false, false}}
	for i, w := range want {
		dark, ok := themeReport(msgs[i])
		if dark != w.dark || ok != w.ok {
			t.Errorf("message %d (%T %v): themeReport = %v, %v, want %v, %v", i, msgs[i], msgs[i], dark, ok, w.dark, w.ok)
		}
	}
	if _, ok := themeReport(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("997")}); ok {
		t.Error("a key message was read as a theme report")
	}
	if _, ok := themeReport(nil); ok {
		t.Error("nil was read as a theme report")
	}
}

// Turning reports on asks for the current theme too, and turning them off
// leaves the pane as the shell expects it.
func TestThemeReportsAreSwitchedOnWithAQueryAndOffAgain(t *testing.T) {
	var b bytes.Buffer
	startThemeReports(&b)
	if b.String() != "\x1b[?2031h\x1b[?996n" {
		t.Errorf("start wrote %q", b.String())
	}
	b.Reset()
	stopThemeReports(&b)
	if b.String() != "\x1b[?2031l" {
		t.Errorf("stop wrote %q", b.String())
	}
}

// A report redraws the sidebar in the reported theme, and the TUI follows it
// the same way: the frame cache does not hand back the old theme's frame.
func TestAThemeReportRedrawsThePanelInTheNewTheme(t *testing.T) {
	withPanelTheme(t, true)
	msgs := parsedMsgs(t, "\x1b[?997;2n\x1b[?997;1n", 2)
	light, dark := msgs[0], msgs[1]

	f := newFleet(t, fleetNow)
	pm := newPaneModel(newFakePanelSource(), newPanelClock(f.now), "", paneOptions{})
	t.Cleanup(pm.panel.close)
	updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 59})
	pm = updated.(paneModel)
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})

	darkCloud, lightCloud := sgrRGB(t, 38, panelPal.cloud.Dark), sgrRGB(t, 38, panelPal.cloud.Light)
	if v := pm.View(); !strings.Contains(v, darkCloud) || strings.Contains(v, lightCloud) {
		t.Fatal("the sidebar does not start in the dark theme")
	}
	updated, cmd := pm.Update(light)
	pm = updated.(paneModel)
	if cmd != nil || lipgloss.HasDarkBackground() {
		t.Fatalf("a light report: command %v, dark %v", cmd != nil, lipgloss.HasDarkBackground())
	}
	if v := pm.View(); !strings.Contains(v, lightCloud) || strings.Contains(v, darkCloud) {
		t.Error("after a light report the sidebar is not drawn in the light theme")
	}
	updated, _ = pm.Update(dark)
	pm = updated.(paneModel)
	if v := pm.View(); !lipgloss.HasDarkBackground() || !strings.Contains(v, darkCloud) {
		t.Error("after a dark report the sidebar is not drawn in the dark theme again")
	}

	m := newTestModel()
	if next, _ := m.Update(light); lipgloss.HasDarkBackground() {
		t.Error("the TUI did not follow a light report")
	} else if _, ok := next.(model); !ok {
		t.Errorf("the TUI returned %T", next)
	}
}
