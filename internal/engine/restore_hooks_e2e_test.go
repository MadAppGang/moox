//go:build tmuxe2e

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// hookServer is a cold server on the real config with hook-shell panes and
// the gate's MOOX_* environment (C1), no snapshot (the boot gate skips): the
// stage for in-process restores whose delivery a real hook consumes.
func hookServer(t *testing.T, opts ...tmuxtest.Option) (*tmuxtest.Server, *Engine) {
	t.Helper()
	opts = append([]tmuxtest.Option{tmuxtest.WithBinaries(), tmuxtest.WithShell(tmuxtest.HookZsh(t)), tmuxtest.WithSize(200, 50)}, opts...)
	s := tmuxtest.StartCold(t, opts...)
	if s.Opt(t, "@moox-active") != "1" {
		t.Fatal("the gate did not activate")
	}
	return s, e2eEngine(t, s)
}

// recordingRunner records every tmux command an engine issues.
type recordingRunner struct {
	r   tmuxRunner
	mu  sync.Mutex
	log [][]string
}

func (r *recordingRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.log = append(r.log, append([]string(nil), args...))
	r.mu.Unlock()
	return r.r.Run(ctx, args...)
}

// recordingScripts records every script call and runs it.
type recordingScripts struct {
	s     scriptRunner
	mu    sync.Mutex
	calls []string
}

func (r *recordingScripts) Run(ctx context.Context, env []string, path string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, filepath.Base(path)+" "+strings.Join(args, " "))
	r.mu.Unlock()
	return r.s.Run(ctx, env, path, args...)
}

func claudeRow(p *snapshot.Pane, sid, launch string) {
	p.Role = snapshot.RoleClaude
	p.Claude = &snapshot.Claude{SessionID: sid, Launcher: "claude"}
	p.Launch = launch
}

// TestE2E_Restore_ContentsPrintedOnce (B46, T4): a pane's saved scrollback is
// printed once by the hook and never recorded as a command.
func TestE2E_Restore_ContentsPrintedOnce(t *testing.T) {
	s, e := hookServer(t)
	sid := newSession(t, s, "c", s.Home, "printf 'contents-once-marker\\n'; exec /bin/sh")
	pane := firstPane(t, s, sid)
	if !tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		return strings.Contains(s.Run(t, "capture-pane", "-p", "-t", pane), "contents-once-marker")
	}) {
		t.Fatal("never printed")
	}
	s.Run(t, "new-window", "-d", "-t", sid+":")
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	got := res.Created[saved.Panes[0].Key]
	var text string
	tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		text = s.Run(t, "capture-pane", "-p", "-J", "-S", "-", "-t", got)
		return strings.Contains(text, "contents-once-marker")
	})
	if n := strings.Count(text, "contents-once-marker"); n != 1 {
		t.Fatalf("printed %d times:\n%s", n, text)
	}
	if b, err := os.ReadFile(filepath.Join(e.Paths().Launch, strings.TrimPrefix(got, "%"))); err == nil && len(b) > 0 {
		t.Fatalf("the contents print was recorded: %q", b)
	}
}

// TestE2E_Restore_EnvDeliveryNoNudge (B36, B37, RR6): the resume reaches the
// shell through the env: no send-keys, no pending file, and the hook consumes
// it (ack, LAUNCH OK).
func TestE2E_Restore_EnvDeliveryNoNudge(t *testing.T) {
	s, e := hookServer(t)
	rec := &recordingRunner{r: e.tmux}
	e.tmux = rec
	snap := snapOf("n:0:1", "n:1:1")
	snap.Panes[0].Launch = "sleep 4242"
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, lv := restoreNow(t, e, true)
	for _, c := range rec.log {
		if c[0] == "send-keys" {
			t.Fatalf("the restore nudged: %v", c)
		}
	}
	if names, _ := os.ReadDir(e.Paths().Pending); len(names) != 0 {
		t.Fatalf("pending files written: %v", names)
	}
	if lv.States[res.Plan.Rows[0].Key] != verdict.StateOK || stubPayload(t, s, res.Created[res.Plan.Rows[0].Key]) != "sleep 4242" {
		t.Fatalf("LAUNCH %+v", lv)
	}
}

