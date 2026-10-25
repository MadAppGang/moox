package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	msnap "github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// TestHumanAge pins the age wording, including the one case that isn't just
// arithmetic: a snapshot stamped in the future (clock skew, or a file copied off
// another machine) must not render as a negative duration.
func TestHumanAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Hour, "just now"}, // future timestamp, not "-1h ago"
		{0, "just now"},
		{30 * time.Second, "just now"},
		{90 * time.Second, "1m ago"},
		{59 * time.Minute, "59m ago"},
		{2 * time.Hour, "2h ago"},
		{23 * time.Hour, "23h ago"},
		{25 * time.Hour, "1d ago"},
		{9 * 24 * time.Hour, "9d ago"},
	}
	for _, c := range cases {
		if got := humanAge(c.in); got != c.want {
			t.Errorf("humanAge(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSnapshotSummary checks the singular/plural switch, because "1 sessions,
// 1 windows" is the kind of thing that ships and then reads as broken forever.
func TestSnapshotSummary(t *testing.T) {
	now := time.Unix(1_780_007_200, 0)
	one := snapshot{sessions: []string{"solo"}, windows: 1, savedAt: now.Add(-2 * time.Hour)}
	if got, want := one.summary(now), "1 session, 1 window, saved 2h ago"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	many := snapshot{sessions: []string{"a", "b"}, windows: 7, savedAt: now.Add(-2 * time.Hour)}
	if got, want := many.summary(now), "2 sessions, 7 windows, saved 2h ago"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	// contents is what the restoring view shows: the age is meaningless once the
	// restore is already under way.
	if got, want := many.contents(), "2 sessions, 7 windows"; got != want {
		t.Errorf("contents = %q, want %q", got, want)
	}
}

func TestCanRestoreRequiresBothHalves(t *testing.T) {
	empty := newRestorableModel(t)
	if !empty.canRestore() {
		t.Error("empty list + snapshot should be restorable")
	}

	populated := empty
	populated.list.SetItems(toItems([]session{{name: "magus", windows: 3}}))
	if populated.canRestore() {
		t.Error("a populated list must never offer the restore")
	}

	noSnapshot := empty
	noSnapshot.snap = snapshot{}
	if noSnapshot.canRestore() {
		t.Error("no snapshot means nothing to offer")
	}
}

// TestRestoreKeysStartTheRestore checks both affordances, and that they only
// fire from the empty state. enter is the primary one — with a snapshot on disk
// the empty state is an offer, not a dead end.
func TestRestoreKeysStartTheRestore(t *testing.T) {
	for _, k := range []string{"ctrl+s", "enter"} {
		t.Run(k, func(t *testing.T) {
			called := false
			stubSeam(t, &restoreSnapshot, func() error { called = true; return nil })

			m := newRestorableModel(t)
			m2, cmd := applyMsg(m, keyMsg(k))
			if !m2.restoring {
				t.Fatalf("%s should put the model into the restoring state", k)
			}
			if cmd == nil {
				t.Fatalf("%s should return a command", k)
			}
			m2 = pump(t, m2, cmd)
			if !called {
				t.Errorf("%s should have run the restore", k)
			}
			if m2.restoring {
				t.Error("the model should leave the restoring state once the restore returns")
			}
		})
	}
}

// TestRestoreKeyInertWithSessions pins the other side of the safety property at
// the keystroke level: ^s and enter must not restore when sessions exist. enter
// keeps its real job there — attaching.
func TestRestoreKeyInertWithSessions(t *testing.T) {
	stubSeam(t, &restoreSnapshot, func() error {
		t.Error("restore must not run while sessions exist")
		return nil
	})
	stubSeam(t, &loadSnapshot, testSnapshot)

	m := newTestModel(session{name: "magus", windows: 3})
	m.snap = testSnapshot()

	m2, _ := applyMsg(m, keyMsg("ctrl+s"))
	if m2.restoring {
		t.Error("^s must be inert while sessions exist")
	}

	m3, _ := applyMsg(m, keyMsg("enter"))
	if m3.restoring {
		t.Error("enter must not restore while sessions exist")
	}
	if m3.selected != "magus" {
		t.Errorf("enter should still attach, got selected=%q", m3.selected)
	}
}

// TestRestoreFailureSurfaces verifies a failed restore reports the shell's own
// message instead of leaving the user on a silently unchanged empty screen.
func TestRestoreFailureSurfaces(t *testing.T) {
	stubSeam(t, &restoreSnapshot, func() error { return stubErr("no server running") })

	m := newRestorableModel(t)
	m2, cmd := applyMsg(m, keyMsg("ctrl+s"))
	m2 = pump(t, m2, cmd)

	if m2.restoring {
		t.Error("a failed restore must clear the restoring state")
	}
	if !strings.Contains(m2.status, "no server running") {
		t.Fatalf("status should carry the restore error, got %q", m2.status)
	}
	if !strings.Contains(m2.View(), "no server running") {
		t.Fatalf("the failure should be visible, got:\n%s", m2.View())
	}
}

// TestRestoreRefreshesTheList proves the list shows what tmux actually has after
// a restore, not what the snapshot promised. The two deliberately disagree here.
func TestRestoreRefreshesTheList(t *testing.T) {
	stubSeam(t, &restoreSnapshot, func() error { return nil })
	// The snapshot claims three sessions; tmux comes back with two. The list must
	// follow tmux.
	stubSeam(t, &listSessions, func() []session {
		return []session{{name: "magus", windows: 3}, {name: "mnemex", windows: 1}}
	})

	m := newRestorableModel(t)
	m2, cmd := applyMsg(m, keyMsg("ctrl+s"))
	m2 = pump(t, m2, cmd)

	if got := len(m2.list.Items()); got != 2 {
		t.Fatalf("list should hold the 2 sessions tmux reported, got %d", got)
	}
	if m2.canRestore() {
		t.Error("the offer must be gone once sessions exist")
	}
}

// TestKeysInertWhileRestoring guards the window in which the restore is building
// the server: a second ^s would start a concurrent restore, and enter would
// attach to a half-built session. ctrl+c is the deliberate exception.
func TestKeysInertWhileRestoring(t *testing.T) {
	stubSeam(t, &restoreSnapshot, func() error {
		t.Error("no restore should be started by a key pressed mid-restore")
		return nil
	})

	m := newRestorableModel(t)
	m.restoring = true

	for _, k := range []string{"ctrl+s", "enter", "ctrl+t", "ctrl+x", "esc", "down"} {
		m2, _ := applyMsg(m, keyMsg(k))
		if m2.mode != modeList || m2.form != nil {
			t.Errorf("%s should be swallowed mid-restore, but opened a form", k)
		}
		if m2.selected != "" {
			t.Errorf("%s should not select a session mid-restore", k)
		}
	}

	// ctrl+c still lets go of the TUI — the restore is a separate process and
	// finishes on its own.
	_, cmd := applyMsg(m, keyMsg("ctrl+c"))
	if _, quit := pumpTracked(t, m, cmd); !quit {
		t.Error("ctrl+c should still quit while restoring")
	}
}

// TestRestoringViewShowsProgress checks the screen says something while a
// multi-second shell script runs, rather than looking frozen.
func TestRestoringViewShowsProgress(t *testing.T) {
	m := newRestorableModel(t)
	m.restoring = true

	view := m.View()
	if !strings.Contains(view, "restoring") {
		t.Fatalf("the restoring view should say so, got:\n%s", view)
	}
	if !strings.Contains(view, "3 sessions") {
		t.Fatalf("the restoring view should name what is coming back, got:\n%s", view)
	}

	// The spinner must keep ticking while restoring, and stop once it isn't —
	// otherwise the tick reschedules itself forever after the restore returns.
	if _, cmd := applyMsg(m, spinner.TickMsg{}); cmd == nil {
		t.Error("the spinner should keep ticking while restoring")
	}
	idle := m
	idle.restoring = false
	if _, cmd := applyMsg(idle, spinner.TickMsg{}); cmd != nil {
		t.Error("the spinner must stop ticking once the restore is done")
	}
}

// TestEmptyStateOffersRestore is the user-visible half: with a snapshot on disk
// the empty state stops being a dead end.
func TestEmptyStateOffersRestore(t *testing.T) {
	m := newRestorableModel(t)
	view := m.View()

	for _, want := range []string{"no tmux sessions", "3 sessions", "9 windows", "^s"} {
		if !strings.Contains(view, want) {
			t.Errorf("empty state should mention %q, got:\n%s", want, view)
		}
	}
}

// TestEmptyStateWithoutSnapshotIsUnchanged makes sure a machine that has never
// saved still gets the plain empty state, with no offer it cannot honour.
func TestEmptyStateWithoutSnapshotIsUnchanged(t *testing.T) {
	stubSeam(t, &loadSnapshot, func() snapshot { return snapshot{} })
	m := newModel()
	m, _ = applyMsg(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	view := m.View()
	if !strings.Contains(view, "no tmux sessions") {
		t.Fatalf("empty state should still render, got:\n%s", view)
	}
	if strings.Contains(view, "last snapshot") || strings.Contains(view, "^s") {
		t.Fatalf("nothing to restore must not be advertised, got:\n%s", view)
	}
}

// TestReadMooxSnapshot reads moox-last of the server's own key (§4.2) through
// the engine, counts sessions and windows, and takes the age from
// created_at.
func TestReadMooxSnapshot(t *testing.T) {
	root := t.TempDir()
	env := tmuxtarget.MapEnv{"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": filepath.Join(root, "xs"),
		"XDG_DATA_HOME": filepath.Join(root, "xd"), "XDG_CONFIG_HOME": filepath.Join(root, "xc")}
	target := tmuxtarget.Target{Bin: "/bin/false", Socket: filepath.Join(root, "sock")}
	e, err := engine.New(engine.Options{Target: target, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.Paths().Snapshots
	if snap := readMooxSnapshot(target, env); !snap.empty() {
		t.Fatalf("a machine that never saved offers %+v", snap)
	}

	s := &msnap.Snapshot{Format: msnap.FormatName, Version: 1, Source: msnap.SourceSave, CreatedAt: "2026-10-01T10:00:00Z",
		Client: &msnap.Client{Session: "magus"}}
	for _, name := range []string{"magus", "mnemex"} {
		s.Sessions = append(s.Sessions, msnap.Session{Name: name, LastWindow: -1})
	}
	add := func(sess string, w int, id string) {
		s.Panes = append(s.Panes, msnap.Pane{Key: msnap.PaneKey(sess, w, 0), Session: sess, Window: w, SavedID: id,
			Cwd: "/", Role: msnap.RoleShell, Active: true})
		s.Windows = append(s.Windows, msnap.Window{Session: sess, Index: w, Name: "w", Layout: layoutV1(len(s.Panes)),
			LayoutFormat: msnap.LayoutV1, AutomaticRename: msnap.AutoRenameUnset})
	}
	add("magus", 0, "%1")
	add("magus", 1, "%2")
	add("mnemex", 0, "%3")
	if err := msnap.Validate(s); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	b, err := msnap.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "moox-20261001T100000.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// A pointer at a file that is not there: the read heals to the newest
	// valid snapshot rather than offering nothing (and writes nothing).
	if err := os.WriteFile(filepath.Join(dir, "moox-last"), []byte("moox-20261002T100000.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := readMooxSnapshot(target, env)
	if got := strings.Join(snap.sessions, ","); got != "magus,mnemex" || snap.windows != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !snap.savedAt.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("savedAt = %v", snap.savedAt)
	}
	if snap.path != filepath.Join(dir, "moox-20261001T100000.json") {
		t.Errorf("path = %q", snap.path)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "moox-last")); string(b) != "moox-20261002T100000.json\n" {
		t.Errorf("reading the offer rewrote moox-last: %q", b)
	}
}

// TestRequestRestoreNeedsASnapshot: with nothing to restore the request is
// never sent (and no server is started).
func TestRequestRestoreNeedsASnapshot(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "sock")
	err := requestRestore(context.Background(), tmuxtarget.Target{Bin: "/bin/false", Socket: sock}, nil, snapshot{})
	if err == nil {
		t.Fatal("an empty snapshot was requested")
	}
	if _, serr := os.Lstat(sock); serr == nil {
		t.Fatal("a server was started for nothing")
	}
}

// layoutV1 is tmux's v1 layout string for one 80x24 pane with id n.
func layoutV1(n int) string {
	body := fmt.Sprintf("80x24,0,0,%d", n)
	csum := 0
	for _, c := range []byte(body) {
		csum = (csum >> 1) + ((csum & 1) << 15)
		csum += int(c)
		csum &= 0xffff
	}
	return fmt.Sprintf("%04x,%s", csum, body)
}
