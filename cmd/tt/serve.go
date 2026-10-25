package main

// serve.go is `tt serve` (moox §4.5, §3.3, §4.3): the per-server daemon and
// the config-time gate that starts it.
//
//	tt serve --ensure [--replace]  config-time: match, participation marker, env, fork daemon; exits 0 ALWAYS
//	tt serve --foreground          the daemon body: the observer + the server-lifetime waiter
//	tt serve --stop                corroborated SIGTERM, exactly as observe --stop
//	tt serve --status [--json]     pid, version, socket, saver state, last save, last restore
//
// --ensure runs inside moox.conf's SYNCHRONOUS, silent run-shell (§3.1), so
// it prints nothing and exits 0 on every path (RR12): a non-zero exit or a
// line of output from a config-time job is exactly what crashed tmux 3.7b.
// Its outcome is @moox-active (always written when the server answers) and
// daemon.log.
//
// The daemon hosts the observer and the saver (internal/saver, §5): its four
// waiters are the daemon's tie to the server (confirm-then-respawn, §4.5),
// its one loop is the only caller of Engine.Save and Engine.Restore in the
// process, and every restore — the boot restore (--foreground --boot, forked
// by the cold gate's BootGate, serve_boot.go) and the §6.4a requests — runs
// in that loop until its LAUNCH verdict.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
	"github.com/MadAppGang/moox/internal/observer/adapters/proclive"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const serveUsage = `usage: tt serve --ensure [--replace] | --foreground | --stop | --status [--json]

The per-server moox daemon. One runs for each tmux server whose socket name
matches @moox-sockets; it hosts the session observer (the socket "tt mcp", the
sidebar and "tt sessions" attach to) and lives exactly as long as its server.

Modes (exactly one):
  --ensure      what moox.conf runs at every config load. Matches the server
                against @moox-sockets, writes the participation marker and the
                MOOX_* environment, and starts the daemon if none of this
                version or newer is running. Prints nothing; always exits 0;
                the outcome is @moox-active and daemon.log
  --replace     with --ensure: replace a running daemon even if it is newer
  --foreground  the daemon itself, in this process (what --ensure starts)
  --stop        SIGTERM the daemon, after corroborating the pid is really it
  --status      report the daemon and the server's moox state
  --json        with --status: the machine-readable document

The server is --tmux-socket (global flag), else $TT_TMUX_SOCKET, else $TMUX's.
Logs: ~/.local/state/moox/sockets/<key>/daemon.log.
`

// The hook slots moox owns on an active server (§5.1); deactivation unsets
// every hook at these indices, whatever its name (§3.3).
const (
	slotFirstActive = 101
	slotLastActive  = 104
)

// Files in the per-socket moox state dir.
const (
	daemonLogName = "daemon.log"
	markerName    = "active"
	pinsName      = "pins"
	saverState    = "saver.state"
	vetoName      = "moox-excluded"
	lastPointer   = "moox-last"
)

// ensureBudget bounds the whole of --ensure: it runs inside a synchronous
// config-time job, so a wedged tmux must not wedge the config load.
const ensureBudget = 10 * time.Second

type serveFlags struct {
	ensure, foreground, stop, status bool
	replace, json                    bool
	// boot: with --foreground, the daemon the cold gate forked to
	// boot-restore (§6.4). Not in the usage: only --ensure passes it.
	boot bool
}

func runServe(args []string, stdout, stderr io.Writer) int {
	f, code, ok := parseServeFlags(args, stdout, stderr)
	if !ok {
		return code
	}
	switch {
	case f.ensure:
		runEnsure(f.replace)
		return jsonout.ExitOK
	case f.foreground:
		return runServeForeground(stderr, f.boot)
	case f.stop:
		dir := ttStateDir()
		if dir == "" {
			fmt.Fprintln(stderr, "tt serve: no state directory: no absolute $HOME")
			return jsonout.ExitNoRead
		}
		return stopServer("tt serve", dir, stderr)
	default:
		return runServeStatus(f.json, stdout, stderr)
	}
}