// TestE2E_Restore_SlowShellStillResumes (R29): a shell that comes up seconds
// after the pane still consumes its resume on its first prompt.
func TestE2E_Restore_SlowShellStillResumes(t *testing.T) {
	s, e := hookServer(t)
	// The bootstrap is an idle shell, as the manual-restore precondition and
	// its re-check under the lock require (G1-2-H1); only the panes the
	// restore creates get the slow shell.
	resetTo(t, s, "bootstrap")
	hook := s.Run(t, "show-options", "-gv", "default-command")
	s.Run(t, "set-option", "-g", "default-command", "sleep 2; "+strings.TrimSpace(hook))
	snap := snapOf("slow:0:1", "slow:1:1")
	snap.Panes[0].Launch = "sleep 2929"
	publish(t, e, snap)
	res, lv := restoreNow(t, e, true)
	if lv.Result != verdict.Pass || stubPayload(t, s, res.Created[res.Plan.Rows[0].Key]) != "sleep 2929" {
		t.Fatalf("LAUNCH %+v", lv)
	}
}

// TestE2E_Restore_ShellPaneStaysArmedForThaw (R2-H9, §4.4): a restored shell
// pane that printed contents and consumed no command stays armed: a pending
// file written by a stub thaw, then respawn-pane, is consumed.
func TestE2E_Restore_ShellPaneStaysArmedForThaw(t *testing.T) {
	s, e := hookServer(t)
	publish(t, e, snapOf("thaw:0:1", "thaw:1:1"))
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	pane := res.Created["thaw:0.0"]
	marker := filepath.Join(t.TempDir(), "thawed")
	writeFile(t, e.Paths().Pending, strings.TrimPrefix(pane, "%"), "touch "+shq(marker)+"\n")
	s.Run(t, "respawn-pane", "-k", "-t", pane)
	if !tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool { _, err := os.Stat(marker); return err == nil }) {
		t.Fatalf("the pending file was not consumed:\n%s", s.Run(t, "capture-pane", "-p", "-t", pane))
	}
}

// TestE2E_LaunchVerdict_ConsumeThenExitBeforeFirstProbe (R27, PR-C4): the
// `wt` usage error — a Claude row whose command runs and exits before the
// first probe — is judged by its durable ack: EXITED, LAUNCH FAIL, not
// NOT-FIRED.
func TestE2E_LaunchVerdict_ConsumeThenExitBeforeFirstProbe(t *testing.T) {
	s, e := hookServer(t)
	e.timing.first = 2 * time.Second
	bin := t.TempDir()
	writeFile(t, bin, "wt", "#!/bin/sh\necho 'usage: wt <name> <colour>' >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	snap := snapOf("wt:0:1", "wt:1:1")
	claudeRow(&snap.Panes[0], "sid-wt", bin+"/wt foo red")
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, lv := restoreNow(t, e, false)
	k := res.Plan.Rows[0].Key
	if res.Plan.Rows[0].Cmd != bin+"/wt foo red --resume sid-wt" {
		t.Fatalf("cmd %q", res.Plan.Rows[0].Cmd)
	}
	if lv.States[k] != verdict.StateExited || lv.Result != verdict.Fail {
		t.Fatalf("LAUNCH %+v", lv)
	}
	if !strings.Contains(s.Opt(t, "@claude-continuity-boot-warning"), "LAUNCH FAIL") {
		t.Fatalf("FAIL did not set the warning: %q", s.Opt(t, "@claude-continuity-boot-warning"))
	}
}

// seedFreeze writes a minimal valid pane entry and its banner (cc_store.sh
// format, verified by cc_store_verify).
func seedFreeze(t *testing.T, dir, key, banner string) {
	t.Helper()
	writeFile(t, dir, key+".state", "v\t1\nunit\tpane\nserver_pid\t1\nwindow_id\t@1\npane_id\t%1\npane_count\t0\nsid_count\t0\nend\t1\n")
	if banner != "" {
		writeFile(t, dir, key+".banner", banner)
	}
}

// TestE2E_Restore_TombstoneFirstNoRelaunch (B38, R31): a frozen row is
// reclaimed (cc_freeze.sh reclaim: @cc-frozen on the new pane, the entry
// re-pointed) and NEVER relaunched, even with a sid; its banner is its
// contents and it is neither considered nor judged
// (TestE2E_Restore_TombstoneBannerIsContentsNotJudged).
func TestE2E_Restore_TombstoneFirstNoRelaunch(t *testing.T) {
	s, e := hookServer(t)
	const key = "1761350400-abcdef"
	seedFreeze(t, e.Paths().FreezeDir, key, "TOMBSTONE-BANNER\n")
	snap := snapOf("tomb:0:1", "tomb:1:1")
	p := &snap.Panes[0]
	claudeRow(p, "sid-frozen", "")
	p.Role, p.FrozenKey, p.Title = snapshot.RoleFrozen, key, "❄ FROZEN "+key+" 1p/1s 2026-10-25"
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, lv := restoreNow(t, e, true)
	pane := res.Created[p.Key]
	if r := res.Plan.Rows[0]; r.Kind != relaunch.KindFrozen || r.Resumes() || !r.Contents {
		t.Fatalf("row %+v", r)
	}
	if len(lv.States) != 0 || res.Boot.Result != verdict.Nothing || res.Boot.Misses != 0 {
		t.Fatalf("BOOT %+v LAUNCH %+v", res.Boot, lv)
	}
	if got := strings.TrimSpace(s.Run(t, "show-options", "-pqv", "-t", pane, "@cc-frozen")); got != key {
		t.Fatalf("@cc-frozen %q", got)
	}
	state, _ := os.ReadFile(filepath.Join(e.Paths().FreezeDir, key+".state"))
	if !strings.Contains(string(state), "pane_id\t"+pane+"\n") {
		t.Fatalf("entry not re-pointed at %s:\n%s", pane, state)
	}
	var text string
	tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		text = s.Run(t, "capture-pane", "-p", "-t", pane)
		return strings.Contains(text, "TOMBSTONE-BANNER")
	})
	if !strings.Contains(text, "TOMBSTONE-BANNER") || strings.Contains(text, "MOOX-STUB") {
		t.Fatalf("tombstone pane:\n%s", text)
	}
}

