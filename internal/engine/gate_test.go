package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
)

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// TestGate_WaitBootstrapIsBounded (B27, R2-M3): the daemon's poll for the
// bootstrap session gives up when the deadline passes with no session (the
// saver then records SKIPPED:no-bootstrap: TestSaver_BootNoBootstrapSkips),
// polls more than once before that, and answers at once when a session exists.
func TestGate_WaitBootstrapIsBounded(t *testing.T) {
	f := newRFake()
	e, _, _ := newRestoreEngine(t, f)
	polls := 0
	f.onCmd = func(c []string) {
		if c[0] == "list-sessions" {
			polls++
		}
	}
	start := time.Now()
	if e.WaitBootstrap(context.Background(), 10*time.Millisecond, 150*time.Millisecond) {
		t.Fatal("no session exists, yet the poll found a bootstrap")
	}
	if took := time.Since(start); took < 150*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the poll returned after %v, want the 150ms deadline", took)
	}
	if polls < 2 {
		t.Fatalf("%d polls before the deadline, want several", polls)
	}

	f.addSession("bootstrap")
	polls, start = 0, time.Now()
	if !e.WaitBootstrap(context.Background(), time.Hour, time.Hour) {
		t.Fatal("the bootstrap session was not found")
	}
	if polls != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("an existing session took %d polls / %v", polls, time.Since(start))
	}
}

// TestGate_NotCold (T1, T2, B47, R16): a server with sessions never
// boot-restores and its pending/ and launch/ are left alone (a reload, the
// sidebar's start button, a daemon re-ensure).
func TestGate_NotCold(t *testing.T) {
	f := newRFake()
	f.addSession("work")
	e, _, _ := newRestoreEngine(t, f)
	publish(t, e, snapOf("a:0:1", "a:1:1"))
	writeFile(t, e.paths.Pending, "7", "x")
	writeFile(t, e.paths.Launch, "7", "x")
	g, err := e.BootGate(context.Background(), 1)
	if err != nil || g.Cold || g.Restore || g.Result != "" {
		t.Fatalf("%+v %v", g, err)
	}
	if !exists(filepath.Join(e.paths.Pending, "7")) || !exists(filepath.Join(e.paths.Launch, "7")) {
		t.Fatal("a non-cold gate purged")
	}
	if len(f.log) != 0 {
		t.Fatalf("a non-cold gate ran tmux: %v", f.log)
	}
}

// TestGate_OneWindowSnapshotSkipped (G4, RR8): a valid Last() with one window
// is not boot-restored; the purges ran anyway (check 3).
func TestGate_OneWindowSnapshotSkipped(t *testing.T) {
	f := newRFake()
	e, _, _ := newRestoreEngine(t, f)
	publish(t, e, snapOf("a:0:2"))
	writeFile(t, e.paths.Pending, "3", "x")
	g, err := e.BootGate(context.Background(), 0)
	if err != nil || g.Restore || g.Result != BootSkippedPrefix+"one-window" || f.opts["@moox-boot-result"] != g.Result {
		t.Fatalf("%+v %v opts=%v", g, err, f.opts)
	}
	if exists(filepath.Join(e.paths.Pending, "3")) || g.Purge.Pending != 1 {
		t.Fatalf("pending not purged on a cold gate: %+v", g)
	}
	// two windows → restore
	f2 := newRFake()
	e2, _, _ := newRestoreEngine(t, f2)
	publish(t, e2, snapOf("a:0:1", "a:1:1"))
	if g, _ := e2.BootGate(context.Background(), 0); !g.Restore || g.Result != "" {
		t.Fatalf("%+v", g)
	}
	// no snapshot → SKIPPED:no-snapshot; a boot result already set → nothing
	f3 := newRFake()
	e3, _, _ := newRestoreEngine(t, f3)
	if g, _ := e3.BootGate(context.Background(), 0); g.Restore || g.Result != BootSkippedPrefix+"no-snapshot" {
		t.Fatalf("%+v", g)
	}
	f2.opts["@moox-boot-result"] = "PASS"
	if g, _ := e2.BootGate(context.Background(), 0); g.Restore {
		t.Fatalf("a set @moox-boot-result still restored: %+v", g)
	}
}