func parseServeFlags(args []string, stdout, stderr io.Writer) (serveFlags, int, bool) {
	fs := flag.NewFlagSet("tt serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	var f serveFlags
	fs.BoolVar(&f.ensure, "ensure", false, "config-time gate")
	fs.BoolVar(&f.foreground, "foreground", false, "the daemon body")
	fs.BoolVar(&f.stop, "stop", false, "stop the daemon")
	fs.BoolVar(&f.status, "status", false, "report")
	fs.BoolVar(&f.replace, "replace", false, "with --ensure: replace a running daemon")
	fs.BoolVar(&f.json, "json", false, "with --status: JSON")
	fs.BoolVar(&f.boot, "boot", false, "with --foreground: boot-restore (forked by the cold gate)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, serveUsage)
			return f, jsonout.ExitOK, false
		}
		fmt.Fprint(stderr, "\n"+serveUsage)
		return f, jsonout.ExitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tt serve: unexpected argument %q\n\n%s", fs.Arg(0), serveUsage)
		return f, jsonout.ExitUsage, false
	}
	n := 0
	for _, b := range []bool{f.ensure, f.foreground, f.stop, f.status} {
		if b {
			n++
		}
	}
	switch {
	case n != 1:
		fmt.Fprintf(stderr, "tt serve: give exactly one of --ensure, --foreground, --stop, --status\n\n%s", serveUsage)
		return f, jsonout.ExitUsage, false
	case f.replace && !f.ensure:
		fmt.Fprintf(stderr, "tt serve: --replace is only meaningful with --ensure\n\n%s", serveUsage)
		return f, jsonout.ExitUsage, false
	case f.json && !f.status:
		fmt.Fprintf(stderr, "tt serve: --json is only meaningful with --status\n\n%s", serveUsage)
		return f, jsonout.ExitUsage, false
	case f.boot && !f.foreground:
		fmt.Fprintf(stderr, "tt serve: --boot is only meaningful with --foreground\n\n%s", serveUsage)
		return f, jsonout.ExitUsage, false
	}
	return f, jsonout.ExitOK, true
}

// ---------------------------------------------------------------------------
// the gate: tt serve --ensure
// ---------------------------------------------------------------------------

// gate is one --ensure run against one server.
type gate struct {
	t      tmuxtarget.Target
	paths  sockets.Paths
	claude string
	log    io.Writer
	// lint is sockets.Lint of @moox-sockets, logged once the log is open.
	lint []error
}

// serverState is everything --ensure reads from the server, in ONE exec.
type serverState struct {
	socketPath  string // #{socket_path}, verbatim: what tmux puts in every pane's $TMUX
	setting     string // @moox-sockets
	active      string // @moox-active
	root, bin   string // @moox-root, @moox-bin
	configFiles string // #{config_files}
	sessions    int    // #{server_sessions}: 0 exactly on a cold start (E4)
}

const stateSep = "\x1f"

