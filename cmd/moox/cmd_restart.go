package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const restartUsage = `usage: moox restart [-L NAME | -S PATH] [--yes] [--force] [--snapshot FILE]`

// restartHooks are test seams (nil in production): called after each named
// step of an in-process restart.
var restartHooks func(step string)

func restartStep(s string) {
	if restartHooks != nil {
		restartHooks(s)
	}
}

// cmdRestart is `moox restart` (§8.5): save, end every session, start a cold
// server (the boot gate restores), with the user guarded (PR-H2).
func cmdRestart(m *meta, args []string) int {
	fs := flagSet(m, "restart", restartUsage)
	sf := addSockFlags(fs)
	yes := fs.Bool("yes", false, "no confirmation (required without a terminal)")
	force := fs.Bool("force", false, "restart even if the final save did not publish")
	snap := fs.String("snapshot", "", "come back with FILE (a pinned snapshot), not moox-last")
	detached := fs.Bool("detached", false, "internal: the re-exec'd process that survives the session kills")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := sf.check(); err != nil || fs.NArg() != 0 {
		fmt.Fprintf(m.stderr, "moox restart: %v\n%s\n", firstErr(err, fs.Args()), restartUsage)
		return exitUsage
	}
	t, err := m.target(sf)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restart: %v\n", err)
		return exitTmuxAbsent
	}
	if _, code := m.tmuxVersion(t, "restart"); code != exitOK {
		return code
	}
	e, err := m.engine(t)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restart: %v\n", err)
		return exitError
	}
	snapPath := ""
	if *snap != "" {
		if snapPath, err = saver.ResolveSnapshot(e, *snap, m.cwd); err != nil {
			fmt.Fprintf(m.stderr, "moox restart: --snapshot: %v\n", err)
			return exitInvalid
		}
	}
	if *detached {
		signal.Ignore(syscall.SIGHUP)
		return restartRun(m, t, e, *force, snapPath)
	}
	if !alive(m.ctx, t) {
		fmt.Fprintf(m.stderr, "moox restart: no server answers on %s\n", t.Socket)
		return exitNotApplic
	}

	// 1. Target and confirmation.
	if !*yes {
		if !m.stdinTTY {
			fmt.Fprintln(m.stderr, "moox restart: without a terminal, --yes is required")
			return exitUsage
		}
		sessions, panes := counts(m.ctx, t)
		if !confirmRestart(m.stdin, m.stderr, t.Socket, sockets.Legacy(t.Socket), sessions, panes) {
			fmt.Fprintln(m.stderr, "moox restart: aborted; nothing was changed")
			return exitAborted
		}
	}
	// 2. Run from a pane of the target: re-exec detached, so the process
	// survives the kill of its own session; it never attaches.
	if m.sameServer(t) {
		return restartDetach(m, t, e, *force, snapPath)
	}
	return restartRun(m, t, e, *force, snapPath)
}

func counts(ctx context.Context, t tmuxtarget.Target) (sessions, panes int) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := t.Run(cctx, "list-panes", "-a", "-F", "#{session_id}")
	if err != nil {
		return 0, 0
	}
	seen := map[string]bool{}
	for _, s := range strings.Fields(string(out)) {
		panes++
		seen[s] = true
	}
	return len(seen), panes
}

// confirmRestart is the TTY prompt (§8.5 step 1): it names the socket and
// what ends; the legacy default server additionally requires typing its
// name.
func confirmRestart(in io.Reader, out io.Writer, socket string, legacy bool, sessions, panes int) bool {
	name := filepath.Base(socket)
	fmt.Fprintf(out, "moox restart ends %d sessions / %d panes on server `%s` (%s), saves them first and restores them.\n",
		sessions, panes, name, socket)
	r := bufio.NewReader(in)
	if legacy {
		fmt.Fprintf(out, "This is your default tmux server. Type its name (%s) to continue: ", name)
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(line) == name
	}
	fmt.Fprint(out, "Continue? [y/N] ")
	line, _ := r.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func confirm(m *meta, prompt, want string) bool {
	fmt.Fprint(m.stderr, prompt)
	line, _ := bufio.NewReader(m.stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line)) == want
}

