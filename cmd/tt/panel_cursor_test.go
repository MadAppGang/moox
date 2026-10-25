package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Entering the sidebar puts the cursor on the session in the pane you came
// from: tmux's last active pane in the sidebar's window, else the only other
// pane there.
func TestFocusPutsTheCursorOnThePaneYouCameFrom(t *testing.T) {
	withPanelTheme(t, true)
	pm, f := pickPane(t)
	pm.opts.Pane = "%900"
	pm.View()
	nav := pm.panel.memo.nav
	target := nav[len(nav)-1].s // the last row, far from where the cursor starts
	target.Tmux = &observer.TmuxRef{Pane: "%901"}
	next := *f.snap
	next.Sessions = nil
	for _, s := range f.snap.Sessions {
		if panelSessionKey(s) == panelSessionKey(target) {
			s = target
		}
		next.Sessions = append(next.Sessions, s)
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(&next)})
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
		return "%900 0\n%901 1\n%902 0\n", nil
	})
	updated, cmd := pm.Update(tea.FocusMsg{})
	*pm = updated.(paneModel)
	if cmd == nil {
		t.Fatal("focus did not ask tmux which pane it came from")
	}
	updated, _ = pm.Update(cmd())
	*pm = updated.(paneModel)
	if s, _ := pm.panel.cursorSession(); s.Name != target.Name {
		t.Errorf("focus put the cursor on %s, want %s, the pane it came from", s.Name, target.Name)
	}
	if row := cursorRow(t, pm.View()); !strings.Contains(row, target.Name) {
		t.Errorf("the highlighted row is %q, want %s in view", row, target.Name)
	}

	for _, c := range []struct{ out, want string }{
		{"%900 0\n%901 1\n", "%901"},
		{"%900 1\n%901 0\n", "%901"}, // the sidebar itself is never the answer
		{"%900 0\n%901 0\n%902 0\n", ""},
		{"%900 0\n", ""},
	} {
		if got := pickCameFrom(c.out, "%900"); got != c.want {
			t.Errorf("pickCameFrom(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// cursorFill is the SGR that opens the cursor's fill.
func cursorFill() string {
	return sgrOf(lipgloss.NewStyle().Background(panelPal.view.cursorBg))
}

// cursorRow is the plain text of the line painted on the cursor fill, ""
// when none is.
func cursorRow(t *testing.T, view string) string {
	t.Helper()
	fill := cursorFill()
	if fill == "" {
		t.Fatal("the test profile has no colour, so the cursor fill cannot be seen")
	}
	bar := sgrOf(lipgloss.NewStyle().Foreground(panelPal.view.tabOnFg).Background(panelPal.view.tabOnBg).Bold(true))
	for _, l := range strings.Split(view, "\n") {
		if strings.HasPrefix(l, bar) {
			return strings.Join(plain(l), "")
		}
	}
	return ""
}

// The cursor: a focused sidebar highlights its first row; j and k (and the
// arrows) move the highlight over the rows in drawing order, stopping at
// either end; it is the session that is selected, so it stays on its session
// when the rows reorder; a sidebar at rest draws none.
func TestTheCursorMovesOverTheRows(t *testing.T) {
	withPanelTheme(t, true)
	pm, f := pickPane(t)
	pm.View()
	nav := pm.panel.memo.nav
	if len(nav) < 3 {
		t.Fatalf("the fleet draws %d rows", len(nav))
	}
	name := func(i int) string { return nav[i].s.Name }
	if row := cursorRow(t, pm.View()); !strings.Contains(row, name(0)) {
		t.Fatalf("the cursor starts on %q, want the first row, %s", row, name(0))
	} else if !strings.HasPrefix(row, "❯") {
		t.Errorf("the highlighted row %q does not start with the ❯ pointer", row)
	}
	// Every item on the bar takes the selection's colours: one style opens
	// the line and one reset closes it, no badge keeps its own fill.
	for _, l := range strings.Split(pm.View(), "\n") {
		if strings.Contains(l, "❯") {
			if n := strings.Count(l, "\x1b["); n != 2 {
				t.Errorf("the selection bar carries %d styles, want one across the line: %q", n-1, l)
			}
		}
	}
	press(t, pm, "k")
	if s, _ := pm.panel.cursorSession(); s.Name != name(0) {
		t.Errorf("k on the first row moved the cursor to %s", s.Name)
	}
	press(t, pm, "j")
	press(t, pm, "j")
	if row := cursorRow(t, pm.View()); !strings.Contains(row, name(2)) {
		t.Errorf("two j's highlight %q, want %s", row, name(2))
	}
	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyUp})
	*pm = updated.(paneModel)
	if s, _ := pm.panel.cursorSession(); s.Name != name(1) {
		t.Errorf("↑ moved the cursor to %s, want %s", s.Name, name(1))
	}
	if n := strings.Count(pm.View(), cursorFill()+" "); n == 0 {
		t.Error("the highlighted row's lines are not on the fill")
	}

	// The rows reorder (the first session leaves): the cursor stays on its
	// session.
	kept := name(1)
	next := *f.snap
	next.Sessions = nil
	for _, s := range f.snap.Sessions {
		if s.Name != name(0) {
			next.Sessions = append(next.Sessions, s)
		}
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(&next)})
	if s, _ := pm.panel.cursorSession(); s.Name != kept {
		t.Errorf("after the rows moved the cursor is on %s, want %s", s.Name, kept)
	}

	// At rest no row is highlighted.
	updated, _ = pm.Update(tea.BlurMsg{})
	*pm = updated.(paneModel)
	if row := cursorRow(t, pm.View()); row != "" {
		t.Errorf("a sidebar at rest highlights %q", row)
	}

	// Nor does a drawing of a sidebar (tt panel-render): nothing moves it.
	drawn := sidebar(t, f.now, f.snap, modeProject, 0)
	if row := cursorRow(t, drawn.view()); row != "" {
		t.Errorf("a drawn sidebar highlights %q", row)
	}
}