// readServerState is one display-message: it works on a server with no
// session yet (the cold-start gate sees 0 sessions, E4), and \x1f survives -u
// (PV11).
func readServerState(ctx context.Context, t tmuxtarget.Target) (serverState, error) {
	format := strings.Join([]string{"#{socket_path}", "#{@moox-sockets}", "#{@moox-active}",
		"#{@moox-root}", "#{@moox-bin}", "#{config_files}", "#{server_sessions}"}, stateSep)
	out, err := t.Run(ctx, "display-message", "-p", format)
	if err != nil {
		return serverState{}, err
	}
	f := strings.Split(strings.TrimRight(string(out), "\n"), stateSep)
	if len(f) != 7 {
		return serverState{}, fmt.Errorf("display-message returned %d fields, want 7: %q", len(f), out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(f[6]))
	if err != nil {
		return serverState{}, fmt.Errorf("display-message: #{server_sessions} %q: %w", f[6], err)
	}
	return serverState{socketPath: f[0], setting: f[1], active: f[2], root: f[3], bin: f[4], configFiles: f[5],
		sessions: n}, nil
}

// runEnsure is --ensure. It never returns an error and never prints: every
// outcome goes to daemon.log (when the server's state dir may exist) and to
// @moox-active.
func runEnsure(replace bool) {
	defer func() { _ = recover() }() // a config-time job must exit 0, even on a bug
	t := ttTarget()
	paths, ok := ttPaths()
	if !ok || t.Socket == "" {
		return
	}
	cp, err := claudePaths()
	if err != nil {
		return
	}
	g := &gate{t: t, paths: paths, claude: cp.ClaudeRoot, log: io.Discard}

	ctx, cancel := context.WithTimeout(context.Background(), ensureBudget)
	defer cancel()
	st, err := readServerState(ctx, t)
	if err != nil {
		g.openExistingLog()
		g.logf("ENSURE-UNREACHABLE socket=%s: %v", t.Socket, err)
		return
	}
	g.lint = sockets.Lint(st.setting)
	if sockets.Match(sockets.Name(st.socketPath), st.setting) {
		g.activate(ctx, st, replace)
		return
	}
	g.exclude(ctx, st)
}

func (g *gate) logLint(setting string) {
	for _, err := range g.lint {
		g.logf("ENSURE-LINT @moox-sockets %q: %v", setting, err)
	}
}

func (g *gate) logf(format string, a ...any) {
	fmt.Fprintf(g.log, "%s pid=%d %s\n", time.Now().UTC().Format(time.RFC3339), os.Getpid(), fmt.Sprintf(format, a...))
}

// openLog creates the per-socket state dir and appends to its daemon.log.
func (g *gate) openLog() *statedir.Dir {
	d, err := statedir.Open(g.paths.State, g.claude)
	if err != nil {
		return nil
	}
	if f, err := d.OpenAppend(daemonLogName); err == nil {
		g.log = f
	}
	return d
}

// openExistingLog logs only into a state dir that already exists: a server
// that does not take part gets no files under its key (§12.4).
func (g *gate) openExistingLog() *statedir.Dir {
	d, err := statedir.OpenExisting(g.paths.State, g.claude)
	if err != nil {
		return nil
	}
	if f, err := d.OpenAppend(daemonLogName); err == nil {
		g.log = f
	}
	return d
}

// activate is the match branch (§4.3). Order is load-bearing: the veto goes
// and the marker exists BEFORE the env says MOOX_ACTIVE 1, so a shell that
// reads the env can rely on the files (§3.3, R3-C4).
func (g *gate) activate(ctx context.Context, st serverState, replace bool) {
	state := g.openLog()
	if state == nil {
		return
	}
	defer func() { _ = state.Close() }()
	g.logf("ENSURE-MATCH socket=%s key=%s setting=%q", st.socketPath, g.paths.Key, st.setting)
	g.logLint(st.setting)

	if g.paths.Legacy {
		g.removeVeto()
	}
	if err := state.WriteFile(markerName, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		g.logf("MARKER-FAILED %s: %v", state.Join(markerName), err)
	}
	g.tightenContinuityDirs()

	is38 := "0"
	if v, err := g.t.Version(ctx); err == nil && v.AtLeast(3, 8) {
		is38 = "1"
	}
	root, bin := g.rootAndBin(st)
	logDir := g.paths.Log
	env := func(k, v string) []string { return []string{"set-environment", "-g", k, v} }
	opt := func(k, v string) []string { return []string{"set-option", "-g", k, v} }
	cmds := [][]string{
		env("MOOX_SOCKET", st.socketPath),
		env("MOOX_ACTIVE", "1"),
		env("MOOX_STATE_KEY", g.paths.Key),
		env("MOOX_PANES_DIR", g.paths.Panes),
		env("MOOX_LAUNCH_DIR", g.paths.Launch),
		env("MOOX_PENDING_DIR", g.paths.Pending),
		env("MOOX_STATE_DIR", g.paths.State),
	}
	if root != "" {
		cmds = append(cmds, env("MOOX_ROOT", root))
	}
	if bin != "" {
		cmds = append(cmds, env("MOOX_BIN", bin))
	}
	cmds = append(cmds,
		opt("@claude-continuity-launch-dir", g.paths.Launch),
		opt("@claude-continuity-pending-dir", g.paths.Pending),
		opt("@claude-continuity-panes-dir", g.paths.Panes),
		opt("@claude-continuity-log-file", filepath.Join(logDir, "continuity.log")),
		opt("@moox-log-dir", logDir),
		opt("@moox-tmux-38", is38),
		opt("@moox-warning", g.pinsWarning(state)),
		opt("@moox-active", "1"),
	)
	if rb := reloadBinding(st.configFiles); rb != nil {
		cmds = append(cmds, rb)
	}
	if _, err := g.t.Run(ctx, tmuxtarget.Chain(cmds...)...); err != nil {
		g.logf("ENSURE-ENV-FAILED: %v", err)
		return
	}
	// The daemon is forked on every match (L3); BootGate decides, before any
	// session exists, whether it boot-restores (§6.4).
	boot := g.bootGate(ctx, st.sessions)
	g.ensureDaemon(ctx, replace, boot)
}

// rootAndBin are MOOX_ROOT and MOOX_BIN: the options moox.conf set, else what
// this binary can tell about itself.
func (g *gate) rootAndBin(st serverState) (string, string) {
	root, bin := st.root, st.bin
	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
	}
	if bin == "" && exe != "" {
		bin = filepath.Dir(exe)
	}
	if root == "" && exe != "" {
		wd, _ := os.Getwd()
		if r, err := mooxroot.Stable(exe, wd); err == nil {
			root = r
		}
	}
	if bin != "" {
		bin = filepath.Clean(bin)
	}
	return root, bin
}