// restartDetach re-execs `moox restart --detached --yes …` in its own
// session (Setsid), SIGHUP ignored, stdio to log/restart.log; this process
// exits and the detached one does the work.
func restartDetach(m *meta, t tmuxtarget.Target, e *engine.Engine, force bool, snapPath string) int {
	logDir, err := statedir.Open(e.Paths().Log, filepath.Join(homeOf(m), ".claude"))
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restart: %v\n", err)
		return exitError
	}
	f, err := logDir.OpenAppend("restart.log")
	_ = logDir.Close()
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restart: %v\n", err)
		return exitError
	}
	defer func() { _ = f.Close() }()
	// Rebuilt from the parsed flags: the socket as a resolved path, the
	// snapshot as an absolute one.
	argv := []string{"restart", "--detached", "--yes", "-S", t.Socket}
	if force {
		argv = append(argv, "--force")
	}
	if snapPath != "" {
		argv = append(argv, "--snapshot", snapPath)
	}
	cmd := exec.Command(m.exe, argv...)
	cmd.Env = m.environ
	cmd.Dir = m.cwd
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(m.stderr, "moox restart: %v\n", err)
		return exitError
	}
	_ = cmd.Process.Release()
	fmt.Fprintf(m.stderr, "moox: restarting server %s; reattach with `moox` (log: %s)\n", filepath.Base(t.Socket),
		filepath.Join(e.Paths().Log, "restart.log"))
	return exitOK
}

func homeOf(m *meta) string {
	if r, err := m.roots(); err == nil {
		return r.Home
	}
	return m.env.Getenv("HOME")
}

