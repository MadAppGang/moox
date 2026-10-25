//go:build tmuxe2e

package main

// E2E tests of the moox command on ISOLATED servers (tmuxtest: sockets
// under a temp root, scratch HOME/XDG, /bin/sh panes, TMUX_TMPDIR pointing
// at an empty dir). The binary under test is built into the server's
// scratch bin dir (WithBinaries) and run with Server.Env().

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func scratch(s *tmuxtest.Server) tmuxtarget.MapEnv {
	return tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS, "XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC}
}

// mooxBin runs the built moox against s with s.Env(); no terminal.
func mooxBin(t *testing.T, s *tmuxtest.Server, extraEnv []string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(s.Bin, "moox"), args...)
	cmd.Env = s.Env(extraEnv...)
	cmd.Dir = s.Home
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("moox %v: %v", args, err)
	}
	return code, out.String(), errb.String()
}

func newSess(t *testing.T, s *tmuxtest.Server, name string) string {
	t.Helper()
	return strings.TrimSpace(s.Run(t, tmuxtarget.Chain(tmuxtarget.NewSession{Name: tmuxtarget.Literal(name),
		Cwd: tmuxtarget.Literal(s.Home), Width: 160, Height: 40, PrintFormat: "#{session_id}"}.Args())...))
}

func sessions(t *testing.T, s *tmuxtest.Server) []string {
	t.Helper()
	out, err := s.RunErr("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil
	}
	f := strings.Fields(out)
	sort.Strings(f)
	return f
}

func paneCount(t *testing.T, s *tmuxtest.Server) int {
	t.Helper()
	return len(strings.Fields(s.Run(t, "list-panes", "-a", "-F", "#{pane_id}")))
}