// reloadBinding re-binds prefix r when #{config_files} is a comma-joined list
// (two -f files): that string is no path, so keys.conf's one-path form cannot
// reload it, but source-file takes several paths (§3.1, Q7). nil when the list
// holds one file. The ";" between the bound commands is escaped by Chain.
func reloadBinding(configFiles string) []string {
	if !strings.Contains(configFiles, ",") {
		return nil
	}
	cmd := []string{"bind-key", "r", "source-file"}
	for _, f := range strings.Split(configFiles, ",") {
		if f = strings.TrimSpace(f); f != "" {
			cmd = append(cmd, f)
		}
	}
	return append(cmd, ";", "display-message", "moox: reloaded")
}

// pinsWarning is @moox-warning (A16): re-set at every config load from the
// pins file, "" when nothing is pinned.
func (g *gate) pinsWarning(state *statedir.Dir) string {
	data, _, err := state.ReadFile(pinsName)
	if err != nil {
		return ""
	}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("restore INCOMPLETE: %d pinned — moox snapshot show", n)
}

func (g *gate) removeVeto() {
	d, err := statedir.OpenExisting(filepath.Dir(g.paths.Veto), g.claude)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	if err := d.Remove(vetoName, statedir.TypeRegular, nil); err == nil {
		g.logf("VETO-REMOVED %s", g.paths.Veto)
	} else if !errors.Is(err, fs.ErrNotExist) {
		g.logf("VETO-REMOVE-FAILED %s: %v", g.paths.Veto, err)
	}
}

func (g *gate) writeVeto() {
	d, err := statedir.Open(filepath.Dir(g.paths.Veto), g.claude)
	if err != nil {
		g.logf("VETO-FAILED %s: %v", g.paths.Veto, err)
		return
	}
	defer func() { _ = d.Close() }()
	if err := d.WriteFile(vetoName, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		g.logf("VETO-FAILED %s: %v", g.paths.Veto, err)
		return
	}
	g.logf("VETO-WRITTEN %s", g.paths.Veto)
}

// tightenContinuityDirs makes every continuity directory the gate hands to
// the shell 0700 (§4.2, PR-M10): the per-key ones are created so, and the
// legacy ~/.config/tmux-claude/{launch,pending,panes} — shared with the
// pre-moox pipeline — are tightened if they exist and are more permissive. A
// mode change is not a data migration (D7 holds); a 0644 file the shell wrote
// under the user's umask is unreadable to others inside a 0700 directory.
func (g *gate) tightenContinuityDirs() {
	cc := filepath.Dir(g.paths.Veto)
	type dir struct {
		path   string
		create bool
	}
	dirs := []dir{
		{g.paths.State, true}, {g.paths.Log, true},
		{g.paths.Launch, true}, {g.paths.Pending, true}, {g.paths.Panes, true},
		{filepath.Join(cc, "launch"), false}, {filepath.Join(cc, "pending"), false}, {filepath.Join(cc, "panes"), false},
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d.path] {
			continue
		}
		seen[d.path] = true
		open := statedir.OpenExisting
		if d.create {
			open = statedir.Open
		}
		h, err := open(d.path, g.claude)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				g.logf("MODE-CHECK-FAILED %s: %v", d.path, err)
			}
			continue
		}
		changed, err := h.TightenToOwner()
		_ = h.Close()
		switch {
		case err != nil:
			g.logf("MODE-TIGHTEN-FAILED %s: %v", d.path, err)
		case changed:
			g.logf("MODE-TIGHTENED %s", d.path)
		}
	}
}