// TestE2E_Restore_TombstoneBannerIsContentsNotJudged: the banner travels as
// MOOX_RESTORE_CONTENT, a tombstone has no MOOX_RESUME and no ack, so it
// never enters the LAUNCH verdict (R2-H8); a missing banner changes nothing.
func TestE2E_Restore_TombstoneBannerIsContentsNotJudged(t *testing.T) {
	s, e := hookServer(t)
	const key = "1761350400-bbbbbb"
	seedFreeze(t, e.Paths().FreezeDir, key, "") // no banner
	snap := snapOf("tb:0:1", "tb:1:1")
	snap.Panes[0].Role, snap.Panes[0].FrozenKey, snap.Panes[0].Title = snapshot.RoleFrozen, key, "❄ FROZEN "+key
	snap.Panes[1].Launch = "sleep 777"
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, lv := restoreNow(t, e, true)
	if res.Plan.Rows[0].Contents || len(lv.States) != 1 || lv.States[snap.Panes[1].Key] != verdict.StateOK {
		t.Fatalf("row %+v LAUNCH %+v", res.Plan.Rows[0], lv)
	}
	if res.Boot.Result != verdict.Pass || res.Boot.Considered != 1 {
		t.Fatalf("BOOT %+v", res.Boot)
	}
}

// TestE2E_Restore_FrozenTitleSurvivesShellOSC (PR-H12): exec D turns
// allow-set-title off before it sets the frozen title, so a shell's later
// OSC 2 cannot replace the tombstone's key carrier.
func TestE2E_Restore_FrozenTitleSurvivesShellOSC(t *testing.T) {
	s, e := hookServer(t)
	resetTo(t, s, "bootstrap") // an idle bootstrap (G1-2-H1); the restored panes get the OSC shell
	hook := strings.TrimSpace(s.Run(t, "show-options", "-gv", "default-command"))
	s.Run(t, "set-option", "-g", "default-command", `sleep 1; printf '\033]2;SHELL-OSC-TITLE\033\\'; `+hook)
	const key = "1761350400-cccccc"
	seedFreeze(t, e.Paths().FreezeDir, key, "")
	snap := snapOf("osc:0:2", "osc:1:1")
	title := "❄ FROZEN " + key + " 1p/0s 2026-10-25"
	snap.Panes[0].Role, snap.Panes[0].FrozenKey, snap.Panes[0].Title = snapshot.RoleFrozen, key, title
	publish(t, e, snap)
	res, _ := restoreNow(t, e, true)
	other := res.Created[snap.Panes[1].Key]
	if !tmuxtest.Poll(5*time.Second, 100*time.Millisecond, func() bool {
		return strings.TrimSpace(s.Run(t, "display", "-p", "-t", other, "#{pane_title}")) == "SHELL-OSC-TITLE"
	}) {
		t.Fatal("the control pane never took the shell's OSC title (the oracle is dead)")
	}
	if got := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[snap.Panes[0].Key], "#{pane_title}")); got != title {
		t.Fatalf("tombstone title %q, want %q", got, title)
	}
}