// seedSnapshot builds a workspace on a throwaway server and saves it: two
// sessions ("work" with 2 windows, one split; "notes"), returns the snapshot
// dir and the saved snapshot's path.
func seedSnapshot(t *testing.T) (string, string) {
	t.Helper()
	a := tmuxtest.Start(t, tmuxtest.WithSession("boot"))
	newSess(t, a, "work")
	a.Run(t, "new-window", "-t", "work:", "-d")
	a.Run(t, "split-window", "-t", "work:1", "-d")
	newSess(t, a, "notes")
	a.Run(t, "kill-session", "-t", "boot")
	a.Run(t, "set-option", "-g", "@moox-active", "1")
	e, err := engine.New(engine.Options{Target: a.T, Env: scratch(a), Producer: "moox e2e"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Save(context.Background(), engine.SaveRequest{Reason: "seed", Force: true})
	if err != nil || !res.Published {
		t.Fatalf("seed save: %+v %v", res, err)
	}
	last, err := e.Last()
	if err != nil {
		t.Fatal(err)
	}
	return a.Paths(t).Snapshots, last.Path
}

// waitIdle waits until no restore owns the server (the LAUNCH verdict ran).
func waitIdle(t *testing.T, s *tmuxtest.Server) {
	t.Helper()
	if !tmuxtest.Poll(60*time.Second, 200*time.Millisecond, func() bool {
		out, err := s.RunErr("show-options", "-gqv", "@moox-restoring")
		return err == nil && strings.TrimSpace(out) == ""
	}) {
		t.Fatalf("@moox-restoring still set after 60 s: %q", s.Opt(t, "@moox-restoring"))
	}
}

// TestE2E_Start_WaitsForRestoreThenAttachTarget (§8.2): `moox` with no
// server starts one on the real config; the boot gate restores the seeded
// snapshot; moox waits for it and names the restored session as the attach
// target (attach replaced by --dry-attach), never the dropped bootstrap.
func TestE2E_Start_WaitsForRestoreThenAttachTarget(t *testing.T) {
	dir, _ := seedSnapshot(t)
	b := tmuxtest.Prepare(t, tmuxtest.WithBinaries(), tmuxtest.WithSnapshot(dir))
	code, out, errOut := mooxBin(t, b, nil, "-S", b.T.Socket, "--dry-attach")
	if code != 0 {
		t.Fatalf("moox exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got := sessions(t, b); strings.Join(got, ",") != "notes,work" {
		t.Fatalf("sessions after the start = %v (want the snapshot's, bootstrap dropped)\nstderr: %s", got, errOut)
	}
	if r := b.Opt(t, "@moox-boot-result"); r != "PASS" && r != "NOTHING" {
		t.Fatalf("@moox-boot-result = %q\nstderr: %s", r, errOut)
	}
	target := b.Opt(t, "@moox-restore-target")
	if want := "attach-session -t " + target; target == "" || strings.TrimSpace(out) != want {
		t.Fatalf("attach line %q, want %q", strings.TrimSpace(out), want)
	}
	name := strings.TrimSpace(b.Run(t, "display-message", "-p", "-t", target, "#{session_name}"))
	if name != "notes" && name != "work" {
		t.Fatalf("attach target %s is session %q", target, name)
	}
	if !strings.Contains(errOut, "restoring the saved workspace") {
		t.Errorf("no progress line:\n%s", errOut)
	}
	waitIdle(t, b)

	// A second `moox` finds the server and attaches (no new session).
	code, out, _ = mooxBin(t, b, nil, "-S", b.T.Socket, "--dry-attach")
	if code != 0 || strings.TrimSpace(out) != "attach-session" {
		t.Fatalf("second moox: exit %d, %q", code, out)
	}
	// No terminal and no --dry-attach: it stops after starting, exit 0.
	code, _, errOut = mooxBin(t, b, nil, "-S", b.T.Socket)
	if code != 0 || !strings.Contains(errOut, "attach from a terminal") {
		t.Fatalf("no-terminal moox: exit %d, %q", code, errOut)
	}
}

// TestE2E_Restore_NoServerBootstrapsWithHandover (§6.4a step 0, R2-H3):
// `moox restore` with nothing running starts the server with handover (the
// gate does not auto-restore), then the daemon restores on the request.
func TestE2E_Restore_NoServerBootstrapsWithHandover(t *testing.T) {
	dir, _ := seedSnapshot(t)
	b := tmuxtest.Prepare(t, tmuxtest.WithBinaries(), tmuxtest.WithSnapshot(dir))
	code, out, errOut := mooxBin(t, b, nil, "restore", "-S", b.T.Socket, "--stub-commands", "--wait-launch")
	if code != 0 {
		t.Fatalf("moox restore exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if r := b.Opt(t, "@moox-boot-result"); r != "HANDOVER" {
		t.Fatalf("@moox-boot-result = %q, want HANDOVER (the gate must not auto-restore)", r)
	}
	if r := strings.TrimSpace(out); r != "PASS" && r != "NOTHING" {
		t.Fatalf("restore result %q", r)
	}
	if got := sessions(t, b); strings.Join(got, ",") != "notes,work" {
		t.Fatalf("sessions = %v", got)
	}
	if !strings.Contains(errOut, "with handover") {
		t.Errorf("no bootstrap line:\n%s", errOut)
	}
	// A second request into the now-populated server is refused (exit 3).
	code, _, errOut = mooxBin(t, b, nil, "restore", "-S", b.T.Socket)
	if code != exitNotApplic {
		t.Fatalf("restore into a populated server: exit %d (%s)", code, errOut)
	}
}

// startTarget is a cold server on the real config that a moox binary can
// restart (local.conf carries the harness lines), with a workspace.
func startTarget(t *testing.T) *tmuxtest.Server {
	t.Helper()
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithLocalConf())
	newSess(t, s, "work")
	s.Run(t, "new-window", "-t", "work:", "-d")
	s.Run(t, "split-window", "-t", "work:1", "-d")
	if !tmuxtest.Poll(15*time.Second, 100*time.Millisecond, func() bool { return saver.Running(s.Paths(t)) }) {
		t.Fatal("the daemon did not start")
	}
	return s
}

func restartLog(s *tmuxtest.Server, t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(s.Paths(t).Log, "restart.log"))
	return string(b)
}

// restartFromPane types `moox restart <flags>` into a pane of s and waits
// for the detached process to bring the server back.
func restartFromPane(t *testing.T, s *tmuxtest.Server, pane string, flags ...string) {
	t.Helper()
	cmd := filepath.Join(s.Bin, "moox") + " restart " + strings.Join(flags, " ")
	s.Run(t, "send-keys", "-t", pane, "-l", cmd)
	s.Run(t, "send-keys", "-t", pane, "Enter")
	if !tmuxtest.Poll(90*time.Second, 250*time.Millisecond, func() bool {
		return strings.Contains(restartLog(s, t), "is back")
	}) {
		t.Fatalf("the server did not come back; restart.log:\n%s", restartLog(s, t))
	}
	waitIdle(t, s)
}

func testRestartFromPane(t *testing.T, flags ...string) {
	s := startTarget(t)
	before := paneCount(t, s)
	pane := strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", "work:1.1", "#{pane_id}"))
	oldBin := s.Opt(t, "@moox-bin") // the same bin after the restart
	srvPid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}"))
	restartFromPane(t, s, pane, flags...)

	if pid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}")); pid == srvPid {
		t.Fatalf("the server pid %s did not change: it was not restarted", pid)
	}
	if got := paneCount(t, s); got != before-1 {
		t.Fatalf("panes after the restart = %d, want %d (all but the restarting pane)\nrestart.log:\n%s", got, before-1, restartLog(s, t))
	}
	if got := strings.Join(sessions(t, s), ","); got != "boot,work" {
		t.Fatalf("sessions after the restart = %s", got)
	}
	if !strings.Contains(restartLog(s, t), "excluded pane \""+pane+"\"") {
		t.Fatalf("the final save did not exclude %s:\n%s", pane, restartLog(s, t))
	}
	// Nothing on the restored server runs `moox restart` again.
	if out := s.Run(t, "list-panes", "-a", "-F", "#{pane_start_command} #{pane_current_command}"); strings.Contains(out, "restart") {
		t.Fatalf("a restored pane runs restart again:\n%s", out)
	}
	if s.Opt(t, "@moox-bin") != oldBin {
		t.Fatalf("@moox-bin changed across the restart")
	}
}

