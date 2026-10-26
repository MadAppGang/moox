//go:build tmuxe2e

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// ---------------------------------------------------------------------------
// harness for restore E2E tests on isolated servers (tmuxtest.Start, /bin/sh
// panes unless a test sets its own default-command)
// ---------------------------------------------------------------------------

func scratchEnv(s *tmuxtest.Server) tmuxtarget.MapEnv {
	return tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS, "XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC}
}

// e2eEngine is an engine on s with the scratch roots and a fast LAUNCH probe.
func e2eEngine(t *testing.T, s *tmuxtest.Server) *Engine {
	t.Helper()
	e, err := New(Options{Target: s.T, Env: scratchEnv(s), Producer: "moox e2e", Log: testLog{t}})
	if err != nil {
		t.Fatal(err)
	}
	e.timing = &launchTiming{first: 300 * time.Millisecond, every: 300 * time.Millisecond,
		settle: 500 * time.Millisecond, deadline: 20 * time.Second}
	return e
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newSession creates a detached session and returns its $id.
func newSession(t *testing.T, s *tmuxtest.Server, name, cwd string, cmd ...string) string {
	t.Helper()
	return strings.TrimSpace(s.Run(t, tmuxtarget.Chain(tmuxtarget.NewSession{Name: tmuxtarget.Literal(name), Cwd: tmuxtarget.Literal(cwd),
		Width: 160, Height: 40, PrintFormat: "#{session_id}", Command: cmd}.Args())...))
}

// mkdir makes a scratch dir under s.Home.
func mkdir(t *testing.T, s *tmuxtest.Server, name string) string {
	t.Helper()
	p := filepath.Join(s.Home, name)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// saveNow publishes the live server as a snapshot (with contents).
func saveNow(t *testing.T, e *Engine, s *tmuxtest.Server) *snapshot.Snapshot {
	t.Helper()
	s.Run(t, "set-option", "-g", "@moox-active", "1")
	res, err := e.Save(context.Background(), SaveRequest{Reason: "e2e", Contents: true, Force: true})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !res.Published {
		t.Fatalf("save published nothing: %+v", res)
	}
	last, err := e.Last()
	if err != nil {
		t.Fatal(err)
	}
	return last.Snapshot
}

// resetTo leaves the server with ONE fresh session named name (the
// bootstrap of a manual restore): it is created first, then every other
// session is killed, so exit-empty never ends the server.
func resetTo(t *testing.T, s *tmuxtest.Server, name string) string {
	t.Helper()
	tmp := "moox-reset-" + strings.ReplaceAll(t.Name(), "/", "-")
	boot := newSession(t, s, tmp, s.Home)
	var kill [][]string
	for _, id := range strings.Fields(s.Run(t, "list-sessions", "-F", "#{session_id}")) {
		if id != boot {
			kill = append(kill, []string{"kill-session", "-t", id})
		}
	}
	if len(kill) > 0 {
		s.Run(t, tmuxtarget.Chain(kill...)...)
	}
	s.Run(t, tmuxtarget.RenameSession(boot, tmuxtarget.Literal(name))...)
	return boot
}

// restoreNow restores Last() and waits for the LAUNCH verdict.
func restoreNow(t *testing.T, e *Engine, stub bool) (RestoreResult, LaunchVerdict) {
	t.Helper()
	res, err := e.Restore(context.Background(), RestoreRequest{Kind: KindRestore, Stub: stub})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	select {
	case lv := <-res.Launch:
		return res, lv
	case <-time.After(60 * time.Second):
		t.Fatal("no LAUNCH verdict within 60 s")
	}
	return res, LaunchVerdict{}
}

type livePane struct {
	key, id, cwd, title string
}

func listPanes(t *testing.T, s *tmuxtest.Server) []livePane {
	t.Helper()
	out := s.Run(t, "list-panes", "-a", "-F", "#S:#I.#P\x1f#{pane_id}\x1f#{pane_current_path}\x1f#{pane_title}")
	var ps []livePane
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) == 4 {
			ps = append(ps, livePane{f[0], f[1], f[2], f[3]})
		}
	}
	return ps
}

func sessionNames(t *testing.T, s *tmuxtest.Server) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(s.Run(t, "list-sessions", "-F", "#{session_name}"), "\n"), "\n")
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestE2E_Restore_BootstrapUntouchedAndDropped is Q2's sequence (T5, U3):
// the bootstrap `main` with its own window → renamed moox-boot-<pid> → a new
// `main` with 2 windows and 3 panes in row order, none of them the
// bootstrap's → the bootstrap dropped → only the restored `main` is left.
func TestE2E_Restore_BootstrapUntouchedAndDropped(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSession("main"), tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	a, b, c := mkdir(t, s, "a"), mkdir(t, s, "b"), mkdir(t, s, "c")
	first := strings.TrimSpace(s.Run(t, "list-windows", "-t", "main", "-F", "#{window_id}"))
	s.Run(t, "respawn-pane", "-k", "-c", a, "-t", first, tmuxtest.PaneShell)
	w2 := strings.TrimSpace(s.Run(t, tmuxtarget.NewWindow{Target: "main:", Cwd: tmuxtarget.Literal(b),
		PrintFormat: "#{window_id}"}.Args()...))
	s.Run(t, tmuxtarget.SplitWindow{Target: w2, Cwd: tmuxtarget.Literal(c)}.Args()...)
	saved := saveNow(t, e, s)
	if len(saved.Panes) != 3 || len(saved.Windows) != 2 {
		t.Fatalf("saved %d panes, %d windows", len(saved.Panes), len(saved.Windows))
	}

	boot := resetTo(t, s, "main")
	bootPane := strings.TrimSpace(s.Run(t, "list-panes", "-t", boot, "-F", "#{pane_id}"))
	res, lv := restoreNow(t, e, true)

	if got := sessionNames(t, s); len(got) != 1 || got[0] != "main" {
		t.Fatalf("sessions after restore: %q (want only the restored main)", got)
	}
	if _, err := s.RunErr("has-session", "-t", boot); err == nil {
		t.Fatalf("bootstrap %s still exists", boot)
	}
	ps := listPanes(t, s)
	if len(ps) != 3 {
		t.Fatalf("panes: %+v", ps)
	}
	for _, p := range ps {
		if p.id == bootPane {
			t.Fatalf("the bootstrap pane %s was used by the restore", bootPane)
		}
	}
	want := []string{"main:0.0 " + a, "main:1.0 " + b, "main:1.1 " + c}
	for i, p := range ps {
		real, _ := filepath.EvalSymlinks(strings.Fields(want[i])[1])
		if got := p.key + " " + p.cwd; got != want[i] && got != strings.Fields(want[i])[0]+" "+real {
			t.Errorf("pane %d: %q, want %q", i, got, want[i])
		}
	}
	if res.Boot.Result != verdict.Nothing || res.Boot.Misses != 0 || lv.Result != verdict.Pass {
		t.Fatalf("verdicts: BOOT %+v LAUNCH %+v", res.Boot, lv)
	}
	if s.Opt(t, "@moox-restoring") != "" {
		t.Fatalf("@moox-restoring still set after the verdict")
	}
}