// TestE2E_Restore_RunsTabReconcileLast (B44, I13): ledger seed (only when the
// freeze store exists) and then cc_tab_reconcile.sh --all are the restore's
// last script calls, after every reclaim.
func TestE2E_Restore_RunsTabReconcileLast(t *testing.T) {
	s, e := hookServer(t)
	const key = "1761350400-dddddd"
	seedFreeze(t, e.Paths().FreezeDir, key, "")
	sc := &recordingScripts{s: e.scripts}
	e.scripts = sc
	snap := snapOf("tab:0:1", "tab:1:1")
	snap.Panes[0].Role, snap.Panes[0].FrozenKey = snapshot.RoleFrozen, key
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	restoreNow(t, e, true)
	got := strings.Join(sc.calls, " | ")
	want := "cc_freeze.sh reclaim " + key
	if len(sc.calls) != 3 || !strings.HasPrefix(sc.calls[0], want) || sc.calls[1] != "cc_freeze.sh ledger-seed" ||
		sc.calls[2] != "cc_tab_reconcile.sh --all" {
		t.Fatalf("script calls: %s", got)
	}
}

// TestE2E_Restore_ScratchContentsCleared (T12): after the LAUNCH verdict
// restore/<run>/ — contents no shell consumed, acks — is gone.
func TestE2E_Restore_ScratchContentsCleared(t *testing.T) {
	s, e := hookServer(t)
	snap := snapOf("scr:0:1", "scr:1:1")
	snap.Panes[0].Launch = "sleep 31"
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	restoreNow(t, e, true)
	names, err := os.ReadDir(filepath.Join(e.Paths().State, "restore"))
	if err != nil || len(names) != 0 {
		t.Fatalf("restore/ after the verdict: %v %v", names, err)
	}
}

// TestE2E_Restore_NoObserverPanes (B45, T9, RR10): a tt-observer row is not
// restored; its leaf is removed from the layout tmux accepts.
func TestE2E_Restore_NoObserverPanes(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(200, 60))
	e := e2eEngine(t, s)
	snap := snapOf("obs:0:3", "obs:1:1")
	snap.Panes[1].Role = snapshot.RoleTTObserver
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	if _, ok := res.Created[snap.Panes[1].Key]; ok || len(res.Plan.Dropped) != 1 {
		t.Fatalf("observer restored: %+v", res.Plan.Dropped)
	}
	n := strings.Count(s.Run(t, "list-panes", "-t", res.Created[snap.Panes[0].Key], "-F", "#{pane_id}"), "%")
	if n != 2 {
		t.Fatalf("window 0 has %d panes, want 2", n)
	}
	root, _, _ := layout.Parse(snap.Windows[0].Layout)
	if err := root.RemoveLeaf(snap.Panes[1].SavedID); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[snap.Panes[0].Key], "#{window_layout}"))
	if geometry(t, got) != geometry(t, root.EncodeV1()) {
		t.Fatalf("layout %s, want %s", geometry(t, got), geometry(t, root.EncodeV1()))
	}
}

// TestE2E_Restore_DropsBootstrapOneSessionSnapshot (T5): a one-session
// snapshot still drops the bootstrap (≥1 restored session).
func TestE2E_Restore_DropsBootstrapOneSessionSnapshot(t *testing.T) {
	s := tmuxtest.Start(t)
	e := e2eEngine(t, s)
	publish(t, e, snapOf("only:0:1", "only:1:1"))
	boot := resetTo(t, s, "0")
	restoreNow(t, e, true)
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "only" {
		t.Fatalf("sessions %q", got)
	}
	if _, err := s.RunErr("has-session", "-t", boot); err == nil {
		t.Fatal("bootstrap kept")
	}
}