// TestE2E_Restart_FromInsideTargetPane (§8.5 step 2, PR-H2): run from a pane
// of the target, restart re-execs detached (no terminal), survives its own
// session's kill, brings the server back restored, and the restarting pane
// is left out of the final save (R2-H16).
func TestE2E_Restart_FromInsideTargetPane(t *testing.T) { testRestartFromPane(t, "--yes") }

// TestE2E_Restart_ForcedNotReplayed: --force changes the abort rule only;
// the restarting pane is still excluded and is not replayed.
func TestE2E_Restart_ForcedNotReplayed(t *testing.T) { testRestartFromPane(t, "--yes", "--force") }

// TestE2E_Restart_NoSaveDuringTeardown (§8.5 steps 3–4): once restart has
// set @moox-restoring shutdown:<pid>, no other save publishes — a direct
// save from another process is refused shutting-down (75) — and the
// daemon is stopped before the final save.
func TestE2E_Restart_NoSaveDuringTeardown(t *testing.T) {
	s := startTarget(t)
	s.Run(t, "new-window", "-t", "work:", "-d") // something new for the final save to publish
	snaps := func() int {
		names, _ := os.ReadDir(s.Paths(t).Snapshots)
		n := 0
		for _, d := range names {
			if strings.HasPrefix(d.Name(), "moox-") && strings.HasSuffix(d.Name(), ".json") {
				n++
			}
		}
		return n
	}
	start := snaps()
	var checked []string
	restartHooks = func(step string) {
		switch step {
		case "marked", "saved":
			code, out, errOut := mooxBin(t, s, nil, "save", "--direct", "--force", "-S", s.T.Socket)
			if code != exitBusy || strings.TrimSpace(out) != "shutting-down" {
				t.Errorf("%s: a direct save exited %d %q (%s), want 75 shutting-down", step, code, out, errOut)
			}
		case "daemon-stopped":
			if saver.Running(s.Paths(t)) {
				t.Errorf("the daemon still runs after tt serve --stop")
			}
		}
		if step == "saved" && snaps() != start+1 {
			t.Errorf("snapshots after the final save = %d, want %d (only the restart's own)", snaps(), start+1)
		}
		if step == "marked" && snaps() != start {
			t.Errorf("a snapshot was published after the shutdown mark")
		}
		checked = append(checked, step)
	}
	defer func() { restartHooks = nil }()
	m := newMeta(context.Background(), s.Env())
	var out bytes.Buffer
	m.stdout, m.stderr = &out, &out
	m.stdinTTY, m.stdoutTTY = false, false
	code := run(m, []string{"restart", "--yes", "-S", s.T.Socket})
	if code != 0 {
		t.Fatalf("restart exit %d:\n%s", code, out.String())
	}
	if strings.Join(checked, ",") != "marked,daemon-stopped,saved,killed" {
		t.Fatalf("steps = %v", checked)
	}
	waitIdle(t, s)
	if got := strings.Join(sessions(t, s), ","); got != "boot,work" {
		t.Fatalf("sessions after the restart = %s\n%s", got, out.String())
	}
}

