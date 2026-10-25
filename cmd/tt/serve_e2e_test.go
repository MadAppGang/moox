//go:build tmuxe2e

package main

// moox P2's isolated-server gate: tt per server (§4.5, §3.3, V7). Every server
// here is a tmuxtest server (-f /dev/null -L moox-test-*/moox-val-*, /bin/sh
// panes, scratch HOME/XDG); every tt process runs the binary built from this
// tree with an EXPLICIT environment — scratch HOME, XDG_*, CLAUDE_CONFIG_DIR,
// and a PATH whose `claude` is a stub that fails — and is pointed at its
// server with --tmux-socket. Nothing here can reach the user's default server
// or real state: the default socket is never named, and HOME is scratch.
//
// Run: go test -tags tmuxe2e -run 'TestE2E_' ./cmd/tt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

// ttBinaries builds this tree's tt twice, once: unstamped (0.0.0-dev) and
// stamped 99.0.0, so the handshake can be shown to replace an older daemon.
func ttBinaries(t *testing.T) (dev, newer string) {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "ttbin")
		if buildErr != nil {
			return
		}
		for _, b := range []struct{ out, ldflags string }{
			{"tt", ""},
			{"tt-99", "-X " + mooxroot.ModulePath + "/internal/mooxroot.Version=99.0.0"},
		} {
			cmd := exec.Command("go", "build", "-ldflags", b.ldflags, "-o", filepath.Join(buildDir, b.out), ".")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("go build %s: %v: %s", b.out, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(buildDir, "tt"), filepath.Join(buildDir, "tt-99")
}

// world is one scratch user: HOME, a SHORT state root (a unix socket path must
// stay under 104 bytes, and t.TempDir() embeds the test name), and a PATH
// whose claude is a stub.
type world struct {
	t                  *testing.T
	home, xs, xd, xc   string
	claude, stubBin    string
	extraEnv           []string
	started            []int // daemon pids seen, SIGTERMed at cleanup if still alive
	startedMu          sync.Mutex
	defaultSocketGuard string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	short, err := os.MkdirTemp("/tmp", "mx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	w := &world{t: t, home: filepath.Join(t.TempDir(), "home"), xs: filepath.Join(short, "xs"),
		xd: filepath.Join(t.TempDir(), "xd"), xc: filepath.Join(t.TempDir(), "xc"), stubBin: filepath.Join(short, "bin")}
	w.claude = filepath.Join(w.home, ".claude")
	for _, d := range []string{w.home, w.xs, w.xd, w.xc, w.stubBin, filepath.Join(w.claude, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// claude agents --json must never reach a real CLI.
	if err := os.WriteFile(filepath.Join(w.stubBin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	w.defaultSocketGuard, _ = tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, "default")
	t.Cleanup(w.reapDaemons)
	return w
}

func (w *world) env() []string {
	env := []string{
		"HOME=" + w.home, "XDG_STATE_HOME=" + w.xs, "XDG_DATA_HOME=" + w.xd, "XDG_CONFIG_HOME=" + w.xc,
		"CLAUDE_CONFIG_DIR=" + w.claude, "PATH=" + w.stubBin + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C", "TERM=dumb",
	}
	return append(env, w.extraEnv...)
}

func (w *world) paths(s *tmuxtest.Server) sockets.Paths {
	w.t.Helper()
	p, err := sockets.For(s.T.Socket, tmuxtarget.MapEnv{"HOME": w.home, "XDG_STATE_HOME": w.xs,
		"XDG_DATA_HOME": w.xd, "XDG_CONFIG_HOME": w.xc})
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

type ttResult struct {
	stdout, stderr string
	code           int
}

// tt runs one tt process to completion, bounded.
func (w *world) tt(bin string, args ...string) ttResult {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = w.env()
	cmd.Dir = w.home
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		w.t.Fatalf("running %s %q: %v", bin, args, err)
	}
	return ttResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// ensure is what moox.conf's gate runs: it must exit 0 and print nothing.
func (w *world) ensure(bin string, s *tmuxtest.Server, extra ...string) {
	w.t.Helper()
	r := w.tt(bin, append([]string{"serve", "--ensure", "--tmux-socket", s.T.Socket, "--tmux-bin", s.T.Bin}, extra...)...)
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		w.t.Fatalf("tt serve --ensure: exit %d, stdout %q, stderr %q; it must exit 0 and print nothing (RR12)",
			r.code, r.stdout, r.stderr)
	}
}

// daemon waits for the server's daemon pid file to name a live pid other than
// not, and returns its record.
func (w *world) daemon(s *tmuxtest.Server, not int) wire.PidFile {
	w.t.Helper()
	dir := w.paths(s).TTState
	var rec wire.PidFile
	ok := tmuxtest.Poll(10*time.Second, 50*time.Millisecond, func() bool {
		r, running := daemonRunning(dir)
		if running && r.PID != not {
			rec = r
			return true
		}
		return false
	})
	if !ok {
		w.t.Fatalf("no daemon came up in %s within 10s; daemon.log:\n%s", dir, w.log(s))
	}
	w.startedMu.Lock()
	w.started = append(w.started, rec.PID)
	w.startedMu.Unlock()
	return rec
}

func (w *world) log(s *tmuxtest.Server) string {
	b, err := os.ReadFile(filepath.Join(w.paths(s).State, daemonLogName))
	if err != nil {
		return "(no daemon.log: " + err.Error() + ")"
	}
	return string(b)
}

// reapDaemons SIGTERMs any daemon this test started that outlived its server
// (a failure path); a healthy run finds them all gone already. Only pids this
// test saw in its own scratch pid files are signalled.
func (w *world) reapDaemons() {
	w.startedMu.Lock()
	defer w.startedMu.Unlock()
	for _, pid := range w.started {
		if syscall.Kill(pid, 0) == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func startServer(t *testing.T, name string) *tmuxtest.Server {
	t.Helper()
	opts := []tmuxtest.Option{}
	if name != "" {
		opts = append(opts, tmuxtest.WithName(fmt.Sprintf("%s-%d", name, os.Getpid())))
	}
	s := tmuxtest.Start(t, opts...)
	s.Run(t, "set-option", "-g", "@moox-sockets", s.Name)
	return s
}

func envOf(t *testing.T, s *tmuxtest.Server, name string) string {
	t.Helper()
	out, err := s.RunErr("show-environment", "-g", name)
	if err != nil {
		return ""
	}
	_, v, _ := strings.Cut(strings.TrimSpace(out), "=")
	return v
}

// ---------------------------------------------------------------------------
// the gate and the daemon
// ---------------------------------------------------------------------------

func TestE2E_Serve_EnsureIdempotentAndUpgrades(t *testing.T) {
	dev, newer := ttBinaries(t)
	w := newWorld(t)
	s := startServer(t, "")
	p := w.paths(s)

	w.ensure(dev, s)
	if got := s.Opt(t, "@moox-active"); got != "1" {
		t.Fatalf("@moox-active = %q after a matching ensure, want 1; daemon.log:\n%s", got, w.log(s))
	}
	if got := envOf(t, s, "MOOX_SOCKET"); got != s.T.Socket {
		t.Fatalf("MOOX_SOCKET = %q, want the server's own #{socket_path} %q", got, s.T.Socket)
	}
	for k, want := range map[string]string{
		"MOOX_ACTIVE": "1", "MOOX_STATE_KEY": p.Key, "MOOX_STATE_DIR": p.State,
		"MOOX_LAUNCH_DIR": p.Launch, "MOOX_PENDING_DIR": p.Pending, "MOOX_PANES_DIR": p.Panes,
	} {
		if got := envOf(t, s, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for k, want := range map[string]string{
		"@claude-continuity-launch-dir": p.Launch, "@claude-continuity-pending-dir": p.Pending,
		"@claude-continuity-panes-dir": p.Panes, "@moox-log-dir": p.Log,
	} {
		if got := s.Opt(t, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := s.Opt(t, "@claude-continuity-by-pid-dir"); got != "" {
		t.Errorf("the gate set @claude-continuity-by-pid-dir = %q; moox never sets it (R3-C2)", got)
	}
	if _, err := os.Lstat(p.Marker); err != nil {
		t.Fatalf("participation marker %s: %v", p.Marker, err)
	}
	first := w.daemon(s, 0)
	if first.TT != mooxroot.DevVersion {
		t.Fatalf("the dev daemon reports version %q, want %q", first.TT, mooxroot.DevVersion)
	}

	// Idempotent: the same version keeps the running daemon.
	w.ensure(dev, s)
	if rec, ok := daemonRunning(p.TTState); !ok || rec.PID != first.PID {
		t.Fatalf("a second ensure replaced or lost the daemon: %+v running=%v (first pid %d)", rec, ok, first.PID)
	}
	if !strings.Contains(w.log(s), "DAEMON-KEPT pid="+strconv.Itoa(first.PID)) {
		t.Fatalf("no DAEMON-KEPT line for pid %d:\n%s", first.PID, w.log(s))
	}

	// Upgrade: a newer binary replaces it.
	w.ensure(newer, s)
	second := w.daemon(s, first.PID)
	if second.TT != "99.0.0" {
		t.Fatalf("the replacement reports %q, want 99.0.0", second.TT)
	}
	if !tmuxtest.Poll(3*time.Second, 50*time.Millisecond, func() bool { return !alive(first.PID) }) {
		t.Fatalf("the old daemon pid %d is still alive after its replacement", first.PID)
	}

	// No ping-pong: the older dev build leaves the newer daemon running.
	w.ensure(dev, s)
	if rec, ok := daemonRunning(p.TTState); !ok || rec.PID != second.PID {
		t.Fatalf("an older ensure replaced the newer daemon: %+v", rec)
	}
	if !strings.Contains(w.log(s), "VERSION-SKEW") {
		t.Fatalf("no VERSION-SKEW line:\n%s", w.log(s))
	}

	// --replace forces it, even downwards.
	w.ensure(dev, s, "--replace")
	third := w.daemon(s, second.PID)
	if third.TT != mooxroot.DevVersion {
		t.Fatalf("--replace started %q, want the dev build", third.TT)
	}

	// --status sees it.
	r := w.tt(dev, "serve", "--status", "--json", "--tmux-socket", s.T.Socket, "--tmux-bin", s.T.Bin)
	var st wire.ServeStatus
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || r.code != 0 {
		t.Fatalf("serve --status --json: exit %d, %v: %s %s", r.code, err, r.stdout, r.stderr)
	}
	if !st.Running || st.PID == nil || *st.PID != third.PID || st.Active == nil || *st.Active != "1" || !st.Participating {
		t.Fatalf("status = %+v, want running pid %d, active 1, participating", st, third.PID)
	}
}

func TestE2E_Serve_ExitsWithServer(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)
	s := startServer(t, "")
	w.ensure(dev, s)
	rec := w.daemon(s, 0)

	s.Close() // kill-session every session; exit-empty ends the server
	if !tmuxtest.Poll(10*time.Second, 50*time.Millisecond, func() bool { return !alive(rec.PID) }) {
		t.Fatalf("daemon pid %d still alive 10s after its server ended; daemon.log:\n%s", rec.PID, w.log(s))
	}
	if log := w.log(s); !strings.Contains(log, "SERVER-GONE") {
		t.Fatalf("the daemon did not log SERVER-GONE:\n%s", log)
	}
	if _, err := os.Lstat(filepath.Join(w.paths(s).TTState, sockserve.PidName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the daemon left its pid file behind: %v", err)
	}
}

var waiterStart = regexp.MustCompile(`WAITER-START pid=(\d+)`)

func waiterPIDs(log string) []int {
	var pids []int
	for _, m := range waiterStart.FindAllStringSubmatch(log, -1) {
		n, _ := strconv.Atoi(m[1])
		pids = append(pids, n)
	}
	return pids
}

func TestE2E_Serve_WaiterRespawnsWhileServerAlive(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)
	s := startServer(t, "")
	w.ensure(dev, s)
	rec := w.daemon(s, 0)

	var waiter int
	if !tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		pids := waiterPIDs(w.log(s))
		if len(pids) > 0 && alive(pids[len(pids)-1]) {
			waiter = pids[len(pids)-1]
			return true
		}
		return false
	}) {
		t.Fatalf("no live waiter logged:\n%s", w.log(s))
	}
	// Only a process this test's daemon started, and only after checking it is
	// that daemon's `wait-for` on THIS socket.
	out, err := exec.Command("ps", "-o", "ppid=,command=", "-p", strconv.Itoa(waiter)).Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) < 2 || f[0] != strconv.Itoa(rec.PID) || !strings.Contains(string(out), s.T.Socket) ||
		!strings.Contains(string(out), "wait-for") {
		t.Fatalf("pid %d is not this daemon's waiter (ps: %q, %v); refusing to signal it", waiter, out, err)
	}
	if err := syscall.Kill(waiter, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	if !tmuxtest.Poll(8*time.Second, 50*time.Millisecond, func() bool {
		pids := waiterPIDs(w.log(s))
		return strings.Contains(w.log(s), "WAITER-RESPAWN") && len(pids) >= 2 &&
			pids[len(pids)-1] != waiter && alive(pids[len(pids)-1])
	}) {
		t.Fatalf("no respawned waiter within 8s:\n%s", w.log(s))
	}
	if !alive(rec.PID) {
		t.Fatalf("the daemon exited although its server is alive:\n%s", w.log(s))
	}
	if strings.Contains(w.log(s), "SERVER-GONE") {
		t.Fatalf("a killed waiter was taken for a dead server:\n%s", w.log(s))
	}
}

func TestE2E_Gate_TightensContinuityDirs(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	s := startServer(t, "")
	p := w.paths(s)

	legacy := filepath.Join(w.home, ".config", "tmux-claude")
	pre := []string{filepath.Join(legacy, "launch"), filepath.Join(legacy, "pending"), filepath.Join(legacy, "panes"), p.Launch}
	for _, d := range pre {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.ensure(dev, s)
	w.daemon(s, 0)

	log := w.log(s)
	for _, d := range append(pre, p.Pending, p.Panes, p.State, p.Log) {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if m := fi.Mode().Perm(); m != 0o700 {
			t.Errorf("%s is %o after the gate, want 0700", d, m)
		}
	}
	for _, d := range pre {
		if !strings.Contains(log, "MODE-TIGHTENED "+d) {
			t.Errorf("no MODE-TIGHTENED line for the pre-existing 0755 %s", d)
		}
	}
	if fi, err := os.Stat(filepath.Join(p.State, daemonLogName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("daemon.log mode: %v %v, want 0600", fi.Mode(), err)
	}
	if t.Failed() {
		t.Logf("daemon.log:\n%s", log)
	}
}

// TestE2E_Gate_NoMatchLeavesNoFilesAndDeactivates: a server whose name does
// not match gets @moox-active 0 and nothing under its key; a server that WAS
// active and is then excluded loses its daemon, marker and slots 101-104
// (§3.3; the shell half is TestE2E_Reload_ActivateThenExclude).
func TestE2E_Gate_NoMatchLeavesNoFilesAndDeactivates(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)

	b := startServer(t, "")
	b.Run(t, "set-option", "-g", "@moox-sockets", "nothing-matches-this")
	w.ensure(dev, b)
	if got := b.Opt(t, "@moox-active"); got != "0" {
		t.Fatalf("@moox-active = %q on a non-matching server, want 0", got)
	}
	if got := envOf(t, b, "MOOX_ACTIVE"); got != "0" {
		t.Fatalf("MOOX_ACTIVE = %q, want 0", got)
	}
	if _, err := os.Lstat(w.paths(b).State); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a non-matching server got files under its key %s: %v", w.paths(b).State, err)
	}

	a := startServer(t, "")
	w.ensure(dev, a)
	rec := w.daemon(a, 0)
	a.Run(t, tmuxtarget.Chain(
		[]string{"set-hook", "-g", "after-new-window[101]", "wait-for -S moox-dirty"},
		[]string{"set-hook", "-g", "client-attached[102]", "wait-for -S moox-touch"},
		[]string{"set-hook", "-g", "after-new-window[100]", "display-message theme"},
		[]string{"set-hook", "-g", "pane-focus-in[104]", "display-message tt"},
		[]string{"bind-key", "C-s", "wait-for", "-S", "moox-save"},
		[]string{"bind-key", "Z", "display-message", "freeze"},
	)...)
	a.Run(t, "set-option", "-g", "@moox-sockets", "nothing-matches-this")
	w.ensure(dev, a)

	if !tmuxtest.Poll(3*time.Second, 50*time.Millisecond, func() bool { return !alive(rec.PID) }) {
		t.Fatalf("the daemon survived deactivation:\n%s", w.log(a))
	}
	if _, err := os.Lstat(w.paths(a).Marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker survived deactivation: %v", err)
	}
	hooks := a.Run(t, "show-hooks", "-g")
	if got := activeSlotsIn(hooks); len(got) != 0 {
		t.Fatalf("slots 101-104 survived deactivation: %q", got)
	}
	if !strings.Contains(hooks, "after-new-window[100]") {
		t.Fatalf("the theme slot [100] was removed too:\n%s", hooks)
	}
	keys := a.Run(t, "list-keys", "-T", "prefix")
	if strings.Contains(keys, " C-s ") || strings.Contains(keys, " Z ") {
		t.Fatalf("prefix C-s / Z are still bound:\n%s", keys)
	}
	if a.Opt(t, "@moox-active") != "0" || envOf(t, a, "MOOX_ACTIVE") != "0" {
		t.Fatal("@moox-active / MOOX_ACTIVE are not 0 after deactivation")
	}
}

// TestE2E_Gate_ReloadBindingForTwoConfigFiles: with two -f files the gate
// re-binds prefix r to source both (§3.1, Q7).
func TestE2E_Gate_ReloadBindingForTwoConfigFiles(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)
	s := tmuxtest.StartCold(t)
	s.Run(t, "set-option", "-g", "@moox-sockets", s.Name)
	w.ensure(dev, s)
	files := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{config_files}"))
	if !strings.Contains(files, ",") {
		t.Skipf("config_files %q is one file; nothing to re-bind", files)
	}
	keys := s.Run(t, "list-keys", "-T", "prefix", "r")
	for _, f := range strings.Split(files, ",") {
		if !strings.Contains(keys, f) {
			t.Fatalf("prefix r does not source %s:\n%s", f, keys)
		}
	}
	if !strings.Contains(keys, "moox: reloaded") {
		t.Fatalf("prefix r lost its display:\n%s", keys)
	}
}

// ---------------------------------------------------------------------------
// V7: tt per server
// ---------------------------------------------------------------------------

func registerClaude(t *testing.T, w *world, s *tmuxtest.Server, sid string) int {
	t.Helper()
	pane := strings.Fields(s.Run(t, "display-message", "-p", "-t", s.Session, "#{pane_pid} #{session_name}:#{window_id}.#{pane_id}"))
	pid, _ := strconv.Atoi(pane[0])
	rec := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"startedAt":%d,"version":"2.1.273","kind":"interactive",`+
		`"entrypoint":"cli","tmux":%q,"name":"v7-%d","nameSource":"derived","updatedAt":%d,"status":"idle","statusUpdatedAt":%d}`,
		pid, sid, w.home, time.Now().UnixMilli(), pane[1], pid, time.Now().UnixMilli(), time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(w.claude, "sessions", strconv.Itoa(pid)+".json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	return pid
}

func sessionPIDs(t *testing.T, out string) map[int]bool {
	t.Helper()
	// Decoded untyped: json struct tags live only in the wire package (audit).
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("tt sessions --json: %v\n%s", err, out)
	}
	sessions, _ := doc["sessions"].([]any)
	pids := map[int]bool{}
	for _, s := range sessions {
		row, _ := s.(map[string]any)
		if pid, ok := row["pid"].(float64); ok {
			pids[int(pid)] = true
		}
	}
	return pids
}

func TestE2E_V7_TTPerServer(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)
	a := startServer(t, "moox-val-a")
	b := startServer(t, "moox-val-b")
	// Same session name, same ids on both fresh servers: the registry refs read
	// identically, which is the collision the foreign rule exists for.
	pa := registerClaude(t, w, a, "00000000-0000-4000-8000-00000000000a")
	pb := registerClaude(t, w, b, "00000000-0000-4000-8000-00000000000b")

	// tt serve for a and for b: one daemon each, in each server's own dir.
	w.ensure(dev, a)
	w.ensure(dev, b)
	da, db := w.daemon(a, 0), w.daemon(b, 0)
	if da.PID == db.PID || w.paths(a).TTState == w.paths(b).TTState {
		t.Fatalf("the two servers share a daemon or a state dir: %+v %+v", da, db)
	}

	for _, tc := range []struct {
		s          *tmuxtest.Server
		mine, them int
	}{{a, pa, pb}, {b, pb, pa}} {
		r := w.tt(dev, "sessions", "--json", "--tmux-socket", tc.s.T.Socket, "--tmux-bin", tc.s.T.Bin)
		if r.code != 0 {
			t.Fatalf("tt sessions on %s: exit %d: %s", tc.s.Name, r.code, r.stderr)
		}
		got := sessionPIDs(t, r.stdout)
		if !got[tc.mine] || got[tc.them] {
			t.Fatalf("tt sessions --tmux-socket %s lists %v; want its own %d and not the other server's %d\n%s",
				tc.s.Name, got, tc.mine, tc.them, r.stdout)
		}
	}

	// The popup command built for a names a's socket.
	prev := resolveTarget
	t.Cleanup(func() { resolveTarget = prev })
	resolveTarget = func() (tmuxtarget.Target, error) { return a.T, nil }
	if got := popupShell("new-session -d -s "+popupSession, false, ""); !strings.Contains(got, "-S '"+a.T.Socket+"'") {
		t.Fatalf("popup for a: %q has no -S %s", got, a.T.Socket)
	}

	// Outside tmux, TT_TMUX_SOCKET=a: every tmux exec tt makes names a.
	shimLog := filepath.Join(w.stubBin, "tmux.log")
	shim := filepath.Join(w.stubBin, "tmux-shim")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + tmuxtest.ConfQuote(shimLog) + "\nexec " + tmuxtest.ConfQuote(a.T.Bin) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	w.extraEnv = []string{"TT_TMUX_SOCKET=" + a.T.Socket, "TT_TMUX_BIN=" + shim}
	r := w.tt(dev, "sessions", "--json")
	w.extraEnv = nil
	if r.code != 0 || !sessionPIDs(t, r.stdout)[pa] {
		t.Fatalf("TT_TMUX_SOCKET=a tt sessions: exit %d, %s %s", r.code, r.stdout, r.stderr)
	}
	logged, err := os.ReadFile(shimLog)
	if err != nil || len(bytes.TrimSpace(logged)) == 0 {
		t.Fatalf("the shim saw no tmux exec: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		if !strings.HasPrefix(line, "-u -S "+a.T.Socket+" ") {
			t.Fatalf("a tmux exec did not name a's socket: %q", line)
		}
		if strings.Contains(line, w.defaultSocketGuard) {
			t.Fatalf("a tmux exec named the default socket: %q", line)
		}
	}
}

// ---------------------------------------------------------------------------
// reload: activate, then exclude, with pre-existing hook panes (§3.3)
// ---------------------------------------------------------------------------

// p6Ready reports whether P6's config split and hook edits are in the tree:
// the gate line in moox.conf, active.conf, and the marker test in both hooks.
func p6Ready() (string, bool) {
	share, err := tmuxtest.ShareRoot()
	if err != nil {
		return "", false
	}
	need := map[string]string{
		"moox.conf":        "serve --ensure",
		"conf/active.conf": "",
		"plugins/tmux-claude-continuity/claude-continuity.zsh":       "_cc_live",
		"plugins/tmux-claude-continuity/claude-continuity.bash":      "_cc_live",
		"plugins/tmux-claude-continuity/scripts/on_session_start.sh": "_cc_participating",
	}
	for rel, marker := range need {
		b, err := os.ReadFile(filepath.Join(share, rel))
		if err != nil || (marker != "" && !bytes.Contains(b, []byte(marker))) {
			return share, false
		}
	}
	return share, true
}

func TestE2E_Reload_ActivateThenExclude(t *testing.T) {
	share, ready := p6Ready()
	if !ready {
		t.Skip("needs P6 hook edits")
	}
	dev, _ := ttBinaries(t)
	_ = dev
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithShell(tmuxtest.HookZsh(t)))
	if s.Opt(t, "@moox-active") != "1" {
		t.Fatalf("the cold gate did not activate %s", s.Name)
	}
	p, err := sockets.For(s.T.Socket, tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS,
		"XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC})
	if err != nil {
		t.Fatal(err)
	}
	// A zsh pane (the server's default-command) and a bash pane, both started
	// while the server participates.
	zshPane := strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", s.Session, "#{pane_id}"))
	bash := tmuxtest.HookBash(t)
	bashPane := strings.TrimSpace(s.Run(t, "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", zshPane, bash.Command()))
	pause := func() { tmuxtest.Poll(time.Second, time.Second, func() bool { return false }) }
	launchOf := func(pane string) string { return filepath.Join(p.Launch, strings.TrimPrefix(pane, "%")) }

	// Positive control: while the server participates, a running command in
	// each pre-existing pane IS recorded — so the "nothing recorded" below is
	// the exclusion's doing, not a hook that never ran.
	for _, pane := range []string{zshPane, bashPane} {
		pane := pane
		s.Run(t, "send-keys", "-t", pane, "sleep 2", "Enter")
		if !tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool { return fileExists(launchOf(pane)) }) {
			t.Fatalf("control: pane %s recorded no launch/%s while active (the hook did not run?)", pane, strings.TrimPrefix(pane, "%"))
		}
	}
	// Let both sleeps end, then clear whatever recording is left (the zsh hook
	// clears its own at the prompt; the bash hook's clear is P6's to judge),
	// so a file seen below can only have been written after the exclusion.
	pause()
	pause()
	pause()
	for _, pane := range []string{zshPane, bashPane} {
		if fileExists(launchOf(pane)) {
			t.Logf("pane %s left launch/%s after its command ended; removed before the exclusion",
				pane, strings.TrimPrefix(pane, "%"))
			if err := os.Remove(launchOf(pane)); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Exclude and reload.
	daemonRec, _ := daemonRunning(p.TTState)
	s.Run(t, "set-option", "-g", "@moox-sockets", "nothing-matches-this")
	s.SourceMoox(t)
	if s.Opt(t, "@moox-active") != "0" {
		t.Fatalf("@moox-active is %q after the excluding reload", s.Opt(t, "@moox-active"))
	}
	if _, err := os.Lstat(p.Marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker survived the exclusion: %v", err)
	}
	if got := activeSlotsIn(s.Run(t, "show-hooks", "-g")); len(got) != 0 {
		t.Fatalf("slots 101-104 survived: %q", got)
	}
	if rec, running := daemonRunning(p.TTState); running {
		t.Fatalf("the daemon survived the exclusion: %+v", rec)
	}
	// Snapshots the daemon published while the server still participated are
	// legitimate: the positive control's commands are dirty events, and a
	// debounce save can be in flight across the exclusion (its observer half
	// drops the pid file before its saver half returns). They are listed once
	// the stopped daemon's process is gone; only one written after that is a
	// defect.
	if daemonRec.PID > 0 && !tmuxtest.Poll(10*time.Second, 50*time.Millisecond, func() bool { return !alive(daemonRec.PID) }) {
		t.Fatalf("the stopped daemon pid %d is still running", daemonRec.PID)
	}
	snapsBefore := map[string]bool{}
	if ents, err := os.ReadDir(p.Snapshots); err == nil {
		for _, e := range ents {
			snapsBefore[e.Name()] = true
		}
	}

	// Snapshot what is under the key, plant a pending file for each pane.
	before := treeOf(t, p.State)
	for _, pane := range []string{zshPane, bashPane} {
		n := strings.TrimPrefix(pane, "%")
		if err := os.MkdirAll(p.Pending, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p.Pending, n), []byte("touch "+filepath.Join(s.Home, "consumed-"+n)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hook := filepath.Join(share, "plugins", "tmux-claude-continuity", "scripts", "on_session_start.sh")
	for _, pane := range []string{zshPane, bashPane} {
		s.Run(t, "send-keys", "-t", pane, "printf '{\"session_id\":\"stub\"}' | "+hook+" >/dev/null 2>&1; sleep 2", "Enter")
	}
	pause()
	for _, pane := range []string{zshPane, bashPane} {
		if fileExists(launchOf(pane)) {
			t.Errorf("pane %s recorded a launch after the exclusion", pane)
		}
	}
	pause()
	pause()
	for _, pane := range []string{zshPane, bashPane} {
		n := strings.TrimPrefix(pane, "%")
		if _, err := os.Stat(filepath.Join(s.Home, "consumed-"+n)); err == nil {
			t.Errorf("pane %s consumed a planted pending file after the exclusion", pane)
		}
		if _, err := os.Stat(filepath.Join(p.Pending, n)); err != nil {
			t.Errorf("pane %s's planted pending file is gone: %v", pane, err)
		}
	}
	after := treeOf(t, p.State)
	for f := range after {
		if !before[f] && !strings.HasPrefix(f, "pending/") && f != daemonLogName {
			t.Errorf("a new file under the key after the exclusion: %s", f)
		}
	}
	// Both ways of saving refuse an excluded server with exit 3 ("not-active"),
	// and neither writes anything under its key.
	moox := filepath.Join(s.Bin, "moox")
	if !fileExists(moox) {
		t.Fatalf("WithBinaries built no moox in %s", s.Bin)
	}
	for _, args := range [][]string{{"save", "--direct", "--tmux-socket", s.T.Socket}, {"save", "-S", s.T.Socket}} {
		cmd := exec.Command(moox, args...)
		cmd.Env = append(os.Environ(), "HOME="+s.Home, "XDG_STATE_HOME="+s.XS, "XDG_DATA_HOME="+s.XD, "XDG_CONFIG_HOME="+s.XC)
		out, err := cmd.Output()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 || strings.TrimSpace(string(out)) != "not-active" {
			t.Errorf("moox %s on the excluded server: %v %q, want exit 3 not-active", strings.Join(args, " "), err, out)
		}
	}
	for f := range treeOf(t, p.State) {
		if !after[f] && !strings.HasPrefix(f, "pending/") && f != daemonLogName {
			t.Errorf("a file under the key after the refused saves: %s", f)
		}
	}
	if ents, err := os.ReadDir(p.Snapshots); err == nil {
		for _, e := range ents {
			if !snapsBefore[e.Name()] {
				dl, _ := os.ReadFile(filepath.Join(p.State, daemonLogName))
				t.Errorf("a snapshot written on the excluded server: %s; daemon.log:\n%s", e.Name(), dl)
			}
		}
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func treeOf(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out[rel] = true
		}
		return nil
	})
	return out
}