// restartRun is §8.5 steps 3–6.
func restartRun(m *meta, t tmuxtarget.Target, e *engine.Engine, force bool, snapPath string) int {
	logf := func(format string, a ...any) {
		fmt.Fprintf(m.stderr, "%s moox restart: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
	pid := os.Getpid()
	value := fmt.Sprintf("shutdown:%d:%d", pid, time.Now().Unix())
	ctx := context.WithoutCancel(m.ctx) // a ^C must not leave a half-torn-down server
	run := func(args ...string) error {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := t.Run(cctx, args...)
		return err
	}
	// The bin dir, the excluded pane and the share root, read before anything
	// changes: a root that cannot be found must stop the restart while the
	// server is still untouched, not after its sessions are gone (G2-H11).
	bin, _ := option(ctx, t, "@moox-bin")
	exclude := ""
	if m.sameServer(t) {
		exclude = m.env.Getenv("TMUX_PANE")
	}
	root, err := m.rootDir()
	if err != nil {
		logf("%v; the server is untouched", err)
		return exitError
	}

	// 3. Shutdown marker BEFORE the save and the first kill: every other save
	// (daemon, --direct, freeze) now returns ErrShuttingDown. A live restore
	// or another restart owns the server until it ends: stopping the daemon
	// now would kill a restore mid-creation and let this final save publish
	// its half-built layout (G2-H1). The claim is a compare-and-set, so two
	// restarts cannot both take it.
	prior, err := option(ctx, t, "@moox-restoring")
	if err != nil {
		logf("reading @moox-restoring: %v", err)
		return exitError
	}
	if prior != "" {
		kind, owner, ok := engine.RestoringOwner(prior)
		switch {
		case !ok:
			logf("@moox-restoring %q does not parse (busy-restoring); `moox doctor --fix restoring` clears it; the server is untouched", prior)
			return exitBusy
		case pidLive(owner):
			logf("a %s (pid %d) owns the server; try again when it ends; the server is untouched", kind, owner)
			return exitBusy
		}
		// A dead owner: its marker may be replaced; its restoring-from is
		// pinned by the next gate, and abort puts the marker back.
	}
	if err := run("if-shell", "-F", "#{==:#{@moox-restoring},"+prior+"}", "set-option -g @moox-restoring "+value); err != nil {
		logf("setting @moox-restoring: %v", err)
		return exitError
	}
	if got, err := option(ctx, t, "@moox-restoring"); err != nil || got != value {
		// A failed read may still have left this restart's claim set: drop it,
		// conditionally, so it cannot block every later save (G2-2-L2).
		_ = run(engine.ClearRestoringArgs(value)...)
		logf("@moox-restoring changed meanwhile (%q): another restore or restart owns the server; the server is untouched", got)
		return exitBusy
	}
	restartStep("marked")
	abort := func(code int, why string) int {
		logf("%s; the server is untouched", why)
		if prior != "" {
			// hand the dead owner's marker back, so the next save still
			// recovers (pins) its restore
			_ = run("if-shell", "-F", "#{==:#{@moox-restoring},"+value+"}", "set-option -g @moox-restoring "+prior)
		} else {
			_ = run(engine.ClearRestoringArgs(value)...)
		}
		ensureDaemon(m, t, bin)
		return code
	}
	stopDaemon(m, t, bin)
	// The daemon's own marker writers are unconditional, so a restore it began
	// between the read-back above and the stop has replaced the claim
	// (G2-2-M2). Its marker is left as it is (abort's hand-back is conditional
	// on this restart's value), and so is the server.
	if got, err := option(ctx, t, "@moox-restoring"); err != nil || got != value {
		return abort(exitBusy, fmt.Sprintf("@moox-restoring changed while the daemon stopped (%q): a restore owns the server", got))
	}
	restartStep("daemon-stopped")

	// 4. The final save, in process (R3-H2), without this pane's own
	// recording (R2-H16).
	sctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	res, err := e.Save(sctx, engine.SaveRequest{Reason: "restart", Direct: true, Contents: true, ShutdownOwner: pid,
		ExcludePane: exclude})
	cancel()
	result := saver.ResultOf(res, err)
	logf("final save: %s (excluded pane %q)", result, exclude)
	word, _, _ := strings.Cut(result, " ")
	switch {
	case word == "published" || word == "unchanged" || word == "pristine":
	case word == "blocked" || word == "shutting-down" || word == "restoring":
		// --force overrides a failed or transient save, never the clobber
		// guard's refusal, another restart's ownership (G2-M7) or a live
		// restore's (G2-2-M2).
		return abort(exitError, "the final save was refused ("+result+"); see `moox snapshot show`")
	case !force:
		return abort(exitError, "the final save did not publish ("+result+"); use --force to restart anyway")
	}
	restartStep("saved")
	// A one-window workspace is SKIPPED:one-window by the cold boot gate
	// (a bootstrap-shaped snapshot), so an explicit restart restores it by
	// request instead, as --snapshot does (G2-H3).
	if snapPath == "" {
		if last, err := e.Last(); err == nil && len(last.Snapshot.Windows) <= 1 {
			snapPath = last.Path
			logf("a one-window workspace: %s is requested explicitly", snapPath)
		}
	}

	// 5. End every session; exit-empty ends the server. A server that does
	// not list its sessions but still answers is NOT restarted (G2-H10).
	cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := t.Run(cctx, "list-sessions", "-F", "#{session_id}")
	ccancel()
	switch {
	case err == nil:
		var cmds [][]string
		for _, id := range strings.Fields(string(out)) {
			cmds = append(cmds, []string{"kill-session", "-t", id})
		}
		if len(cmds) > 0 {
			if err := run(tmuxtarget.Chain(cmds...)...); err != nil {
				// A partial kill: the wait below falls through to kill-server (G2-2-L1).
				logf("ending the sessions: %v", err)
			}
		}
	case alive(ctx, t):
		return abort(exitError, "listing the sessions failed ("+err.Error()+")")
	default:
		logf("the server ended on its own after the final save (%v)", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	killed := false
	for alive(ctx, t) {
		if time.Now().After(deadline) {
			if killed {
				// It still answers, so it still holds this restart's claim and
				// has no daemon: give both back (G2-2-L2).
				_ = run(engine.ClearRestoringArgs(value)...)
				ensureDaemon(m, t, bin)
				logf("the server %s still answers after kill-server; not starting a second one", t.Socket)
				return exitError
			}
			logf("the server still answers 2 s after its sessions ended; kill-server")
			_ = run("kill-server")
			killed, deadline = true, time.Now().Add(2*time.Second)
		}
		pause(50 * time.Millisecond)
	}
	restartStep("killed")

	// 6. Start a cold server: the boot gate restores Last(); with --snapshot
	// the start hands over and FILE is requested instead (A16). The old
	// server is gone from here on: a failure says so, names the final save,
	// and tries once more as a plain cold start (G2-H11).
	extra := []string(nil)
	if snapPath != "" {
		extra = []string{"MOOX_NO_AUTO_RESTORE=1"}
	}
	// The retry is the SAME start: it keeps the handover env and the explicit
	// request (G2-2-M1). Dropping them made a one-window workspace come back
	// as a fresh bootstrap (SKIPPED:one-window) and replaced --snapshot FILE
	// by Last(), both reported "is back" with exit 0.
	sid, err := saver.StartServer(ctx, t, root, "main", saver.ServerEnv(m.environ, extra...))
	if err != nil {
		logf("starting the server failed (%v); the server is DOWN, the final save is %q; retrying the start once", err, res.File)
		if sid, err = saver.StartServer(ctx, t, root, "main", saver.ServerEnv(m.environ, extra...)); err != nil {
			hint := "`moox -S " + shArg(t.Socket) + "`"
			if snapPath != "" {
				hint += ", then `moox restore -S " + shArg(t.Socket) + " --snapshot " + shArg(snapPath) + "`"
			}
			logf("%v; start it with %s", err, hint)
			return exitError
		}
	}
	if snapPath == "" {
		target := waitBoot(m, t, sid)
		logf("server %s is back (session %s)", t.Socket, target)
		return exitOK
	}
	c := saver.Client{Target: t, Paths: e.Paths(), Root: root, Environ: m.environ, Session: "main"}
	if err := c.EnsureDaemon(ctx); err != nil {
		logf("%v", err)
		return restoreExit(err)
	}
	o, err := c.Request(ctx, snapPath, m.env.Getenv("MOOX_STUB_COMMANDS") == "1")
	if err != nil {
		logf("restoring %s: %v", snapPath, err)
		return restoreExit(err)
	}
	logf("server %s is back from %s: %s", t.Socket, snapPath, o.Result)
	fmt.Fprintln(m.stdout, o.Result)
	return exitOK
}

// pidLive reports whether pid names a running process (EPERM: it exists).
func pidLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// ttBin is <@moox-bin>/tt, else tt beside this binary.
func ttBin(m *meta, bin string) string {
	if bin != "" {
		return filepath.Join(bin, "tt")
	}
	if b := m.env.Getenv("MOOX_BIN"); b != "" {
		return filepath.Join(b, "tt")
	}
	return filepath.Join(filepath.Dir(m.exe), "tt")
}

func runTT(m *meta, t tmuxtarget.Target, bin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ttBin(m, bin), append(args, "--tmux-socket", t.Socket, "--tmux-bin", t.Bin)...)
	cmd.Env = saver.ServerEnv(m.environ)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", ttBin(m, bin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func stopDaemon(m *meta, t tmuxtarget.Target, bin string) {
	if err := runTT(m, t, bin, "serve", "--stop"); err != nil {
		fmt.Fprintf(m.stderr, "moox restart: %v\n", err)
	}
}

func ensureDaemon(m *meta, t tmuxtarget.Target, bin string) {
	_ = runTT(m, t, bin, "serve", "--ensure")
}

// pause is a one-shot wait (the §9.3a rule: one-shot, armed by the caller,
// self-disarming).
func pause(d time.Duration) {
	tm := time.NewTimer(d)
	<-tm.C
}