// TestE2E_Restart_SnapshotRecoversPinned (A16): a pinned snapshot comes
// back with `moox restart --snapshot FILE`: the start hands over and FILE
// is requested, not moox-last; a passing restore releases the pin.
func TestE2E_Restart_SnapshotRecoversPinned(t *testing.T) {
	s := startTarget(t)
	newSess(t, s, "pinned")
	s.Run(t, "new-window", "-t", "pinned:", "-d")
	e, err := engine.New(engine.Options{Target: s.T, Env: scratch(s), Producer: "moox e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := e.Save(context.Background(), engine.SaveRequest{Reason: "pin-me", Force: true}); err != nil || !res.Published {
		t.Fatalf("save: %+v %v", res, err)
	}
	pinned, err := e.Last()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Paths(t).State, "pins"), []byte(pinned.Name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The workspace moves on: moox-last no longer has "pinned".
	s.Run(t, "kill-session", "-t", "pinned")
	newSess(t, s, "later")
	pause(1100 * time.Millisecond) // a distinct snapshot timestamp
	if res, err := e.Save(context.Background(), engine.SaveRequest{Reason: "later", Force: true}); err != nil || !res.Published {
		t.Fatalf("save: %+v %v", res, err)
	}

	code, out, errOut := mooxBin(t, s, []string{"MOOX_STUB_COMMANDS=1"}, "restart", "--yes", "-S", s.T.Socket, "--snapshot", pinned.Path)
	if code != 0 {
		t.Fatalf("restart --snapshot exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	waitIdle(t, s)
	if got := strings.Join(sessions(t, s), ","); got != "boot,pinned,work" {
		t.Fatalf("sessions = %s, want the pinned snapshot's\n%s", got, errOut)
	}
	if r := s.Opt(t, "@moox-boot-result"); r != "HANDOVER" {
		t.Fatalf("@moox-boot-result = %q, want HANDOVER", r)
	}
	if !tmuxtest.Poll(20*time.Second, 200*time.Millisecond, func() bool {
		b, err := os.ReadFile(filepath.Join(s.Paths(t).State, "pins"))
		return err != nil || strings.TrimSpace(string(b)) == ""
	}) {
		b, _ := os.ReadFile(filepath.Join(s.Paths(t).State, "pins"))
		t.Fatalf("the pin was not released by the passing restore: %q", b)
	}
}

// restartInProcess runs `moox restart --yes -S <socket>` in this process
// with s's environment (plus extra) and returns its exit code and output.
func restartInProcess(t *testing.T, s *tmuxtest.Server, extra ...string) (int, string) {
	t.Helper()
	m := newMeta(context.Background(), s.Env(extra...))
	var out bytes.Buffer
	m.stdout, m.stderr = &out, &out
	m.stdinTTY, m.stdoutTTY = false, false
	return run(m, []string{"restart", "--yes", "-S", s.T.Socket}), out.String()
}

// TestE2E_Restart_RefusedWhileRestoring (G2-H1): while a live restore owns
// @moox-restoring, restart refuses with 75 BEFORE it touches anything: the
// marker keeps the restore's value, the daemon still runs, the server is
// the same server.
func TestE2E_Restart_RefusedWhileRestoring(t *testing.T) {
	s := startTarget(t)
	live := "restore:" + strconv.Itoa(os.Getpid()) + ":1700000000" // this test process: alive
	s.Run(t, "set-option", "-g", "@moox-restoring", live)
	srvPid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}"))
	code, out := restartInProcess(t, s)
	if code != exitBusy {
		t.Fatalf("restart during a live restore exited %d, want %d:\n%s", code, exitBusy, out)
	}
	if v := s.Opt(t, "@moox-restoring"); v != live {
		t.Fatalf("@moox-restoring = %q, want the restore's %q", v, live)
	}
	if !saver.Running(s.Paths(t)) {
		t.Fatal("the daemon (and its running restore) was stopped")
	}
	if pid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}")); pid != srvPid {
		t.Fatal("the server was restarted")
	}
	// an unparsable marker is refused the same way (fail closed)
	s.Run(t, "set-option", "-g", "@moox-restoring", "garbage")
	if code, out := restartInProcess(t, s); code != exitBusy || s.Opt(t, "@moox-restoring") != "garbage" {
		t.Fatalf("unparsable marker: exit %d, marker %q\n%s", code, s.Opt(t, "@moox-restoring"), out)
	}
	s.Run(t, "set-option", "-gu", "@moox-restoring")
}