// exclude is the no-match branch (§4.3, §3.3).
func (g *gate) exclude(ctx context.Context, st serverState) {
	state := g.openExistingLog()
	if state != nil {
		defer func() { _ = state.Close() }()
	}
	wasActive := st.active == "1"
	g.logf("ENSURE-NO-MATCH socket=%s key=%s setting=%q was-active=%v", st.socketPath, g.paths.Key, st.setting, wasActive)
	g.logLint(st.setting)

	// The veto first (R3-C4): a shell with no MOOX_* env stops writing the
	// moment it exists, before anything else is torn down.
	if g.paths.Legacy {
		g.writeVeto()
	}
	// The marker next (R2-C5): every already-running shell checks it at the
	// moment it writes.
	if state != nil {
		if err := state.Remove(markerName, statedir.TypeRegular, nil); err == nil {
			g.logf("MARKER-REMOVED %s", state.Join(markerName))
		} else if !errors.Is(err, fs.ErrNotExist) {
			g.logf("MARKER-REMOVE-FAILED %s: %v", state.Join(markerName), err)
		}
	}

	cmds := [][]string{
		{"set-environment", "-g", "MOOX_SOCKET", st.socketPath},
		{"set-environment", "-g", "MOOX_ACTIVE", "0"},
		{"set-option", "-g", "@moox-active", "0"},
	}
	if wasActive {
		g.stopDaemon()
		for _, h := range g.activeHookSlots(ctx) {
			cmds = append(cmds, []string{"set-hook", "-gu", h})
		}
		g.logf("DEACTIVATED: daemon stopped, hook slots %d-%d unset, C-s and Z unbound", slotFirstActive, slotLastActive)
	}
	if _, err := g.t.Run(ctx, tmuxtarget.Chain(cmds...)...); err != nil {
		g.logf("ENSURE-ENV-FAILED: %v", err)
	}
	if wasActive {
		// Separately, and last: an unbind that failed must not have stopped
		// the chain above.
		if _, err := g.t.Run(ctx, tmuxtarget.Chain(
			[]string{"unbind-key", "-q", "C-s"},
			[]string{"unbind-key", "-q", "Z"},
		)...); err != nil {
			g.logf("UNBIND-FAILED: %v", err)
		}
	}
}

// activeHookSlots lists every global hook entry at moox's active slots
// (101-104), as `name[i]`, from the server's own show-hooks -g — so the
// teardown removes exactly what is registered, whatever names active.conf and
// tt.conf use.
func (g *gate) activeHookSlots(ctx context.Context) []string {
	out, err := g.t.Run(ctx, "show-hooks", "-g")
	if err != nil {
		g.logf("SHOW-HOOKS-FAILED: %v", err)
		return nil
	}
	return activeSlotsIn(string(out))
}

func activeSlotsIn(showHooks string) []string {
	var slots []string
	seen := map[string]bool{}
	for _, line := range strings.Split(showHooks, "\n") {
		word, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		open := strings.IndexByte(word, '[')
		if open <= 0 || !strings.HasSuffix(word, "]") {
			continue
		}
		i, err := strconv.Atoi(word[open+1 : len(word)-1])
		if err != nil || i < slotFirstActive || i > slotLastActive || seen[word] {
			continue
		}
		seen[word] = true
		slots = append(slots, word)
	}
	return slots
}

// ensureDaemon keeps one daemon per server (§3.3's handshake): an equal or
// newer one stays (an older ensuring binary logs VERSION-SKEW); an older one,
// or any with --replace, is stopped (corroborated SIGTERM) and replaced.
func (g *gate) ensureDaemon(ctx context.Context, replace, boot bool) {
	mine := observer.ResolveVersion()
	if rec, running := daemonRunning(g.paths.TTState); running {
		c := mooxroot.CompareVersions(mine, rec.TT)
		kept := false
		switch {
		case !hasSaver(g.paths, rec):
			// A plain `tt observe` (the sidebar's fallback) answers on the
			// same socket but never saves: keeping it would leave an active
			// server with no autosave and unanswered requests (G2-H5).
			g.logf("DAEMON-REPLACE pid=%d is an observer without a saver", rec.PID)
		case !replace && c == 0:
			g.logf("DAEMON-KEPT pid=%d version=%s", rec.PID, rec.TT)
			kept = true
		case !replace && c < 0:
			g.logf("VERSION-SKEW running daemon pid=%d is %s, this binary is %s: kept", rec.PID, rec.TT, mine)
			kept = true
		}
		if kept {
			if boot {
				g.logf("BOOT-SKIPPED a daemon of this server already runs; it does not boot-restore")
				_, _ = g.t.Run(ctx, "set-option", "-g", "@moox-boot-result", engine.BootSkippedPrefix+"daemon-kept")
			}
			return
		}
		g.logf("DAEMON-REPLACE pid=%d version=%s by %s (replace=%v)", rec.PID, rec.TT, mine, replace)
		g.stopDaemon()
	}
	if pid := g.forkDaemon(boot); pid > 0 && boot {
		g.markBootRestore(ctx, pid)
	}
}

