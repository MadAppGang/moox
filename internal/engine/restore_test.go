package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

func restoreFake(t *testing.T, e *Engine, req RestoreRequest) (RestoreResult, LaunchVerdict, error) {
	t.Helper()
	res, err := e.Restore(context.Background(), req)
	if err != nil {
		return res, LaunchVerdict{}, err
	}
	select {
	case lv := <-res.Launch:
		return res, lv, nil
	case <-time.After(10 * time.Second):
		t.Fatal("no LAUNCH verdict")
	}
	return res, LaunchVerdict{}, nil
}

// TestBootVerdict_SessionCreateFailureIsMiss: exec A fails (twice) for the
// 2nd of 3 sessions, all of them shell rows: BOOT is INCOMPLETE — not
// NOTHING — with exactly that session's rows missed, and the restore goes on
// with the 3rd.
func TestBootVerdict_SessionCreateFailureIsMiss(t *testing.T) {
	f := newRFake()
	f.addSession("bootstrap")
	e, _, _ := newRestoreEngine(t, f)
	publish(t, e, snapOf("one:0:1", "two:0:2", "three:0:1"))
	f.fail = func(c []string) bool { return c[0] == "new-session" && rflag(c, "-s") == "two" }
	res, lv, err := restoreFake(t, e, RestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Boot.Result != verdict.Incomplete || res.Boot.Misses != 2 ||
		strings.Join(res.Boot.MissKeys, ",") != "two:0.0,two:0.1" {
		t.Fatalf("BOOT %+v", res.Boot)
	}
	if _, ok := res.Created["three:0.0"]; !ok {
		t.Fatalf("the 3rd session was not created after the 2nd failed: %v", res.Created)
	}
	n := 0
	for _, c := range f.log {
		if c[0] == "new-session" && rflag(c, "-s") == "two" {
			n++
		}
	}
	if n != 2 || lv.Result != verdict.Pass {
		t.Fatalf("exec A for `two` ran %d times (want 2: one retry); LAUNCH %+v", n, lv)
	}
}

// TestBootVerdict_UnattemptedSessionsAreMisses: a restore that stops (the
// context ends, the server goes) counts every row of every session it never
// attempted.
func TestBootVerdict_UnattemptedSessionsAreMisses(t *testing.T) {
	f := newRFake()
	f.addSession("bootstrap")
	e, _, _ := newRestoreEngine(t, f)
	publish(t, e, snapOf("one:0:1", "two:0:2", "three:0:1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onCmd = func(c []string) {
		if c[0] == "set-environment" && len(c) > 2 && c[1] == "-t" {
			cancel() // right after the first session's exec A
		}
	}
	res, err := e.Restore(ctx, RestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-res.Launch
	if res.Boot.Result != verdict.Incomplete || res.Boot.Misses != 4 {
		t.Fatalf("BOOT %+v (want the 3 rows never attempted + one) ", res.Boot)
	}
}

// TestRestore_NeverIntoPopulatedServer (B47, R28): a populated server is
// never restored into — not by ManualPreconditions, not by Restore itself.
func TestRestore_NeverIntoPopulatedServer(t *testing.T) {
	f := newRFake()
	f.addSession("work")
	f.addSession("more")
	e, _, _ := newRestoreEngine(t, f)
	publish(t, e, snapOf("a:0:1", "a:1:1"))
	f.opts["@moox-active"] = "1"
	if err := e.ManualPreconditions(context.Background()); !errors.Is(err, ErrPopulated) {
		t.Fatalf("preconditions on 2 sessions: %v", err)
	}
	if _, err := e.Restore(context.Background(), RestoreRequest{}); !errors.Is(err, ErrPopulated) {
		t.Fatalf("restore into 2 sessions: %v", err)
	}
	for _, c := range f.log {
		if c[0] == "new-session" || c[0] == "new-window" || c[0] == "split-window" {
			t.Fatalf("a populated server got %v", c)
		}
	}
	if f.opts["@moox-restoring"] != "" {
		t.Fatalf("@moox-restoring left set: %q", f.opts["@moox-restoring"])
	}

	// one session, one pane, but its shell has a child: refused
	g := newRFake()
	g.addSession("0")
	e2, _, _ := newRestoreEngine(t, g)
	g.opts["@moox-active"] = "1"
	pid := g.panes[0].pid
	e2.procs = rprocs{table: psLine(pid, 1, "zsh") + psLine(pid+1, pid, "vim notes"), alive: map[int]bool{}}
	if err := e2.ManualPreconditions(context.Background()); !errors.Is(err, ErrPopulated) {
		t.Fatalf("a shell with a child: %v", err)
	}
	e2.procs = rprocs{table: psLine(pid, 1, "zsh"), alive: map[int]bool{}}
	if err := e2.ManualPreconditions(context.Background()); err != nil {
		t.Fatalf("a fresh bootstrap: %v", err)
	}
	g.opts["@moox-active"] = "0"
	if err := e2.ManualPreconditions(context.Background()); !errors.Is(err, ErrNotActive) {
		t.Fatalf("not active: %v", err)
	}
	g.opts["@moox-active"] = "1"
	g.opts["@moox-restoring"] = "restore:77:1"
	e2.procs = rprocs{table: psLine(pid, 1, "zsh"), alive: map[int]bool{77: true}}
	if err := e2.ManualPreconditions(context.Background()); !errors.Is(err, ErrRestoring) {
		t.Fatalf("a running restore: %v", err)
	}
}

// TestRestore_SingleBuiltOnSessionIsPopulated (G1-H2, B47): ONE session the
// user has already built on (two windows, or one window split in two) is a
// populated server for Restore itself, under save.lock — it is not renamed
// moox-boot-<pid> (a renamed session leaves every later save) and nothing
// is created in it.
func TestRestore_SingleBuiltOnSessionIsPopulated(t *testing.T) {
	for _, shape := range []string{"two-windows", "split-pane"} {
		f := newRFake()
		s := f.addSession("work") // the snapshot has a session of this name
		w := f.wins[0]
		if shape == "two-windows" {
			w2 := &rWin{id: f.id("@"), sess: s.id, idx: 1}
			f.wins = append(f.wins, w2)
			w = w2
		}
		f.panes = append(f.panes, &rPane{id: f.id("%"), win: w.id, cmd: "zsh", pid: 9500})
		e, _, _ := newRestoreEngine(t, f)
		publish(t, e, snapOf("work:0:1", "work:1:1"))
		for _, kind := range []string{KindRestore, KindBoot} {
			if _, err := e.Restore(context.Background(), RestoreRequest{Kind: kind}); !errors.Is(err, ErrPopulated) {
				t.Fatalf("%s/%s: %v, want ErrPopulated", shape, kind, err)
			}
		}
		for _, c := range f.log {
			switch c[0] {
			case "rename-session", "new-session", "new-window", "split-window", "kill-session":
				t.Fatalf("%s: the user's session got %v", shape, c)
			}
		}
		if f.sessions[0].name != "work" {
			t.Fatalf("%s: the user's session was renamed %q", shape, f.sessions[0].name)
		}
	}
}

// TestRestore_FailedBootRestorePinsSource (G1-H3, A16): a boot restore that
// is refused before it creates anything (here B47: login tabs opened two
// sessions first) pins its source and sets @moox-warning, so the saves that
// resume afterwards cannot supersede the workspace without trace.
func TestRestore_FailedBootRestorePinsSource(t *testing.T) {
	f := newRFake()
	f.addSession("tab1")
	f.addSession("tab2")
	e, _, _ := newRestoreEngine(t, f)
	name := publish(t, e, snapOf("a:0:1", "a:1:1"))
	if _, err := e.Restore(context.Background(), RestoreRequest{Kind: KindBoot}); !errors.Is(err, ErrPopulated) {
		t.Fatalf("boot restore into 2 sessions: %v", err)
	}
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pins, err := st.Pins()
	if err != nil || len(pins) != 1 || pins[0] != name {
		t.Fatalf("pins = %v (%v), want [%s]", pins, err, name)
	}
	if rf, ok := st.RestoringFrom(); ok {
		t.Fatalf("restoring-from %q left behind by a restore that never began", rf)
	}
	if w := f.opts["@moox-warning"]; !strings.Contains(w, "1 pinned") {
		t.Fatalf("@moox-warning = %q", w)
	}
	// a manual restore refused the same way reports to its caller and pins
	// nothing
	g := newRFake()
	g.addSession("x")
	g.addSession("y")
	e2, _, _ := newRestoreEngine(t, g)
	publish(t, e2, snapOf("a:0:1", "a:1:1"))
	if _, err := e2.Restore(context.Background(), RestoreRequest{}); !errors.Is(err, ErrPopulated) {
		t.Fatal(err)
	}
	st2, _ := store.Open(e2.paths, e2.claudeRoot)
	defer func() { _ = st2.Close() }()
	if pins, _ := st2.Pins(); len(pins) != 0 {
		t.Fatalf("a manual refusal pinned %v", pins)
	}
}

// TestRestore_ManualRechecksPaneUnderLock (G1-2-H1): ManualPreconditions
// checks the one pane's foreground command before the lock wait (up to
// 120 s). A job started during the wait keeps the one-window, one-pane
// shape, so the shape-only check under the lock let the restore rename and
// later kill that session. A manual restore now checks again, under the
// lock, that the pane is still an idle shell. A boot restore keeps its rc
// exception: the login shell may still be running its rc files.
func TestRestore_ManualRechecksPaneUnderLock(t *testing.T) {
	for _, c := range []struct {
		name  string
		cmd   string
		child bool
	}{
		{"an editor in the foreground", "vim", false},
		{"a child of the shell", "zsh", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRFake()
			f.addSession("work") // the snapshot has a session of this name
			p := f.panes[0]
			e, _, _ := newRestoreEngine(t, f)
			f.opts["@moox-active"] = "1"
			publish(t, e, snapOf("work:0:1", "work:1:1"))
			if err := e.ManualPreconditions(context.Background()); err != nil {
				t.Fatalf("setup: the fresh shell fails the precheck: %v", err)
			}
			// The job starts during the lock wait, after the precheck.
			e.onStep = func(s string) {
				if s == "lock" {
					p.cmd = c.cmd
					if c.child {
						e.procs = rprocs{alive: map[int]bool{}, table: psLine(p.pid+1, p.pid, "vim notes.txt")}
					}
				}
			}
			if _, err := e.Restore(context.Background(), RestoreRequest{Kind: KindRestore}); !errors.Is(err, ErrPopulated) {
				t.Fatalf("manual restore over a busy pane: %v, want ErrPopulated", err)
			}
			for _, cmd := range f.log {
				switch cmd[0] {
				case "rename-session", "new-session", "new-window", "split-window", "kill-session":
					t.Fatalf("the busy pane's session got %v", cmd)
				}
			}
			if f.sessions[0].name != "work" {
				t.Fatalf("the busy session was renamed %q", f.sessions[0].name)
			}
		})
	}

	// The boot restore does not run the shell check: a login shell busy in
	// its rc files is still the bootstrap.
	f := newRFake()
	f.addSession("bootstrap")
	f.panes[0].cmd = "zsh"
	e, _, _ := newRestoreEngine(t, f)
	e.procs = rprocs{alive: map[int]bool{}, table: psLine(f.panes[0].pid+1, f.panes[0].pid, "compinit")}
	publish(t, e, snapOf("a:0:1", "a:1:1"))
	res, _, err := restoreFake(t, e, RestoreRequest{Kind: KindBoot})
	if err != nil || len(res.Created) == 0 {
		t.Fatalf("boot restore with an rc child: %v, created %d", err, len(res.Created))
	}
}

// TestRestore_BootLockTimeoutPinsSource (G1-2-H2): a boot restore whose
// bounded save.lock wait times out (a hung `moox save` held it) used to
// return before any pin, so the saves that resume could supersede the
// workspace that never came back. It now waits on for the lock — the
// daemon's saving stays held as long as Restore has not returned — and pins
// the source it would have restored, resolved before the wait, under it.
// A daemon that stops meanwhile (ctx done) returns without a pin.
func TestRestore_BootLockTimeoutPinsSource(t *testing.T) {
	f := newRFake()
	f.addSession("bootstrap")
	e, _, _ := newRestoreEngine(t, f)
	e.lockWait = 50 * time.Millisecond
	name := publish(t, e, snapOf("a:0:1", "a:1:1"))
	holder, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	l, err := holder.TryLock(store.Record{Pid: os.Getpid(), Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Restore(context.Background(), RestoreRequest{Kind: KindBoot})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the boot restore returned (%v) while save.lock was still held: saving would resume with nothing pinned", err)
	case <-time.After(500 * time.Millisecond): // 10× the bounded wait
	}
	l.Release()
	select {
	case err := <-done:
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("boot restore: %v, want ErrLockTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the boot restore did not return after save.lock was released")
	}
	pins, err := holder.Pins()
	if err != nil || len(pins) != 1 || pins[0] != name {
		t.Fatalf("pins = %v (%v), want [%s]", pins, err, name)
	}
	if w := f.opts["@moox-warning"]; !strings.Contains(w, "1 pinned") {
		t.Fatalf("@moox-warning = %q", w)
	}
	for _, c := range f.log {
		switch c[0] {
		case "rename-session", "new-session", "new-window", "split-window", "kill-session":
			t.Fatalf("a timed-out boot restore ran %v", c)
		}
	}

	// The daemon stops while it waits: Restore returns, nothing is pinned.
	g := newRFake()
	g.addSession("bootstrap")
	e2, _, _ := newRestoreEngine(t, g)
	e2.lockWait = 50 * time.Millisecond
	publish(t, e2, snapOf("a:0:1", "a:1:1"))
	h2, err := store.Open(e2.paths, e2.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Close() }()
	l2, err := h2.TryLock(store.Record{Pid: os.Getpid(), Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := e2.Restore(ctx, RestoreRequest{Kind: KindBoot}); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("boot restore with a stopping daemon: %v, want ErrLockTimeout", err)
	}
	if pins, _ := h2.Pins(); len(pins) != 0 {
		t.Fatalf("pinned %v without the lock", pins)
	}

	// A manual restore that times out reports it to its caller at once.
	m := newRFake()
	m.addSession("bootstrap")
	e3, _, _ := newRestoreEngine(t, m)
	e3.lockWait = 50 * time.Millisecond
	publish(t, e3, snapOf("a:0:1", "a:1:1"))
	h3, err := store.Open(e3.paths, e3.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h3.Close() }()
	l3, err := h3.TryLock(store.Record{Pid: os.Getpid(), Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Release()
	start := time.Now()
	if _, err := e3.Restore(context.Background(), RestoreRequest{}); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("manual restore: %v, want ErrLockTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a manual restore waited %s past its bounded lock wait", d)
	}
}

func psLine(pid, ppid int, cmd string) string {
	return " " + itoa(pid) + " " + itoa(ppid) + " Sat Oct 25 03:00:00 2026 " + cmd + "\n"
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestRestore_ReadsSnapshotOnce (T13): the snapshot is read once, at the
// start, under save.lock; nothing published meanwhile changes what is
// restored.
func TestRestore_ReadsSnapshotOnce(t *testing.T) {
	f := newRFake()
	f.addSession("bootstrap")
	e, _, _ := newRestoreEngine(t, f)
	publish(t, e, snapOf("a:0:1", "a:1:2"))
	reads := 0
	e.onStep = func(s string) {
		if s == "read-snapshot" {
			reads++
		}
	}
	res, _, err := restoreFake(t, e, RestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(res.Created) != 3 {
		t.Fatalf("%d reads, %d created", reads, len(res.Created))
	}
}

// TestRestore_PositionSidecarIgnored (B35, R7): the restore maps rows to panes
// by the ids its own commands printed; a stale position sidecar is never
// read and never decides a sid.
func TestRestore_PositionSidecarIgnored(t *testing.T) {
	f := newRFake()
	f.addSession("bootstrap")
	e, _, _ := newRestoreEngine(t, f)
	snap := snapOf("a:0:1")
	snap.Panes[0].Role = snapshot.RoleClaude
	snap.Panes[0].Claude = &snapshot.Claude{SessionID: "sid-from-snapshot", Launcher: "claude"}
	publish(t, e, snap)
	writeFile(t, e.paths.Panes, "a-0-0.session-id", "sid-stale\n")
	res, _, err := restoreFake(t, e, RestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Plan.Rows[0].Cmd; got != "claude --resume sid-from-snapshot" {
		t.Fatalf("resume %q", got)
	}
	for _, p := range f.panes {
		for _, kv := range p.env {
			if strings.Contains(kv, "sid-stale") {
				t.Fatalf("the stale sidecar reached a pane: %q", kv)
			}
		}
	}
}

// TestRestore_StaleFrozenIntentLoggedNotRefrozen (B43): a freeze-store entry
// whose tombstone is not in the snapshot is logged; the store is read-only
// and nothing is refrozen.
func TestRestore_StaleFrozenIntentLoggedNotRefrozen(t *testing.T) {
	f := newRFake()
	f.addSession("bootstrap")
	e, sc, log := newRestoreEngine(t, f)
	snap := snapOf("a:0:1", "a:1:1")
	snap.Panes[1].Role, snap.Panes[1].FrozenKey, snap.Panes[1].Title = snapshot.RoleFrozen, "k-claimed", "❄ FROZEN k-claimed"
	publish(t, e, snap)
	state := "v\t1\nunit\tpane\npane_count\t0\nsid_count\t0\nend\t1\n"
	writeFile(t, e.paths.FreezeDir, "k-stale.state", state)
	writeFile(t, e.paths.FreezeDir, "k-claimed.state", state)
	writeFile(t, e.paths.FreezeDir, "k-claimed.banner", "BANNER\n")
	sc.out["reclaim"] = "FROZEN-CLAIMED %3 key=k-claimed unit=pane\n"
	res, lv, err := restoreFake(t, e, RestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "FROZEN-STALE-INTENT key=k-stale") ||
		strings.Contains(log.String(), "FROZEN-STALE-INTENT key=k-claimed") {
		t.Fatalf("log:\n%s", log)
	}
	if b, _ := os.ReadFile(filepath.Join(e.paths.FreezeDir, "k-stale.state")); string(b) != state {
		t.Fatalf("the stale entry was rewritten: %q", b)
	}
	for _, c := range sc.calls {
		if c[1] == "freeze" || (c[1] == "reclaim" && c[2] == "k-stale") {
			t.Fatalf("refrozen or reclaimed the stale intent: %v", c)
		}
	}
	// the tombstone: reclaimed, its banner delivered as contents, neither
	// considered nor judged (R2-H8)
	tomb := res.Plan.Rows[1]
	if tomb.Kind != relaunch.KindFrozen || !tomb.Contents || tomb.Resumes() {
		t.Fatalf("tombstone row %+v", tomb)
	}
	if res.Boot.Result != verdict.Nothing || res.Boot.Misses != 0 || len(lv.States) != 0 {
		t.Fatalf("BOOT %+v LAUNCH %+v", res.Boot, lv)
	}
	// a FROZEN-ORPHAN reclaim is a miss
	f2 := newRFake()
	f2.addSession("bootstrap")
	e2, sc2, _ := newRestoreEngine(t, f2)
	publish(t, e2, snap)
	sc2.out["reclaim"] = "FROZEN-ORPHAN %3 key=k-claimed: no readable state file, REAL MISS\n"
	res2, _, err := restoreFake(t, e2, RestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Boot.Result != verdict.Incomplete || res2.Boot.Misses != 1 {
		t.Fatalf("orphan: BOOT %+v", res2.Boot)
	}
}

// TestLaunchVerdict_NoTimerAfterVerdict (tt §9.3a, PR-L3): every one-shot the
// probe chain arms has fired or been stopped when the verdict is written —
// nothing stays armed, whether the rows decide early or the deadline ends it.
func TestLaunchVerdict_NoTimerAfterVerdict(t *testing.T) {
	for _, decide := range []bool{true, false} {
		f := newRFake()
		e, _, _ := newRestoreEngine(t, f)
		var mu sync.Mutex
		armed := 0
		add := func(n int) { mu.Lock(); armed += n; mu.Unlock() }
		live := func() int { mu.Lock(); defer mu.Unlock(); return armed }
		// a real one-shot, tracked: +1 when armed, -1 when it fires or a Stop
		// prevents it from firing
		e.newTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
			tm := time.NewTimer(d)
			add(1)
			out, done := make(chan time.Time, 1), make(chan struct{})
			go func() {
				select {
				case v := <-tm.C:
					add(-1)
					out <- v
				case <-done:
				}
			}()
			return out, func() bool {
				if tm.Stop() {
					add(-1)
					close(done)
					return true
				}
				return false
			}
		}
		f.addSession("s")
		pane := f.panes[0]
		pane.cmd = "sleep"
		ack := t.TempDir()
		if decide {
			writeFile(t, ack, "r0", "")
		}
		rows := []launchRow{{LaunchRow: verdict.LaunchRow{Key: "s:0.0", Pane: pane.id, Token: "sleep"}, ack: "r0"}}
		lv := e.launchVerdict(context.Background(), rows, ack)
		if n := live(); n != 0 {
			t.Fatalf("decide=%v: %d timer(s) still armed after the verdict %+v", decide, n, lv)
		}
		want := map[bool]string{true: verdict.StateOK, false: verdict.StateNotFired}[decide]
		if lv.States["s:0.0"] != want {
			t.Fatalf("decide=%v: %+v", decide, lv)
		}
	}
}