// TestE2E_Restart_MarkerTakenAfterCASIsNotForced (G2-2-M2): a restore that
// writes @moox-restoring after restart's compare-and-set read-back (the
// daemon's writers are unconditional) owns the server. Restart re-reads the
// marker once the daemon is stopped and backs off with 75, leaving the
// restore's marker and the server alone, even with --force; and --force
// never overrides a "restoring" final save either.
func TestE2E_Restart_MarkerTakenAfterCASIsNotForced(t *testing.T) {
	s := startTarget(t)
	srvPid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}"))
	before := strings.Join(sessions(t, s), ",")
	live := "restore:" + strconv.Itoa(os.Getpid()) + ":1700000000" // this test process: alive
	restartHooks = func(step string) {
		if step == "marked" {
			s.Run(t, "set-option", "-g", "@moox-restoring", live)
		}
	}
	defer func() { restartHooks = nil }()
	m := newMeta(context.Background(), s.Env())
	var buf bytes.Buffer
	m.stdout, m.stderr = &buf, &buf
	m.stdinTTY, m.stdoutTTY = false, false
	code := run(m, []string{"restart", "--yes", "--force", "-S", s.T.Socket})
	out := buf.String()
	if code != exitBusy {
		t.Fatalf("restart --force with a restore marker taken after the CAS exited %d, want %d:\n%s", code, exitBusy, out)
	}
	if pid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}")); pid != srvPid {
		t.Fatalf("the server was restarted:\n%s", out)
	}
	if got := strings.Join(sessions(t, s), ","); got != before {
		t.Fatalf("sessions %s, want %s\n%s", got, before, out)
	}
	if v := s.Opt(t, "@moox-restoring"); v != live {
		t.Fatalf("@moox-restoring = %q, want the restore's %q", v, live)
	}
	s.Run(t, "set-option", "-gu", "@moox-restoring")
}

// TestE2E_Restart_OneWindowWorkspaceComesBack (G2-H3): the cold boot gate
// skips a one-window snapshot (SKIPPED:one-window), so a restart of a
// one-window, two-pane workspace requests it explicitly and the split comes
// back, instead of a fresh bootstrap reported as success.
func TestE2E_Restart_OneWindowWorkspaceComesBack(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithLocalConf())
	if !tmuxtest.Poll(15*time.Second, 100*time.Millisecond, func() bool { return saver.Running(s.Paths(t)) }) {
		t.Fatal("the daemon did not start")
	}
	for _, sess := range sessions(t, s) {
		if sess != "boot" {
			s.Run(t, "kill-session", "-t", "="+sess)
		}
	}
	s.Run(t, "split-window", "-t", "boot:", "-d")
	if n := paneCount(t, s); n != 2 {
		t.Fatalf("setup: %d panes", n)
	}
	code, out := restartInProcess(t, s, "MOOX_STUB_COMMANDS=1")
	if code != 0 {
		t.Fatalf("restart exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "ending the sessions:") {
		t.Fatalf("a clean session kill was logged as a failure (G2-2-L1):\n%s", out)
	}
	waitIdle(t, s)
	if n := paneCount(t, s); n != 2 || strings.Join(sessions(t, s), ",") != "boot" {
		t.Fatalf("after the restart: %d panes, sessions %v (want the 2-pane boot session back)\n%s", n, sessions(t, s), out)
	}
}