// TestE2E_Restore_RenamesCollidingBootstrap (E4, T5): a bootstrap named like a
// snapshot session is renamed moox-boot-<pid> first, so the restore's own
// new-session never fails with "duplicate session", then dropped.
func TestE2E_Restore_RenamesCollidingBootstrap(t *testing.T) {
	s := tmuxtest.Start(t)
	e := e2eEngine(t, s)
	log := &logBuf{}
	e.log = log
	publish(t, e, snapOf("main:0:1", "main:1:1"))
	boot := resetTo(t, s, "main")
	res, _ := restoreNow(t, e, true)
	if res.Boot.Misses != 0 || !strings.Contains(log.String(), `BOOTSTRAP `+boot+` "main" renamed moox-boot-`) {
		t.Fatalf("BOOT %+v log:\n%s", res.Boot, log)
	}
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "main" {
		t.Fatalf("sessions %q", got)
	}
}

// TestE2E_RoundTrip_GroupedSessions (S5, T11): a grouped secondary comes back
// in the primary's group, sharing its windows, with its own active window.
func TestE2E_RoundTrip_GroupedSessions(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	p := newSession(t, s, "prim", s.Home)
	s.Run(t, "new-window", "-d", "-t", p+":")
	s.Run(t, "new-window", "-d", "-t", p+":")
	g := strings.TrimSpace(s.Run(t, tmuxtarget.NewSession{Name: tmuxtarget.Literal("view"), Group: p, PrintFormat: "#{session_id}"}.Args()...))
	s.Run(t, "select-window", "-t", g+":2")
	s.Run(t, "select-window", "-t", p+":1")
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	resetTo(t, s, "bootstrap")
	restoreNow(t, e, true)
	out := s.Run(t, "list-sessions", "-F", "#{session_name} #{session_group} #{session_windows} #{window_index}")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("sessions:\n%s", out)
	}
	want := map[string]int{}
	for _, se := range saved.Sessions {
		want[se.Name] = se.ActiveWindow
	}
	group := ""
	for _, l := range lines {
		f := strings.Fields(l)
		if group == "" {
			group = f[1]
		}
		if f[1] != group || f[1] == "" || f[2] != "3" || f[3] != itoa(want[f[0]]) {
			t.Errorf("session line %q (want group %q, 3 windows, active %d)", l, group, want[f[0]])
		}
	}
}

// TestE2E_Restore_DefaultSizeUnset (PV4, T11, Q4/Q10): new-session -x -y's
// session default-size is unset after the restore, so a window the user
// creates later gets the global size, while restored windows keep theirs.
func TestE2E_Restore_DefaultSizeUnset(t *testing.T) {
	s := tmuxtest.Start(t)
	e := e2eEngine(t, s)
	snap := snapOf("ds:0:1", "ds:1:1")
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	sid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created["ds:0.0"], "#{session_id}"))
	if v := strings.TrimSpace(s.Run(t, "show-options", "-qv", "-t", sid, "default-size")); v != "" {
		t.Fatalf("session default-size %q", v)
	}
	global := strings.TrimSpace(s.Run(t, "show-options", "-gqv", "default-size"))
	later := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_width}x#{window_height}"))
	if later != global {
		t.Fatalf("a later window is %s, want the global %s", later, global)
	}
	if got := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created["ds:0.0"], "#{window_width}x#{window_height}")); got != "80x24" {
		t.Fatalf("restored window %s, want its layout's 80x24", got)
	}
}

// TestE2E_Restore_ActivePane (T10): every window's active pane is the saved
// one.
func TestE2E_Restore_ActivePane(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 60))
	e := e2eEngine(t, s)
	snap := snapOf("act:0:3", "act:1:2")
	snap.Panes[0].Active, snap.Panes[2].Active = false, true
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	for _, p := range snap.Panes {
		got := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[p.Key], "#{pane_active}"))
		if (got == "1") != p.Active {
			t.Errorf("%s active=%s, want %v", p.Key, got, p.Active)
		}
	}
}