// TestGate_Handover (G3, U3, I7): MOOX_NO_AUTO_RESTORE (or the legacy
// TT_NO_AUTO_RESTORE) in the global env skips the boot restore once:
// both are unset and HANDOVER goes to @moox-boot-result, never to
// @moox-restore-result (PR-M3). The purges still run.
func TestGate_Handover(t *testing.T) {
	for _, v := range []string{"MOOX_NO_AUTO_RESTORE=1", "TT_NO_AUTO_RESTORE=1"} {
		f := newRFake()
		f.globEnv = []string{"HOME=/x", v}
		e, _, _ := newRestoreEngine(t, f)
		publish(t, e, snapOf("a:0:1", "a:1:1"))
		writeFile(t, e.paths.Pending, "12", "x")
		g, err := e.BootGate(context.Background(), 0)
		if err != nil || g.Restore || g.Result != BootHandover || f.opts["@moox-boot-result"] != BootHandover {
			t.Fatalf("%s: %+v %v", v, g, err)
		}
		if _, set := f.opts["@moox-restore-result"]; set {
			t.Fatal("HANDOVER reached @moox-restore-result")
		}
		unset := 0
		for _, c := range f.log {
			if c[0] == "set-environment" && c[1] == "-gu" {
				unset++
			}
		}
		if unset != 2 || g.Purge.Pending != 1 {
			t.Fatalf("%s: %d unsets, purge %+v", v, unset, g.Purge)
		}
	}
}

// TestGate_RecoversLeftoverRestoringFrom (§6.2 crash safety): the next gate
// turns a dead restore's restoring-from into a pin.
func TestGate_RecoversLeftoverRestoringFrom(t *testing.T) {
	f := newRFake()
	e, _, _ := newRestoreEngine(t, f)
	name := publish(t, e, snapOf("a:0:1", "a:1:1"))
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st.TryLock(e.record())
	if err := st.BeginAttempt(l, name); err != nil {
		t.Fatal(err)
	}
	l.Release()
	g, _ := e.BootGate(context.Background(), 0)
	pins, _ := st.Pins()
	_ = st.Close()
	if g.Recovered != name || len(pins) != 1 || pins[0] != name {
		t.Fatalf("recovered %q pins %v", g.Recovered, pins)
	}
}

// TestPurge_KeepsRecordingsNewerThanStart (B28, R15): launch/<N> older than
// the server's start goes, a newer one stays; pending/ goes whole (every
// all-digit name); other names are never touched.
func TestPurge_KeepsRecordingsNewerThanStart(t *testing.T) {
	f := newRFake()
	e, _, _ := newRestoreEngine(t, f)
	start := time.Now().Add(-time.Hour)
	writeFile(t, e.paths.Launch, "1", "old")
	writeFile(t, e.paths.Launch, "2", "new")
	writeFile(t, e.paths.Launch, "notes", "x")
	if err := os.Chtimes(filepath.Join(e.paths.Launch, "1"), start.Add(-time.Minute), start.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.paths.Pending, "5", "x")
	writeFile(t, e.paths.Pending, "keep.me", "x")
	r, err := e.Purge(context.Background(), PurgeRequest{Pending: true, Launch: true, StartTime: start.Unix()})
	if err != nil || r.Launch != 1 || r.Pending != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	for p, want := range map[string]bool{"launch/1": false, "launch/2": true, "launch/notes": true, "pending/5": false, "pending/keep.me": true} {
		dir := e.paths.Launch
		if filepath.Dir(p) == "pending" {
			dir = e.paths.Pending
		}
		if got := exists(filepath.Join(dir, filepath.Base(p))); got != want {
			t.Errorf("%s exists=%v, want %v", p, got, want)
		}
	}
}

// TestPurge_UnknownStartKeepsRecordings: with no start_time nothing in
// launch/ is deleted (the opposite of the old blanket delete).
func TestPurge_UnknownStartKeepsRecordings(t *testing.T) {
	f := newRFake()
	e, _, _ := newRestoreEngine(t, f)
	writeFile(t, e.paths.Launch, "1", "old")
	if err := os.Chtimes(filepath.Join(e.paths.Launch, "1"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	r, err := e.Purge(context.Background(), PurgeRequest{Pending: true, Launch: true})
	if err != nil || r.Launch != 0 || !exists(filepath.Join(e.paths.Launch, "1")) {
		t.Fatalf("%+v %v", r, err)
	}
	// a missing directory is not an error
	if _, err := e.Purge(context.Background(), PurgeRequest{Pending: true, Launch: true, StartTime: 5}); err != nil {
		t.Fatal(err)
	}
}