// hasSaver: the observer of rec is the moox daemon. The pid file's role
// decides (G2-2-H1): `tt serve --foreground` writes wire.RoleServe, a plain
// `tt observe` wire.RoleObserve, so a fresh plain observer is never taken
// for a daemon whose saver has not written saver.state yet (the 15 s age
// grace that stood in for the role did exactly that), and a daemon whose
// saver.state write failed is not replaced by every --ensure (G2-2-M4).
// A role-less pid file (a binary from before roles) is the daemon only when
// saver.state names its pid.
func hasSaver(p sockets.Paths, rec wire.PidFile) bool {
	switch rec.Role {
	case wire.RoleServe:
		return true
	case wire.RoleObserve:
		return false
	}
	return saver.Running(p)
}

// daemonRunning reads the observer pid file in the per-socket tt dir and
// reports a daemon only when that pid is alive AND the socket answers: a pid
// file survives a crash, and pids are recycled.
func daemonRunning(dir string) (wire.PidFile, bool) {
	data, err := os.ReadFile(filepath.Join(dir, sockserve.PidName))
	if err != nil {
		return wire.PidFile{}, false
	}
	rec, err := wire.DecodePidFile(data)
	if err != nil {
		return wire.PidFile{}, false
	}
	if alive, err := proclive.New().Alive(rec.PID); err != nil || !alive {
		return rec, false
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, sockserve.SocketName), time.Second)
	if err != nil {
		return rec, false
	}
	_ = conn.Close()
	return rec, true
}

// stopDaemon is the corroborated SIGTERM, then a wait of at most 2 s for the
// pid file to go (§3.3). An uncorroborated pid is refused and logged.
func (g *gate) stopDaemon() {
	out, err := sockserve.StopIn(sockserve.StopConfig{Dir: g.paths.TTState, ClaudeRoot: g.claude})
	switch {
	case err != nil:
		g.logf("DAEMON-STOP-FAILED: %v", err)
		return
	case out.Refused:
		g.logf("DAEMON-STOP-REFUSED pid=%d: %s", out.PID, out.Reason)
		return
	case !out.Signalled:
		return
	}
	g.logf("DAEMON-STOPPED pid=%d (corroborated by %s)", out.PID, out.Corroborated)
	pid := filepath.Join(g.paths.TTState, sockserve.PidName)
	if !pollUntil(2*time.Second, func() bool { _, err := os.Lstat(pid); return errors.Is(err, fs.ErrNotExist) }) {
		g.logf("DAEMON-STOP-SLOW pid file %s still present after 2s", pid)
	}
}

// forkDaemon starts `tt serve --foreground` detached: its own session (Setsid,
// so a SIGHUP to the config's job does not reach it), stdin /dev/null, stdout
// and stderr appended to daemon.log, and no $TMUX — it talks to its server
// through -S only.
func (g *gate) forkDaemon(boot bool) int {
	exe, err := os.Executable()
	if err != nil {
		g.logf("DAEMON-FORK-FAILED: %v", err)
		return 0
	}
	state, err := statedir.Open(g.paths.State, g.claude)
	if err != nil {
		g.logf("DAEMON-FORK-FAILED: %v", err)
		return 0
	}
	defer func() { _ = state.Close() }()
	logf, err := state.OpenAppend(daemonLogName)
	if err != nil {
		g.logf("DAEMON-FORK-FAILED: %v", err)
		return 0
	}
	defer func() { _ = logf.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		g.logf("DAEMON-FORK-FAILED: %v", err)
		return 0
	}
	defer func() { _ = devnull.Close() }()

	args := []string{"serve", "--foreground", "--tmux-socket", g.t.Socket, "--tmux-bin", g.t.Bin}
	if boot {
		args = append(args, "--boot")
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		g.logf("DAEMON-FORK-FAILED: %v", err)
		return 0
	}
	pid := cmd.Process.Pid
	g.logf("DAEMON-FORKED pid=%d boot=%v", pid, boot)
	_ = cmd.Process.Release()
	return pid
}