// TestE2E_Restore_RenumberedWindowNoMisroute (R6): windows at indices 1, 4, 9
// each get their own resume — the row → pane map is by the ids the creating
// commands printed, never by position.
func TestE2E_Restore_RenumberedWindowNoMisroute(t *testing.T) {
	s := tmuxtest.Start(t)
	e := e2eEngine(t, s)
	e.timing.deadline = time.Second
	snap := snapOf("rn:1:1", "rn:4:1", "rn:9:1")
	for i := range snap.Panes {
		snap.Panes[i].Launch = "sleep " + itoa(snap.Panes[i].Window)
	}
	publish(t, e, snap)
	dump := envDump(t, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, false)
	for _, p := range snap.Panes {
		pane := res.Created[p.Key]
		if where := strings.TrimSpace(s.Run(t, "display", "-p", "-t", pane, "#I")); where != itoa(p.Window) {
			t.Errorf("%s landed in window %s", p.Key, where)
		}
		if got := paneEnv(t, dump, pane)["MOOX_RESUME"]; got != p.Launch {
			t.Errorf("%s got %q, want %q", p.Key, got, p.Launch)
		}
	}
}

// TestE2E_Restore_TitlesVerbatim (B39): titles come back byte-identical,
// unicode and the empty title included.
func TestE2E_Restore_TitlesVerbatim(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 60))
	e := e2eEngine(t, s)
	snap := snapOf("tv:0:4", "tv:1:1")
	for i, title := range []string{"✳ Claude Code", "", "two  spaces ; and #", "émoji 🎉 x", "plain"} {
		snap.Panes[i].Title = title
	}
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	for _, p := range snap.Panes {
		got := strings.TrimSuffix(s.Run(t, "display", "-p", "-t", res.Created[p.Key], "#{pane_title}"), "\n")
		if got != p.Title {
			t.Errorf("%s title %q, want %q", p.Key, got, p.Title)
		}
	}
}

// TestE2E_Restore_ResumeNotInheritedByUserNewWindow (C2, E1): after the
// restore, a window the user creates in a restored session gets no
// MOOX_RESUME, MOOX_RESTORE_CONTENT or MOOX_RESUME_ACK.
func TestE2E_Restore_ResumeNotInheritedByUserNewWindow(t *testing.T) {
	s := tmuxtest.Start(t)
	e := e2eEngine(t, s)
	e.timing.deadline = time.Second
	snap := snapOf("u:0:1", "u:1:1")
	snap.Panes[0].Launch = "sleep 55"
	publish(t, e, snap)
	dump := envDump(t, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, false)
	sid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created["u:0.0"], "#{session_id}"))
	if paneEnv(t, dump, res.Created["u:0.0"])["MOOX_RESUME"] != "sleep 55" {
		t.Fatal("the oracle pane did not get its resume")
	}
	for _, args := range [][]string{{"new-window", "-d", "-t", sid + ":"}, {"split-window", "-d", "-t", res.Created["u:0.0"]}} {
		pane := strings.TrimSpace(s.Run(t, append(args, "-P", "-F", "#{pane_id}")...))
		env := paneEnv(t, dump, pane)
		for _, k := range []string{envResume, envContent, envAck} {
			if v, ok := env[k]; ok {
				t.Errorf("%s: %s=%q", args[0], k, v)
			}
		}
	}
}

// TestE2E_Restore_SessionCreateFailurePins (§6.2 pins): a restore whose
// exec A fails for a session is INCOMPLETE and pins its source; the warning
// names the pin.
func TestE2E_Restore_SessionCreateFailurePins(t *testing.T) {
	s := tmuxtest.Start(t)
	e := e2eEngine(t, s)
	name := publish(t, e, snapOf("ok:0:1", "bad:0:1"))
	resetTo(t, s, "bootstrap")
	e.tmux = failingTmux{r: s.T, fail: func(args []string) bool {
		return args[0] == "new-session" && strings.Contains(" "+strings.Join(args, " ")+" ", " -s bad ")
	}}
	res, _ := restoreNow(t, e, true)
	if res.Boot.Result != verdict.Incomplete || res.Boot.Misses != 1 {
		t.Fatalf("BOOT %+v", res.Boot)
	}
	pins, _ := os.ReadFile(filepath.Join(e.Paths().State, "pins"))
	if strings.TrimSpace(string(pins)) != name || !strings.Contains(s.Opt(t, "@moox-warning"), "1 pinned") {
		t.Fatalf("pins %q warning %q", pins, s.Opt(t, "@moox-warning"))
	}
}
