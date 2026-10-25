//go:build tmuxe2e

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// TestE2E_Save_RealServer runs Engine.Save against an isolated tmux server
// (scratch HOME/XDG, /bin/sh panes): the one read chain, the contents chain
// and the post chain parse on real tmux, names and titles with `;`, `#`, `.`
// and an empty title come back exactly, and a second save is `unchanged`.
func TestE2E_Save_RealServer(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	env := tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS, "XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC}
	e, err := New(Options{Target: s.T, Env: env, Producer: "moox e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.Paths().Snapshots, s.XS) {
		t.Fatalf("snapshot dir %s is not under the scratch XDG_STATE_HOME", e.Paths().Snapshots)
	}

	sid := strings.TrimSpace(s.Run(t, tmuxtarget.NewSession{Name: tmuxtarget.Literal("api.v2"), Cwd: tmuxtarget.Literal(s.Home),
		PrintFormat: "#{session_id}", Command: []string{"/bin/sh"}}.Args()...))
	wid := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_id}",
		"/bin/sh -c 'echo hello-moox-contents; exec /bin/sh'"))
	s.Run(t, tmuxtarget.RenameWindow(wid, tmuxtarget.Literal("fix; test #(nope)"))...)
	pid := strings.TrimSpace(s.Run(t, "split-window", "-d", "-t", wid, "-P", "-F", "#{pane_id}", "/bin/sh"))
	s.Run(t, tmuxtarget.SelectPaneTitle(pid, tmuxtarget.Literal("x; #{pane_id} ##"))...)
	s.Run(t, "set-option", "-g", "@moox-active", "1")

	// wait until the echo has been drawn, so the capture has something to hold
	first := strings.TrimSpace(s.Run(t, "display", "-p", "-t", wid+".0", "#{pane_id}"))
	if !tmuxtest.Poll(3e9, 5e7, func() bool {
		return strings.Contains(s.Run(t, "capture-pane", "-p", "-t", first), "hello-moox-contents")
	}) {
		t.Fatal("pane never printed")
	}

	res, err := e.Save(context.Background(), SaveRequest{Reason: "e2e", Contents: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.Execs > 5 || !res.ContentsCaptured {
		t.Fatalf("%+v", res)
	}
	last, err := e.Last()
	if err != nil {
		t.Fatal(err)
	}
	snap := last.Snapshot
	livePanes := strings.Fields(s.Run(t, "list-panes", "-a", "-F", "#{pane_id}"))
	if len(snap.Panes) != len(livePanes) || len(snap.Sessions) != 2 {
		t.Fatalf("%d panes (live %d), %d sessions", len(snap.Panes), len(livePanes), len(snap.Sessions))
	}
	var win *snapshot.Window
	for i := range snap.Windows {
		if snap.Windows[i].ID == wid {
			win = &snap.Windows[i]
		}
	}
	if win == nil || win.Name != "fix; test #(nope)" || win.LayoutFormat == "" || win.AutomaticRename != snapshot.AutoRenameOff {
		t.Fatalf("window: %+v", win)
	}
	titled := paneByID(snap, pid)
	if titled == nil || titled.Title != "x; #{pane_id} ##" || titled.Session != "api.v2" || titled.Role != snapshot.RoleShell {
		t.Fatalf("pane: %+v", titled)
	}
	st, err := store.Open(e.Paths(), filepath.Join(s.Home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	entries, err := st.LoadContents(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entries[first]), "hello-moox-contents") || !paneByID(snap, first).Contents {
		t.Fatalf("contents of %s: %q", first, entries[first])
	}
	if got := strings.TrimSpace(s.Run(t, "show-options", "-gqv", "@moox_save_seq")); got != "1" {
		t.Fatalf("@moox_save_seq %q", got)
	}

	// a pane that is gone between the read and the capture stops the chain at
	// its own display; the panes after it are still captured (E5).
	got := e.captureContents(context.Background(), s.T, newNonce(), []string{first, "%9999", pid}, "-")
	if _, ok := got["%9999"]; ok || !strings.Contains(string(got[first]), "hello-moox-contents") {
		t.Fatalf("resume after a gone pane: %q", got)
	}
	if _, ok := got[pid]; !ok {
		t.Fatalf("the pane after the gone one was not captured: %q", got)
	}

	again, err := e.Save(context.Background(), SaveRequest{Reason: "debounce"})
	if err != nil || again.Published || again.Pristine {
		t.Fatalf("second save: %+v %v", again, err)
	}
	// the read, the local automatic-rename read (I-5: a window here inherits
	// the global value), ps
	if again.Execs > 3 {
		t.Fatalf("structural unchanged save made %d execs", again.Execs)
	}
	if fi, err := os.Stat(last.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
}