// enter switches the tmux client to the highlighted session's pane — session,
// window and pane in one switch-client — and does nothing for a session that
// has no pane to go to.
func TestEnterSwitchesTmuxToTheHighlightedSession(t *testing.T) {
	var sent [][]string
	stubSeam(t, &tmuxSet, func(args ...string) error { sent = append(sent, args); return nil })
	pm, f := pickPane(t)
	press(t, pm, "j")
	s, _ := pm.panel.cursorSession()
	// Give the highlighted session a pane, as the observer does for a session
	// in tmux ("name:@3.%42").
	withPane := *f.snap
	withPane.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
	for i := range withPane.Sessions {
		if withPane.Sessions[i].Name == s.Name {
			withPane.Sessions[i].Tmux = &observer.TmuxRef{Raw: "work:@3.%42", Session: "work", Window: "@3", Pane: "%42"}
		}
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(&withPane)})
	s, _ = pm.panel.cursorSession()
	if s.Tmux == nil || s.Tmux.Pane != "%42" {
		t.Fatalf("the cursor left %s when it gained a pane", s.Name)
	}
	updated, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	*pm = updated.(paneModel)
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	cmd()
	if len(sent) != 1 || strings.Join(sent[0], " ") != "switch-client -t "+s.Tmux.Pane {
		t.Errorf("enter sent tmux %q, want switch-client -t %s", sent, s.Tmux.Pane)
	}

	// A session with no pane: nowhere to go.
	next := withPane
	next.Sessions = append([]observer.Session(nil), withPane.Sessions...)
	for i := range next.Sessions {
		if next.Sessions[i].Name == s.Name {
			next.Sessions[i].Tmux = &observer.TmuxRef{Raw: "not a pane"}
		}
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(&next)})
	if _, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		cmd()
	}
	if len(sent) != 1 {
		t.Errorf("enter on a session with no pane sent tmux %q", sent[1:])
	}
}