// TestE2E_Restart_OneWindowSurvivesStartRetry (G2-2-M1): the retried start
// after a failed first `new-session` keeps the explicit request. It used to
// drop it (snapPath = ""), so a one-window workspace hit SKIPPED:one-window
// again and came back as a fresh bootstrap, reported "is back", exit 0.
func TestE2E_Restart_OneWindowSurvivesStartRetry(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithLocalConf())
	if !tmuxtest.Poll(15*time.Second, 100*time.Millisecond, func() bool { return saver.Running(s.Paths(t)) }) {
		t.Fatal("the daemon did not start")
	}
	for _, sess := range sessions(t, s) {
		if sess != "boot" {
			s.Run(t, "kill-session", "-t", "="+sess)
		}
	}
	s.Run(t, "split-window", "-t", "boot:", "-d")
	if n := paneCount(t, s); n != 2 {
		t.Fatalf("setup: %d panes", n)
	}
	bin, flag := failingTmux(t, s, "new-session -d -s main ", true)
	restartHooks = func(step string) {
		if step == "killed" {
			if err := os.WriteFile(flag, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { restartHooks = nil }()
	code, out := restartInProcess(t, s, "MOOX_STUB_COMMANDS=1", "MOOX_TMUX_BIN="+bin)
	if code != 0 || !strings.Contains(out, "DOWN") {
		t.Fatalf("restart exit %d (want 0 after the DOWN retry):\n%s", code, out)
	}
	waitIdle(t, s)
	if n := paneCount(t, s); n != 2 || strings.Join(sessions(t, s), ",") != "boot" {
		t.Fatalf("after the retried start: %d panes, sessions %v (want the 2-pane boot session back)\n%s", n, sessions(t, s), out)
	}
}

// failingTmux is a tmux wrapper (a /bin/sh script in the server's scratch
// root) that fails the commands matching pattern while flag exists; with
// once, the first failure removes flag.
func failingTmux(t *testing.T, s *tmuxtest.Server, pattern string, once bool) (bin, flag string) {
	t.Helper()
	bin = filepath.Join(s.Root, "tmux-wrap")
	flag = filepath.Join(s.Root, "tmux-wrap.fail")
	rm := ""
	if once {
		rm = "rm -f '" + flag + "'; "
	}
	script := "#!/bin/sh\ncase \" $* \" in *'" + pattern + "'*) if [ -e '" + flag + "' ]; then " + rm +
		"echo 'wrapped failure' >&2; exit 1; fi ;; esac\nexec '" + s.T.Bin + "' \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, flag
}

// TestE2E_Restart_ListSessionsFailureLeavesServer (G2-H10): when the
// server answers but `list-sessions` fails, restart must not report a
// restart it never did: it aborts with the server untouched (same pid,
// same sessions, marker cleared, daemon back).
func TestE2E_Restart_ListSessionsFailureLeavesServer(t *testing.T) {
	s := startTarget(t)
	bin, flag := failingTmux(t, s, "list-sessions -F #{session_id} ", false)
	srvPid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}"))
	before := strings.Join(sessions(t, s), ",")
	restartHooks = func(step string) {
		if step == "saved" {
			if err := os.WriteFile(flag, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { restartHooks = nil }()
	code, out := restartInProcess(t, s, "MOOX_TMUX_BIN="+bin)
	_ = os.Remove(flag)
	if code == 0 || strings.Contains(out, "is back") || !strings.Contains(out, "wrapped failure") {
		t.Fatalf("restart with a failing list-sessions exited %d:\n%s", code, out)
	}
	if pid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}")); pid != srvPid {
		t.Fatalf("the server was restarted:\n%s", out)
	}
	if got := strings.Join(sessions(t, s), ","); got != before {
		t.Fatalf("sessions %s, want %s", got, before)
	}
	if v := s.Opt(t, "@moox-restoring"); v != "" {
		t.Fatalf("@moox-restoring left %q", v)
	}
}

// TestE2E_Restart_StartFailureRetries (G2-H11): the old server is gone when
// the new start fails; restart says the server is DOWN and names the final
// save, then tries a plain cold start, which brings it back.
func TestE2E_Restart_StartFailureRetries(t *testing.T) {
	s := startTarget(t)
	bin, flag := failingTmux(t, s, "new-session -d -s main ", true)
	restartHooks = func(step string) {
		if step == "killed" {
			if err := os.WriteFile(flag, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { restartHooks = nil }()
	code, out := restartInProcess(t, s, "MOOX_TMUX_BIN="+bin)
	if code != 0 || !strings.Contains(out, "DOWN") || !strings.Contains(out, "is back") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	waitIdle(t, s)
	if got := strings.Join(sessions(t, s), ","); got != "boot,work" {
		t.Fatalf("sessions after the retried start = %s\n%s", got, out)
	}
}