func envWithout(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			if k == n {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// pollUntil polls cond every 50 ms until it holds or within passes. Each wait is
// a one-shot timer armed by the poll before it (tt §9.3a: one-shot,
// event-armed, self-disarming, never armed while idle — the loop ends).
func pollUntil(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		t := time.NewTimer(50 * time.Millisecond)
		<-t.C
	}
}

// ---------------------------------------------------------------------------
// the daemon: tt serve --foreground
// ---------------------------------------------------------------------------

// runServeForeground is the daemon: the observer (the socket tt mcp, the
// sidebar and tt sessions attach to) and the saver (§5), whose four waiters
// are also the daemon's tie to its server — the saver returns when one of
// them confirms the server gone (§4.5 Lifetime). With --boot the saver
// starts with the boot restore (§6.4), so EVERY restore runs in the saver's
// loop (A3).
func runServeForeground(stderr io.Writer, boot bool) int {
	t := ttTarget()
	paths, ok := ttPaths()
	if !ok || t.Socket == "" {
		fmt.Fprintln(stderr, "tt serve: no state layout: no absolute $HOME or no socket")
		return jsonout.ExitNoRead
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(stderr, "%s pid=%d %s\n", time.Now().UTC().Format(time.RFC3339), os.Getpid(), fmt.Sprintf(format, a...))
	}
	logf("SERVE-START version=%s socket=%s key=%s boot=%v", observer.ResolveVersion(), t.Socket, paths.Key, boot)
	cp, err := claudePaths()
	if err != nil {
		fmt.Fprintf(stderr, "tt serve: %v\n", err)
		return jsonout.ExitNoRead
	}
	e, err := newEngine(t, cp.ClaudeRoot, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "tt serve: engine: %v\n", err)
		return jsonout.ExitNoRead
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	obsDone := make(chan int, 1)
	go func() {
		obsDone <- serveObserve(ctx, observeFlags{
			Dir:        paths.TTState,
			Timeout:    5 * time.Second,
			StatusLine: statusLineAuto,
			Role:       wire.RoleServe,
		}, stderr)
	}()

	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	timing := saver.ReadTiming(tctx, t)
	tcancel()
	logf("SAVER-START debounce=%s max-wait=%s backup=%s", timing.Debounce, timing.MaxWait, timing.Backup)
	cfg := saver.Config{Target: t, Engine: e, Debounce: timing.Debounce, MaxWait: timing.MaxWait, Backup: timing.Backup,
		Log: stderr, OnState: saverStateWriter(paths.State, cp.ClaudeRoot, logf)}
	if boot {
		cfg.Boot = &saver.Boot{Stub: os.Getenv(envStub) == "1"}
	}
	saverDone := make(chan error, 1)
	go func() { saverDone <- saver.Run(ctx, cfg) }()

	select {
	case code := <-obsDone:
		// The observer ended first: another daemon holds the socket (exit 0),
		// a start failure, or a signal.
		cancel()
		<-saverDone
		logf("SERVE-EXIT observer ended (code %d)", code)
		return code
	case err := <-saverDone:
		cancel()
		<-obsDone
		switch {
		case errors.Is(err, saver.ErrServerGone):
			// "unsets @moox_status if it still can": it cannot, the server is
			// gone. The pid file and socket are removed by the observer's own
			// Close.
			logf("SERVE-EXIT server gone")
		case err == nil:
			bestEffort(t, "set-option", "-gu", "@moox_status")
			logf("SERVE-EXIT stopped")
		default:
			bestEffort(t, "set-option", "-gu", "@moox_status")
			logf("SERVE-EXIT %v", err)
		}
		return jsonout.ExitOK
	}
}

// saverStateWriter writes saver.state ("<pid> <state> <time>"), which
// `tt serve --status` shows and `moox save` reads to know that a SAVER (not
// a bare `tt observe`) holds this server's observer socket. A failed write
// is logged once per failure streak (G2-2-M4): `--ensure` no longer depends
// on it (the pid file's role does), but `moox save` and `--status` do.
func saverStateWriter(dir, claudeRoot string, logf func(string, ...any)) func(string) {
	pid := strconv.Itoa(os.Getpid())
	failing := false
	return func(state string) {
		err := func() error {
			d, err := statedir.Open(dir, claudeRoot)
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()
			return d.WriteFile(saverState, []byte(pid+" "+state+" "+time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
		}()
		switch {
		case err != nil && !failing:
			logf("SAVER-STATE-WRITE-FAILED state=%s: %v", state, err)
		case err == nil && failing:
			logf("SAVER-STATE-WRITE-OK state=%s", state)
		}
		failing = err != nil
	}
}

func bestEffort(t tmuxtarget.Target, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = t.Run(ctx, args...)
}

// observeLifeChannel is the channel a standalone `tt observe` blocks on to
// learn that its server is gone (F1). Nobody signals it: the waiter returns
// only when tmux flushes its waiters on the way down or the server vanishes.
// It is NOT moox-dirty, whose signals belong to the daemon's waiter.
const observeLifeChannel = "tt-observe-life"

// observeTied reports whether a server-mode `tt observe` is a per-server
// process: its tmux server was named explicitly (--tmux-socket,
// $TT_TMUX_SOCKET, or $TMUX). Only then does it exit with that server (D8);
// a bare per-user `tt observe` outside tmux keeps today's lifetime.
func observeTied() bool {
	return currentGlobals().socket != "" || os.Getenv("TT_TMUX_SOCKET") != "" || os.Getenv("TMUX") != ""
}

// tieToServer runs the lifetime watcher of a standalone observer and
// cancels it when its server is gone (or the watcher gave up).
func tieToServer(ctx context.Context, cancel context.CancelFunc, t tmuxtarget.Target, stderr io.Writer) {
	logf := func(format string, a ...any) {
		fmt.Fprintf(stderr, "%s pid=%d %s\n", time.Now().UTC().Format(time.RFC3339), os.Getpid(), fmt.Sprintf(format, a...))
	}
	go func() {
		err := saver.Watcher{Target: t, Channel: observeLifeChannel, Logf: logf}.Run(ctx)
		if ctx.Err() == nil {
			logf("OBSERVE-EXIT %v", err)
			cancel()
		}
	}()
}

// ---------------------------------------------------------------------------
// tt serve --status
// ---------------------------------------------------------------------------

func runServeStatus(asJSON bool, stdout, stderr io.Writer) int {
	t := ttTarget()
	paths, ok := ttPaths()
	if !ok {
		fmt.Fprintln(stderr, "tt serve: no state layout: no absolute $HOME")
		return jsonout.ExitNoRead
	}
	st := wire.ServeStatus{Socket: t.Socket, Key: paths.Key, StateDir: paths.State, TTState: paths.TTState}
	if rec, running := daemonRunning(paths.TTState); running {
		st.Running = true
		pid, v, at := rec.PID, rec.TT, rec.StartedAt
		st.PID, st.Version, st.StartedAt = &pid, &v, &at
	}
	if _, err := os.Lstat(paths.Marker); err == nil {
		st.Participating = true
	}
	st.SaverState = readTrimmed(filepath.Join(paths.State, saverState))
	st.LastSave = readTrimmed(filepath.Join(paths.Snapshots, lastPointer))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := t.Run(ctx, "display-message", "-p", "#{@moox-active}"+stateSep+"#{@moox-restore-result}"); err == nil {
		f := strings.SplitN(strings.TrimRight(string(out), "\n"), stateSep, 2)
		if len(f) == 2 {
			if f[0] != "" {
				st.Active = &f[0]
			}
			if f[1] != "" {
				st.LastRestore = &f[1]
			}
		}
	}
	if asJSON {
		b, err := wire.EncodeServeStatus(st)
		if err != nil {
			fmt.Fprintf(stderr, "tt: %v\n", err)
			return jsonout.ExitNoRead
		}
		_, _ = stdout.Write(b)
		return jsonout.ExitOK
	}
	show := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	daemon := "not running"
	if st.Running {
		daemon = fmt.Sprintf("running (pid %d, %s, since %s)", *st.PID, *st.Version, *st.StartedAt)
	}
	fmt.Fprintf(stdout, "socket:        %s\nkey:           %s\ndaemon:        %s\nactive:        %s\nparticipating: %v\n"+
		"saver:         %s\nlast save:     %s\nlast restore:  %s\nstate:         %s\n",
		st.Socket, st.Key, daemon, show(st.Active), st.Participating,
		show(st.SaverState), show(st.LastSave), show(st.LastRestore), st.StateDir)
	return jsonout.ExitOK
}

func readTrimmed(path string) *string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil
	}
	return &s
}