// A background job has no pane; enter opens it with `claude attach` in a
// popup in its folder, unless attaching could only fail — then the footer
// says why instead (the user, 2026-10-21: a deleted worktree opened to
// "Workspace not trusted").
func TestEnterOpensABackgroundJobInAPopupOrSaysWhyNot(t *testing.T) {
	var sent [][]string
	stubSeam(t, &tmuxSet, func(args ...string) error { sent = append(sent, args); return nil })
	dir := t.TempDir()
	short := "1ce7fef1"
	no := false
	job := observer.Session{
		Kind: observer.KindBackground, Name: "tt-ui-rebuil", CWD: dir, ShortID: &short,
		Status: observer.StatusBlocked,
	}

	msg := openBackground(job)()
	if msg != nil {
		t.Fatalf("a live job with its folder answered %#v", msg)
	}
	want := popupTmux() + " new-session -d -s tt-popup-$$ -c '" + dir + "' 'claude attach 1ce7fef1'" +
		` \; set-option -t tt-popup-$$ @tt_obs_scope off` +
		` \; attach-session -t tt-popup-$$` +
		` \; set-option -t tt-popup-$$ destroy-unattached on`
	if len(sent) != 1 || sent[0][0] != "display-popup" || sent[0][len(sent[0])-1] != want {
		t.Fatalf("tmux got %q, want a display-popup running %q", sent, want)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote(it's) = %s", got)
	}

	gone := filepath.Join(dir, "deleted-worktree")
	bad := "1ce7fef1; rm -rf ~"
	for _, tc := range []struct {
		name string
		edit func(*observer.Session)
		says string
	}{
		{"folder gone", func(s *observer.Session) { s.CWD = gone }, "its folder is gone"},
		{"no folder", func(s *observer.Session) { s.CWD = "" }, "no folder recorded"},
		{"conversation gone", func(s *observer.Session) {
			s.Live, s.LiveReason = &no, observer.LiveReasonJobTranscriptGone
		}, "its conversation is gone"},
		{"ended", func(s *observer.Session) { s.Live, s.LiveReason = &no, observer.LiveReasonJobTerminal }, "has ended"},
		{"odd id", func(s *observer.Session) { s.ShortID = &bad }, "no job id"},
		{"no id", func(s *observer.Session) { s.ShortID = nil }, "no job id"},
	} {
		s := job
		tc.edit(&s)
		got, ok := openBackground(s)().(paneNoteMsg)
		if !ok || !strings.Contains(got.note, tc.says) {
			t.Errorf("%s: footer %q, want it to say %q", tc.name, got.note, tc.says)
		}
	}
	if len(sent) != 1 {
		t.Errorf("a job that cannot be opened still reached tmux: %q", sent[1:])
	}
}

// A click selects a row; a second click on the same row goes there, as enter
// does.
func TestASecondClickOnTheSelectedRowGoesThere(t *testing.T) {
	var sent [][]string
	stubSeam(t, &tmuxSet, func(args ...string) error { sent = append(sent, args); return nil })
	pm, f := pickPane(t)
	pm.View()
	target := pm.panel.memo.nav[2]
	withPane := *f.snap
	withPane.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
	for i := range withPane.Sessions {
		if withPane.Sessions[i].SessionID == target.s.SessionID {
			withPane.Sessions[i].Tmux = &observer.TmuxRef{Raw: "work:@3.%42", Session: "work", Window: "@3", Pane: "%42"}
		}
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(&withPane)})
	pm.View()
	y := -1
	for _, r := range pm.panel.memo.nav {
		if r.s.SessionID == target.s.SessionID {
			y = pm.panel.memo.bodyTop + r.first - pm.panel.memo.bodyOff
		}
	}
	at := func() tea.Cmd {
		updated, cmd := pm.Update(tea.MouseMsg{X: 20, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		*pm = updated.(paneModel)
		return cmd
	}
	if cmd := at(); cmd != nil {
		cmd()
	}
	if len(sent) != 0 {
		t.Fatalf("the first click went somewhere: %q", sent)
	}
	cmd := at()
	if cmd == nil {
		t.Fatal("the second click did nothing")
	}
	cmd()
	if len(sent) != 1 || strings.Join(sent[0], " ") != "switch-client -t %42" {
		t.Errorf("the second click sent tmux %q, want switch-client -t %%42", sent)
	}
}

// space opens the highlighted session in a popup, where enter switches to it:
// a background job with claude attach even when it has a pane, a session in a
// pane as a grouped peek at that pane.
func TestSpaceOpensTheHighlightedSessionInAPopup(t *testing.T) {
	var sent [][]string
	stubSeam(t, &tmuxSet, func(args ...string) error { sent = append(sent, args); return nil })
	space := tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	pm, f := pickPane(t)
	press(t, pm, "j")
	cur, _ := pm.panel.cursorSession()
	dir := t.TempDir()
	short := "b4c9687e"
	withPane := func(bg bool) *observer.Snapshot {
		snap := *f.snap
		snap.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
		for i := range snap.Sessions {
			if snap.Sessions[i].SessionID == cur.SessionID {
				if bg {
					snap.Sessions[i].Kind = observer.KindBackground
					snap.Sessions[i].ShortID = &short
					snap.Sessions[i].CWD = dir
					snap.Sessions[i].Live, snap.Sessions[i].LiveReason = nil, observer.LiveReasonNone
				}
				snap.Sessions[i].Tmux = &observer.TmuxRef{Raw: "work:@3.%42", Session: "work", Window: "@3", Pane: "%42"}
			}
		}
		return &snap
	}
	key := func(msg tea.KeyMsg) {
		t.Helper()
		updated, cmd := pm.Update(msg)
		*pm = updated.(paneModel)
		if cmd == nil {
			t.Fatalf("%q did nothing", msg.String())
		}
		if note, ok := cmd().(paneNoteMsg); ok {
			t.Fatalf("%q answered %q", msg.String(), note.note)
		}
	}
	if space.String() != popupKey {
		t.Fatalf("a space key reads %q, popupKey is %q", space.String(), popupKey)
	}

	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(withPane(true))})
	pm.View()
	key(space)
	if len(sent) != 1 || sent[0][0] != "display-popup" || !strings.Contains(sent[0][len(sent[0])-1], "'claude attach "+short+"'") {
		t.Fatalf("space sent tmux %q, want a display-popup running claude attach %s", sent, short)
	}
	key(tea.KeyMsg{Type: tea.KeyEnter})
	if len(sent) != 2 || strings.Join(sent[1], " ") != "switch-client -t %42" {
		t.Errorf("enter on a job with a pane sent %q, want switch-client -t %%42", sent[1:])
	}

	// A session in a pane: a peek through a grouped session that dies with
	// the popup. The fixture's row above is a job, so move to an interactive
	// one and give it the pane.
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(f.snap)})
	pm.View()
	for range pm.panel.memo.nav {
		if s, _ := pm.panel.cursorSession(); s.Kind != observer.KindBackground {
			break
		}
		pm.panel.moveCursor(1)
	}
	cur, _ = pm.panel.cursorSession()
	if cur.Kind == observer.KindBackground {
		t.Fatal("no interactive row to put the cursor on")
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(withPane(false))})
	pm.View()
	key(space)
	want := popupTmux() + ` new-session -d -s tt-popup-$$ -t %42` +
		` \; select-window -t tt-popup-$$:@3 \; select-pane -t %42` +
		` \; attach-session -t tt-popup-$$` +
		` \; set-option -t tt-popup-$$ destroy-unattached on`
	if len(sent) != 3 || sent[2][0] != "display-popup" || sent[2][len(sent[2])-1] != want {
		t.Fatalf("space on a pane sent %q, want a display-popup running %s", sent[2:], want)
	}

	// No pane and no job: nothing to show, and it says so.
	bare := withPane(false)
	for i := range bare.Sessions {
		if bare.Sessions[i].SessionID == cur.SessionID {
			bare.Sessions[i].Tmux = nil
		}
	}
	pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(bare)})
	pm.View()
	if s, _ := pm.panel.cursorSession(); s.Kind == observer.KindBackground || s.Tmux != nil {
		t.Fatalf("the cursor is on %s, which is not a paneless interactive row", s.Name)
	}
	_, cmd := pm.Update(space)
	if msg, ok := cmd().(paneNoteMsg); !ok || !strings.Contains(msg.note, "no tmux pane") {
		t.Errorf("space with no pane answered %#v", msg)
	}
	if len(sent) != 3 {
		t.Errorf("space with no pane reached tmux: %q", sent[3:])
	}
}

// popupTmux is how every popup shell starts (moox §4.5): `TMUX=` so tmux does
// not refuse to nest, then THIS process's Target — its binary and `-u -S
// <socket>` — so the throwaway session lands on the same server.
func popupTmux() string {
	t := ttTarget()
	return "TMUX= " + shellQuote(t.Bin) + " -u -S " + shellQuote(t.Socket)
}

// TestPopupLandsOnThisServer is the popup half of V7: a popup built while tt
// talks to server a names a's socket with -S, whatever the environment says.
func TestPopupLandsOnThisServer(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	stubSeam(t, &resolveTarget, func() (tmuxtarget.Target, error) {
		return tmuxtarget.Target{Bin: "/opt/tmux bin/tmux", Socket: a}, nil
	})
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,1,0")
	got := popupShell("new-session -d -s "+popupSession, false, "")
	want := "TMUX= '/opt/tmux bin/tmux' -u -S '" + a + "' new-session -d -s " + popupSession
	if !strings.HasPrefix(got, want) {
		t.Fatalf("popup shell = %q\nwant prefix %q", got, want)
	}
}
