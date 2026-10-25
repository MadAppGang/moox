//go:build tmuxlive

package main

// §9.6's GATE, against an ISOLATED tmux server — rewritten at "FINISH: scope
// panes + status line" to test what actually ships:
//
//   - the REAL `tt` binary, built from this tree for every test, running in the
//     isolated server's panes (P10 used a shell stub standing in for it, and
//     the pane guard that decides convergence lives in the binary);
//   - the REAL hook block, read BYTE FOR BYTE out of the user's
//     tmux/.tmux.conf between its `# >>> tt observer >>>` markers and sourced
//     into the isolated server, so what is tested is what the user will run;
//   - REAL keypresses: a tmux client attached from a pty (`script`) receives
//     `C-b C-o`, which closes the "keypress itself is untested" residual P9
//     and P10 both carried;
//   - REAL tmux-resurrect: case 10 runs resurrect's own save.sh and restore.sh
//     against the isolated server, with a simulated reboot in between.
//
// To run it:
//
//	cd moox/cmd/tt && TT_TMUXLIVE=1 go test -tags tmuxlive -run TestLive -v .
//
// and to run the SAME cases against P10's design (its three-hook set, its
// binding, and the binary as it was installed before this change) — which is
// how "failed on the old code" is shown rather than asserted:
//
//	TT_GATE_DESIGN=p10 TT_GATE_BIN=/path/to/old/tt TT_TMUXLIVE=1 go test -tags tmuxlive -run TestLive -v .
//
// and to run this tree's binary against ANOTHER tmux.conf's tt block — how the
// scope="sessions" cases were shown failing on the block as it was before code
// review round 2 (G2):
//
//	TT_GATE_CONF=/path/to/old/.tmux.conf TT_TMUXLIVE=1 go test -tags tmuxlive -run 'TestLiveCase(19|20)' -v .
//
// ############################################################################
// #  SAFETY. Every rule below exists because breaking it has cost this        #
// #  machine its tmux workspace or damaged the state that restores it.        #
// ############################################################################
//
//  1. EVERY tmux invocation from this process goes through liveTmux, which
//     always passes BOTH `-f /dev/null` (cannot source ~/.tmux.conf; on
//     2026-08-17 a scratch server that did cloned the whole workspace and
//     destroyed 16 sessions / 44 windows / 71 panes) and `-L <private socket>`
//     (cannot reach the user's server). audit.TmuxExecInTests polices this file
//     regardless of build tags. `run-shell` is never used, in any form
//     (2026-08-16: it segfaulted the server and killed 16 sessions).
//
//  2. NO PANE OF AN ISOLATED SERVER RUNS THE USER'S ZSH. Found while writing
//     this rewrite, and it is a defect P10's gate had too: the user's ~/.zshrc
//     sources tmux-claude-continuity, whose precmd DELETES
//     ~/.config/tmux-claude/launch/<N> at every prompt, keyed ONLY by the pane
//     NUMBER — and an isolated server's panes are numbered %0, %1, … exactly
//     like the real server's. So a zsh pane in a scratch server deletes the
//     launch recording of the real pane with the same number. Measured: eight
//     live Claude panes' recordings were deleted that way by probe runs on
//     2026-09-19 (see the implementation log). Therefore the server is started
//     with SHELL=/bin/sh, default-shell is pinned to /bin/sh, its first pane
//     runs /bin/sh explicitly, and every pane gets a throwaway HOME, so no rc
//     file of the user's is ever read inside it.
//
//  3. This process's own $TMUX is pointed at the PRIVATE server, so anything
//     it spawns that calls a bare `tmux` (resurrect's scripts, a wrapper) lands
//     there, and the scripts additionally run with a `tmux` wrapper first on
//     PATH that forces both guards.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// liveRefPattern is the anchored, colon-safe reference shape registryfs parses
// with. Session names may contain colons, so the anchoring is load-bearing.
var liveRefPattern = regexp.MustCompile(`^(.+):(@\d+)\.(%\d+)$`)

// liveGate refuses to run unless the env gate is set, so the build tag alone
// cannot start a server.
func liveGate(t *testing.T) string {
	t.Helper()
	if os.Getenv("TT_TMUXLIVE") == "" {
		t.Skip("TT_TMUXLIVE is unset: §9.6's gate starts a tmux server and is opt-in")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not on PATH")
	}
	return assertPrivateSocket(t, fmt.Sprintf("tt-obs-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
}

// assertPrivateSocket is belt and braces on top of the two flags, and it exists
// because the cost of being wrong here is the whole machine's workspace and the
// cost of the guard is nothing.
//
// The failure it guards against is not hypothetical: `-L ""` is accepted by
// tmux and resolves to the DEFAULT socket name, so a socket name that came back
// empty would point every command in this file at the user's own server, and
// the very first `kill-server` in a t.Cleanup would destroy all of it.
func assertPrivateSocket(t *testing.T, sock string) string {
	t.Helper()
	if err := checkPrivateSocket(sock); err != nil {
		t.Fatalf("REFUSING TO RUN: %v", err)
	}
	return sock
}

// checkPrivateSocket is the guard as a pure function, so the test below can
// prove it rejects what it claims to reject. A guard nobody has watched fail is
// a comment.
func checkPrivateSocket(sock string) error {
	if strings.TrimSpace(sock) == "" {
		return fmt.Errorf("the socket name is empty, which tmux resolves to the DEFAULT server")
	}
	if !strings.HasPrefix(sock, "tt-obs-test-") {
		return fmt.Errorf("socket %q is not one of this test's private names", sock)
	}
	if strings.ContainsAny(sock, "/ \t") {
		// -L takes a NAME under the socket directory, never a path.
		return fmt.Errorf("socket %q looks like a path, not a -L name", sock)
	}
	return nil
}

// liveTmux builds one tmux command against the private server. BOTH guards,
// ALWAYS, on EVERY invocation — and as string LITERALS of the exec.Command
// call, with the caller's arguments appended to cmd.Args afterwards, because
// that is the shape audit.TmuxExecInTests can see (P10's correction to §9.6).
func liveTmux(sock string, args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", "-f", "/dev/null", "-L", sock)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

// liveRun runs a tmux command and returns its trimmed stdout.
func liveRun(t *testing.T, sock string, args ...string) string {
	t.Helper()
	out, err := liveTmux(sock, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// liveQuiet runs a tmux command and returns its output or "", for polls.
func liveQuiet(sock string, args ...string) string {
	out, err := liveTmux(sock, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// liveServer starts a private server whose first session is "gate", registers
// kill-server with t.Cleanup (NOT defer, so it runs on a panic too), and
// applies safety rules 2 and 3.
func liveServer(t *testing.T) string {
	t.Helper()
	return liveServerSession(t, "gate")
}

// liveServerSession is liveServer with the first session's name chosen, which
// the restore cases need ("0" is resurrect's bootstrap name).
func liveServerSession(t *testing.T, first string) string {
	t.Helper()
	sock := liveGate(t)
	var socketPath string
	t.Cleanup(func() {
		_ = liveTmux(sock, "kill-server").Run()
		// kill-server leaves the socket FILE behind (P10 removed 44 by hand).
		// Remove it only if its name is one of ours AND nothing answers on it.
		if socketPath != "" && checkPrivateSocket(filepath.Base(socketPath)) == nil &&
			liveTmux(sock, "list-sessions").Run() != nil {
			_ = os.Remove(socketPath)
		}
		// F1: no process this server caused may outlive it. Anything whose
		// argv names this test's private socket is ours; one still running
		// after the grace period fails the test and is killed.
		assertNoLiveStragglers(t, sock)
	})
	startLiveServer(t, sock, first)
	socketPath = liveRun(t, sock, "display-message", "-p", "#{socket_path}")
	return sock
}

// assertNoLiveStragglers waits up to 10 s for every process whose argv
// names this server's socket (`-L <sock>` or a path ending in /<sock>) to
// exit — the observer the start button forks, a waiter, a client — and
// fails the test for, and SIGKILLs, any that does not (F1). Only processes
// naming this test's private socket are ever signalled.
func assertNoLiveStragglers(t *testing.T, sock string) {
	t.Helper()
	if checkPrivateSocket(sock) != nil {
		return
	}
	find := func() map[int]string {
		out, err := exec.Command("ps", "-ww", "-axo", "pid=,command=").Output()
		if err != nil {
			return nil
		}
		found := map[int]string{}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			pid, err := strconv.Atoi(f[0])
			if err != nil || pid == os.Getpid() || f[1] == "ps" {
				continue
			}
			for _, a := range f[1:] {
				if a == sock || strings.HasSuffix(a, "/"+sock) {
					found[pid] = strings.Join(f[1:], " ")
					break
				}
			}
		}
		return found
	}
	left := find()
	for deadline := time.Now().Add(10 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = find() {
		time.Sleep(100 * time.Millisecond)
	}
	for pid, cmd := range left {
		t.Errorf("pid %d outlived its tmux server %s: %s", pid, sock, cmd)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// startLiveServer starts (or, for a simulated reboot, RE-starts) the server on
// an already-guarded socket name.
func startLiveServer(t *testing.T, sock, first string) {
	t.Helper()
	// Rule 2: the server takes default-shell from the SHELL it was started
	// with, so the first pane cannot be a zsh either.
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	liveRun(t, sock, "new-session", "-d", "-s", first, "-x", "220", "-y", "50", "/bin/sh")
	liveRun(t, sock, "set-option", "-g", "default-shell", "/bin/sh")
	liveRun(t, sock, "set-environment", "-g", "HOME", t.TempDir())
	liveRun(t, sock, "set-environment", "-gu", "ENV")
	liveRun(t, sock, "set-environment", "-gu", "BASH_ENV")
	// Rule 3.
	t.Setenv("TMUX", liveRun(t, sock, "display-message", "-p", "#{socket_path}")+",0,0")
	t.Setenv("TMUX_PANE", "")
}

// liveWaitFor polls a condition every 25 ms until it holds or the deadline
// passes, and fails with the server's own pane listing.
func liveWaitFor(t *testing.T, sock string, what string, cond func() bool) {
	t.Helper()
	liveWaitForIn(t, sock, what, 8*time.Second, cond)
}

func liveWaitForIn(t *testing.T, sock, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\npanes:\n%s", what, livePaneDump(sock))
}

// livePaneDump is the listing a failure needs.
func livePaneDump(sock string) string {
	return liveQuiet(sock, "list-panes", "-a", "-F",
		"#{session_name}:#{window_index}(#{window_id},#{window_width}w).#{pane_index} #{pane_id} w=#{pane_width} "+
			"obs=[#{@tt_obs}] cmd=#{pane_current_command} dead=#{pane_dead} start=[#{pane_start_command}]")
}

// ---------------------------------------------------------------------------
// the design under test: this change's, or P10's
// ---------------------------------------------------------------------------

// gateDesign is what a case sources and which binary its panes run.
type gateDesign struct {
	name string
	// conf is the tmux config text sourced into the isolated server.
	conf string
	// bin is the `tt` the panes run.
	bin string
	// prefix is the key the attached client sends before C-o.
	prefix byte
}

// designFor picks the design from TT_GATE_DESIGN: unset means THIS change's,
// "p10" means P10's, for the before/after evidence.
func designFor(t *testing.T) gateDesign {
	t.Helper()
	if os.Getenv("TT_GATE_DESIGN") == "p10" {
		bin := os.Getenv("TT_GATE_BIN")
		if bin == "" {
			t.Fatal("TT_GATE_DESIGN=p10 needs TT_GATE_BIN: the binary as it was before this change")
		}
		return gateDesign{name: "p10", conf: p10Conf, bin: bin, prefix: 0x02}
	}
	if bin := os.Getenv("TT_GATE_BIN"); bin != "" {
		// This tree's block with ANOTHER binary: how case 21 was shown failing
		// on the panel as it was before code review round 2 (A9).
		t.Logf("TT_GATE_BIN: the panes run %s, not a build of this tree", bin)
		return gateDesign{name: "current", conf: currentConf(t), bin: bin, prefix: 0x02}
	}
	return gateDesign{name: "current", conf: currentConf(t), bin: buildGateBinary(t), prefix: 0x02}
}

// p10Conf is P10's design as P10's gate ran it: §5.5(b2)'s three hooks (with
// P10's literal `-l 46`, since the documented format never expanded), the
// README's binding, and §5.5(b2)'s @resurrect-processes line.
const p10Conf = `set -g @tt_obs_scope off
set -g @tt_obs_width 46
set-hook -g after-new-window[50] 'if-shell -F "#{==:#{E:@tt_obs_scope},all}" "split-window -h -f -l 46 -d \"tt observe --pane\""'
set-hook -g session-created[50]  'if-shell -F "#{==:#{E:@tt_obs_scope},all}" "split-window -h -f -l 46 -d \"tt observe --pane\""'
set-hook -g window-linked[50]    'if-shell -F "#{==:#{E:@tt_obs_scope},all}" "split-window -h -f -l 46 -d \"tt observe --pane\""'
bind-key C-o if-shell -F -t ':.{right}' '#{@tt_obs}' 'kill-pane -t ":.{right}"' 'split-window -h -f -l 46 -d "tt observe --pane"'
set -g @resurrect-processes '~tt observe --pane'
`

// tmuxConfPath is the tt block moox ships: share/moox/conf/tt.conf (sourced
// from active.conf on a server that takes part in moox continuity).
func tmuxConfPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "share", "moox", "conf", "tt.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// gateResurrectLine is the tmux-resurrect mapping case 10's REAL resurrect
// round trips need. moox no longer ships it (its engine never saves an
// observer pane), so the gate supplies the line the old tmux.conf carried.
const gateResurrectLine = `set -g @resurrect-processes '"~tt observe --pane->exit"'`

// currentConf is the tt block of share/moox/conf/tt.conf, byte for byte, plus
// gateResurrectLine for the resurrect cases.
func currentConf(t *testing.T) string {
	t.Helper()
	path := tmuxConfPath(t)
	if alt := os.Getenv("TT_GATE_CONF"); alt != "" {
		path = alt
		t.Logf("TT_GATE_CONF: sourcing the tt block of %s, not this repository's", alt)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	block, err := observerBlock(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return gateResurrectLine + "\n" + block
}

// buildGateBinary builds tt from this tree into the test's temp dir. The Go
// build cache makes every build after the first a link step.
func buildGateBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "tt")
	cmd := exec.Command("go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// gateEnv is one test's private world: a bin dir with the design's `tt`, a
// state dir for the socket, and a fixture Claude tree, all installed into the
// isolated server's global environment so every pane it creates sees them.
type gateEnv struct {
	sock   string
	design gateDesign
	root   string
	bin    string
	state  string
	claude string
}

func newGateEnv(t *testing.T, sock string, d gateDesign) *gateEnv {
	t.Helper()
	root := t.TempDir()
	// The state dir holds the observer's unix socket, and macOS caps a socket
	// path at 104 bytes; t.TempDir() is far longer ("bind: invalid argument",
	// found by case 18). So it gets a short private directory of its own.
	state, err := os.MkdirTemp("/tmp", "ttg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	e := &gateEnv{
		sock: sock, design: d, root: root,
		bin:    filepath.Join(root, "bin"),
		state:  state,
		claude: filepath.Join(root, "claude"),
	}
	for _, dir := range []string{e.bin, filepath.Join(e.claude, "sessions"), filepath.Join(e.claude, "jobs"),
		filepath.Join(e.claude, "projects")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(d.bin, filepath.Join(e.bin, "tt")); err != nil {
		t.Fatal(err)
	}
	e.writeTmuxWrapper(t)
	e.install(t)
	return e
}

// writeTmuxWrapper puts a `tmux` first on the panes' PATH that logs every call
// a tt process makes AND forces both guards onto it. tt calls a bare `tmux`
// and relies on $TMUX, which inside a private-server pane already names that
// server; the wrapper makes that true even if $TMUX were somehow wrong.
func (e *gateEnv) writeTmuxWrapper(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + e.callLog() + "\nexec " + real + " -f /dev/null -L " + e.sock + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(e.bin, "tmux"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
}

// callLog is where the wrapper records tt's tmux calls.
func (e *gateEnv) callLog() string { return filepath.Join(e.root, "tmux-calls.log") }

// install puts the env into the server. No `claude` is on this PATH, so the
// corroboration source reports itself unavailable instead of spawning the
// vendor CLI from a test.
func (e *gateEnv) install(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"PATH":              e.path(),
		"XDG_STATE_HOME":    e.state,
		"CLAUDE_CONFIG_DIR": e.claude,
		"TERM_THEME":        "dark",
	} {
		liveRun(t, e.sock, "set-environment", "-g", k, v)
	}
	liveRun(t, e.sock, "set-option", "-g", "default-size", "220x50")
}

// ttDir is tt's state dir for THIS gate server (moox §4.2): tt state is per
// server, and a private -L server is never the legacy default, so it is
// $XDG_STATE_HOME/tt/sockets/<name>@<hash>.
func (e *gateEnv) ttDir() string {
	sock, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{"TMUX_TMPDIR": os.Getenv("TMUX_TMPDIR")}, e.sock)
	if err != nil {
		return filepath.Join(e.state, "tt", "sockets", "unresolvable")
	}
	return filepath.Join(e.state, "tt", "sockets", sockets.Key(sock))
}

// path is the PATH every tt pane of this env runs with.
func (e *gateEnv) path() string { return e.bin + ":/usr/bin:/bin:/usr/sbin:/sbin" }

// window opens a window running a tt command line. MEASURED on tmux 3.7c: a
// pane created by an UNATTACHED client (this test's CLI) gets that client's
// PATH, not the server's global one — so without this, `tt` resolved to the
// INSTALLED ~/go/bin/tt and `tmux` to the real binary, and the first run of
// case 18 silently tested the old build. So the client is given the gate's
// PATH, and the command names the binary under test absolutely.
func (e *gateEnv) window(t *testing.T, name, ttArgs string) {
	t.Helper()
	cmd := liveTmux(e.sock, "new-window", "-d", "-t", "gate:", "-n", name,
		"sleep 0.3; exec "+filepath.Join(e.bin, "tt")+" "+ttArgs)
	cmd.Env = append(os.Environ(), "PATH="+e.path())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opening %s: %v\n%s", name, err, out)
	}
}

// source loads the design's config into the server and then pins the scope to
// "off", which is the baseline every case below was written against.
//
// The shipped block sets @tt_obs_scope = all (the user's decision: "the idea
// was that panel is visible across all sessions"), so without this line every
// case would start with a sidebar it did not ask for. Case 22 sources the block
// AS SHIPPED and asserts that default, so pinning it here cannot hide a change
// to it.
func (e *gateEnv) source(t *testing.T) {
	t.Helper()
	e.sourceAsShipped(t)
	e.scope(t, "off")
}

// sourceAsShipped loads the design's config and changes nothing afterwards.
func (e *gateEnv) sourceAsShipped(t *testing.T) {
	t.Helper()
	path := filepath.Join(e.root, "observer.tmux")
	if err := os.WriteFile(path, []byte(e.design.conf), 0o600); err != nil {
		t.Fatal(err)
	}
	liveRun(t, e.sock, "source-file", path)
}

// scopeNow reads @tt_obs_scope back off the server.
func scopeNow(t *testing.T, sock string) string {
	t.Helper()
	return liveRun(t, sock, "show-option", "-gv", "@tt_obs_scope")
}

// scope sets @tt_obs_scope.
func (e *gateEnv) scope(t *testing.T, v string) {
	t.Helper()
	liveRun(t, e.sock, "set-option", "-g", "@tt_obs_scope", v)
}

// ---------------------------------------------------------------------------
// an attached client on a pty — real keypresses, and real "window shown" events
// ---------------------------------------------------------------------------

// liveClient is `tmux attach` running inside `script`, which gives it a pty,
// so it is a real client: tmux sizes windows to it, fires client hooks for it,
// and runs its key table on what we type. It adds no pane to the server.
type liveClient struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	name string
}

func attachClient(t *testing.T, sock, target string) *liveClient {
	t.Helper()
	if strings.ContainsAny(target, " '\";") {
		t.Fatalf("attach target %q would need quoting", target)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]bool{}
	for _, c := range strings.Fields(liveQuiet(sock, "list-clients", "-F", "#{client_name}")) {
		before[c] = true
	}
	cmd := exec.Command("script", "-q", "/dev/null", "sh", "-c",
		"stty cols 220 rows 50; exec "+tmuxPath+" -f /dev/null -L "+sock+" attach -t "+target)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "HOME=" + t.TempDir()}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &liveClient{cmd: cmd, in: in}
	t.Cleanup(c.kill)
	liveWaitFor(t, sock, "the pty client to attach", func() bool {
		for _, n := range strings.Fields(liveQuiet(sock, "list-clients", "-F", "#{client_name}")) {
			if !before[n] {
				c.name = n
				return true
			}
		}
		return false
	})
	return c
}

func (c *liveClient) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// toggle presses <prefix> C-o on the client.
func (c *liveClient) toggle(t *testing.T, d gateDesign) {
	t.Helper()
	c.press(t, d, 0x0f)
}

// press sends <prefix> <key> on the client.
func (c *liveClient) press(t *testing.T, d gateDesign, key byte) {
	t.Helper()
	if _, err := c.in.Write([]byte{d.prefix, key}); err != nil {
		t.Fatalf("typing the binding: %v", err)
	}
}

// ---------------------------------------------------------------------------
// counting observers
// ---------------------------------------------------------------------------

// paneInfo is one pane as the gate reads it.
type paneInfo struct {
	id, flag, start, cmd string
	index, width         int
	dead                 bool
}

func (p paneInfo) observer() bool {
	return !p.dead && (p.flag == "1" || strings.Contains(p.start, "tt observe --pane"))
}

func (p paneInfo) auto() bool {
	return !p.dead && strings.Contains(p.start, "tt observe --pane --auto")
}

// windowPanes lists one window's panes; target is anything -t accepts.
//
// The output is split RAW: a pane with an empty start command ends its line in
// a tab, and trimming the whole output would drop the last pane's final field
// and, with it, the pane (found the hard way: case 10 recorded a 2-pane window
// as 1 pane).
func windowPanes(sock, target string) []paneInfo {
	raw, err := liveTmux(sock, "list-panes", "-t", target, "-F",
		"#{pane_id}\t#{@tt_obs}\t#{pane_index}\t#{pane_width}\t#{pane_dead}\t#{pane_current_command}\t#{pane_start_command}").Output()
	if err != nil {
		return nil
	}
	var ps []paneInfo
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.SplitN(line, "\t", 7)
		if len(f) != 7 {
			continue
		}
		idx, _ := strconv.Atoi(f[2])
		w, _ := strconv.Atoi(f[3])
		ps = append(ps, paneInfo{id: f[0], flag: f[1], index: idx, width: w, dead: f[4] == "1", cmd: f[5], start: f[6]})
	}
	return ps
}

// observers counts observer panes in one window: flagged by a renderer, or
// split to run one. flagged counts only the former.
func observers(sock, target string) (all, flagged int) {
	for _, p := range windowPanes(sock, target) {
		if p.observer() {
			all++
		}
		if !p.dead && p.flag == "1" {
			flagged++
		}
	}
	return all, flagged
}

// allWindows is every window id on the server, with its session.
func allWindows(sock string) []string {
	var out []string
	for _, l := range strings.Split(liveQuiet(sock, "list-windows", "-a", "-F", "#{session_name}:#{window_id}"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// settled waits until a window has exactly want observers and every one of
// them is FLAGGED — i.e. the renderer started and won its claim, so the count
// is a converged state, not a pane that is about to exit.
func settled(t *testing.T, sock, target string, want int) {
	t.Helper()
	liveWaitFor(t, sock, fmt.Sprintf("%s to settle on %d observer(s)", target, want), func() bool {
		all, flagged := observers(sock, target)
		return all == want && flagged == want
	})
}

// stays asserts a window's observer count holds for a while — convergence, not
// a passing moment.
func stays(t *testing.T, sock, target string, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if all, _ := observers(sock, target); all != want {
			t.Fatalf("%s has %d observer panes, want %d\n%s", target, all, want, livePaneDump(sock))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// windowID resolves a target to its window id.
func windowID(t *testing.T, sock, target string) string {
	t.Helper()
	return liveRun(t, sock, "display-message", "-p", "-t", target, "#{window_id}")
}

// ---------------------------------------------------------------------------
// cases 1–3: baseline, toggle 1, toggle 2 — by KEYPRESS
// ---------------------------------------------------------------------------

func TestLiveCase1to3_BaselineAndBothToggles(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	c := attachClient(t, sock, "gate")

	w := windowID(t, sock, "gate:")
	base := len(windowPanes(sock, w))
	original := liveRun(t, sock, "display-message", "-p", "-t", w, "#{pane_id}")

	c.toggle(t, d)
	settled(t, sock, w, 1)
	if n := len(windowPanes(sock, w)); n != base+1 {
		t.Fatalf("toggle 1: %d panes, want baseline+1 = %d", n, base+1)
	}
	if now := liveRun(t, sock, "display-message", "-p", "-t", w, "#{pane_id}"); now != original {
		t.Fatalf("the active pane moved from %s to %s; -d must leave focus alone", original, now)
	}
	// The toggle is the scope's switch: it writes the option for the whole
	// server, and what it opens is an --auto pane, so the same reconcile that
	// created it can retire it.
	if d.name == "current" {
		if got := scopeNow(t, sock); got == "off" {
			t.Fatal("toggle 1 left @tt_obs_scope off; the switch is global now")
		}
		var auto bool
		for _, pn := range windowPanes(sock, w) {
			if pn.auto() {
				auto = true
			}
		}
		if !auto {
			t.Fatalf("toggle 1 opened a pane that the scope cannot retire\n%s", livePaneDump(sock))
		}
	}

	c.toggle(t, d)
	liveWaitFor(t, sock, "toggle 2 to close the observer", func() bool {
		all, _ := observers(sock, w)
		return all == 0 && len(windowPanes(sock, w)) == base
	})
	if d.name == "current" {
		if got := scopeNow(t, sock); got != "off" {
			t.Fatalf("toggle 2 left @tt_obs_scope %q, want off", got)
		}
	}
	// The assertion that would have caught C1: the USER's pane still exists.
	if !strings.Contains(liveRun(t, sock, "list-panes", "-t", w, "-F", "#{pane_id}"), original) {
		t.Fatalf("toggle 2 destroyed the original pane %s", original)
	}
}

// ---------------------------------------------------------------------------
// case 4: a FOREIGN pane to the right — the fail-safe
// ---------------------------------------------------------------------------

func TestLiveCase4_AForeignPaneToTheRightDestroysNothing(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	c := attachClient(t, sock, "gate")
	w := windowID(t, sock, "gate:")

	c.toggle(t, d)
	settled(t, sock, w, 1)
	var user string
	for _, p := range windowPanes(sock, w) {
		if !p.observer() {
			user = p.id
		}
	}
	// A foreign pane directly to the RIGHT of the active pane, which is what
	// `:.{right}` names. It is split off the USER's pane, not the observer's:
	// since the sidebar learned to re-dock itself to the right edge, a pane
	// split off the observer is moved to its left within a moment, and the
	// case became a race between that and the toggle (it failed 4 runs in 8,
	// on the code before and after the grouping change alike). Split off the
	// user's pane it sits between the two — [user][foreign][sidebar] — the
	// sidebar stays docked, nothing moves, and the fail-safe is exercised
	// every time.
	liveRun(t, sock, "split-window", "-h", "-d", "-t", user, "sleep 600")
	liveWaitFor(t, sock, "[user][foreign][sidebar], the sidebar still at the edge", func() bool {
		type geom struct{ left, width, right int }
		g := map[string]geom{}
		var foreign, sidebar string
		for _, l := range strings.Split(liveQuiet(sock, "list-panes", "-t", w, "-F",
			"#{pane_id} #{pane_left} #{pane_width} #{pane_at_right} #{@tt_obs} #{pane_start_command}"), "\n") {
			f := strings.Fields(l)
			if len(f) < 5 {
				continue
			}
			left, _ := strconv.Atoi(f[1])
			width, _ := strconv.Atoi(f[2])
			right, _ := strconv.Atoi(f[3])
			g[f[0]] = geom{left, width, right}
			switch {
			case strings.Contains(l, "sleep 600"):
				foreign = f[0]
			case f[4] == "1":
				sidebar = f[0]
			}
		}
		u, fp, sb := g[user], g[foreign], g[sidebar]
		return foreign != "" && sidebar != "" && fp.left == u.left+u.width+1 && sb.right == 1
	})
	if active := strings.TrimSpace(liveQuiet(sock, "display-message", "-p", "-t", w, "#{pane_id}")); active != user {
		t.Fatalf("the active pane is %s, want the user's %s: `:.{right}` would not name the foreign pane", active, user)
	}
	settled(t, sock, w, 1)
	var keep []string
	for _, p := range windowPanes(sock, w) {
		if !p.observer() {
			keep = append(keep, p.id)
		}
	}

	// `:.{right}` is now the foreign pane, so the OFF branch finds no observer
	// there and kills nothing. The scope still goes off — that write is
	// unconditional — and the sidebar then retires itself, the panel being off
	// everywhere. What may NOT happen is the property this case exists for:
	// neither the user's pane nor the foreign one is destroyed.
	c.toggle(t, d)
	liveWaitFor(t, sock, "the scope to go off", func() bool { return scopeNow(t, sock) == "off" })
	settled(t, sock, w, 0)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		present := map[string]bool{}
		for _, p := range windowPanes(sock, w) {
			if !p.dead {
				present[p.id] = true
			}
		}
		for _, id := range keep {
			if !present[id] {
				t.Fatalf("pane %s was destroyed by a toggle that found no observer at {right}\n%s", id, livePaneDump(sock))
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// case 5: remain-on-exit on — a toggle pair returns to baseline
// ---------------------------------------------------------------------------

func TestLiveCase5_RemainOnExitDoesNotAccumulateDeadPanes(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	c := attachClient(t, sock, "gate")
	w := windowID(t, sock, "gate:")
	liveRun(t, sock, "set-option", "-w", "-t", w, "remain-on-exit", "on")
	base := len(windowPanes(sock, w))

	c.toggle(t, d)
	settled(t, sock, w, 1)
	c.toggle(t, d)
	liveWaitFor(t, sock, "the pane count to return to baseline", func() bool {
		return len(windowPanes(sock, w)) == base
	})
}

// ---------------------------------------------------------------------------
// case 6: the toggle is GLOBAL — closing the sidebar in one window closes the
// panel, and the other windows retire theirs when they are next shown
// ---------------------------------------------------------------------------

// This is the reported bug, measured: "when i close plane in one window it stay
// remains in all remaining windows". The toggle used to be per-window, so it
// was. It now writes the server-wide @tt_obs_scope, the window you are in loses
// its sidebar at once, and every other window loses its own through the same
// reconcile the next time it is shown.
func TestLiveCase6_TheToggleTurnsThePanelOffEverywhere(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's design has no global toggle")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	c := attachClient(t, sock, "gate")

	first := windowID(t, sock, "gate:")
	c.toggle(t, d) // on, everywhere
	settled(t, sock, first, 1)

	// A second window, shown once so the scope gives it its own sidebar.
	liveRun(t, sock, "new-window", "-d", "-t", "gate:")
	var second string
	for _, w := range strings.Fields(liveRun(t, sock, "list-windows", "-t", "gate", "-F", "#{window_id}")) {
		if w != first {
			second = w
		}
	}
	liveRun(t, sock, "select-window", "-t", second)
	settled(t, sock, second, 1)
	liveRun(t, sock, "select-window", "-t", first)
	settled(t, sock, first, 1)

	// One press, in ONE window. The window it was pressed in empties at once.
	c.toggle(t, d)
	settled(t, sock, first, 0)
	if got := scopeNow(t, sock); got != "off" {
		t.Fatalf("@tt_obs_scope is %q, want off", got)
	}
	// The other window still holds its sidebar only until it is shown again —
	// and then it has none, without a second key press.
	liveRun(t, sock, "select-window", "-t", second)
	settled(t, sock, second, 0)
	stays(t, sock, second, 0, 500*time.Millisecond)

	// And back on, in one press, for the window that is showing.
	c.toggle(t, d)
	settled(t, sock, second, 1)
	if got := scopeNow(t, sock); got != "all" {
		t.Fatalf("@tt_obs_scope is %q, want all", got)
	}
}

// A `sessions` scope survives an off/on pair: the toggle remembers what it
// turned off in @tt_obs_scope_prev instead of rewriting the user's setting to
// `all` (set -gF expands the value; a plain set would store the format text).
func TestLiveCase6b_TheToggleRemembersASessionsScope(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's design has no global toggle")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	liveRun(t, sock, "set-option", "-g", "@tt_obs_sessions", "gate")
	e.scope(t, "sessions")
	// A second pane on the server, so @tt_obs_hold ("a server of exactly one
	// pane" — resurrect's from-scratch marker) stops holding the hooks back.
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "filler", "/bin/sh")
	c := attachClient(t, sock, "gate")

	w := windowID(t, sock, "gate:")
	settled(t, sock, w, 1) // in scope by name

	c.toggle(t, d) // off
	settled(t, sock, w, 0)
	if got := scopeNow(t, sock); got != "off" {
		t.Fatalf("@tt_obs_scope is %q, want off", got)
	}
	c.toggle(t, d) // on again — as `sessions`, not as `all`
	settled(t, sock, w, 1)
	if got := scopeNow(t, sock); got != "sessions" {
		t.Fatalf("@tt_obs_scope is %q after a toggle pair, want sessions", got)
	}
}

// ---------------------------------------------------------------------------
// case 7: scope="all" — EVERY route a window can come into view by ends with
// exactly ONE observer; then scope="off" leaves none
// ---------------------------------------------------------------------------

func TestLiveCase7_EveryCreationRouteEndsWithExactlyOneObserver(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")
	c := attachClient(t, sock, "gate")

	check := func(route, target string) string {
		t.Helper()
		w := windowID(t, sock, target)
		settled(t, sock, w, 1)
		stays(t, sock, w, 1, 700*time.Millisecond)
		t.Logf("route %-44s -> window %s: exactly 1 observer", route, w)
		return w
	}

	// 1. new-window by the attached client (becomes current).
	liveRun(t, sock, "new-window", "-t", "gate:")
	check("new-window", "gate:")

	// 2. new-window -d, then shown.
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "bg")
	liveRun(t, sock, "select-window", "-t", "gate:bg")
	check("new-window -d, then select-window", "gate:bg")

	// 3. new-session -d, then the client switches to it.
	liveRun(t, sock, "new-session", "-d", "-s", "second")
	liveRun(t, sock, "switch-client", "-c", c.name, "-t", "second")
	check("new-session -d, then switch-client", "second:")

	// 4. link-window: a window WITHOUT an observer linked into another session
	// and shown there.
	liveRun(t, sock, "new-session", "-d", "-s", "third")
	third := windowID(t, sock, "third:")
	liveRun(t, sock, "link-window", "-s", third, "-t", "second:")
	liveRun(t, sock, "select-window", "-t", "second:"+third)
	check("link-window, then select-window", third)

	// 5. The SAME window, which now has an observer, shown in its other
	// session: still one (a window, not a session, owns its panes).
	liveRun(t, sock, "switch-client", "-c", c.name, "-t", "third")
	check("linked window shown in its second session", third)

	// 6. move-window into another session, then shown.
	liveRun(t, sock, "new-window", "-d", "-t", "third:", "-n", "mover")
	liveRun(t, sock, "move-window", "-d", "-s", "third:mover", "-t", "gate:")
	liveRun(t, sock, "switch-client", "-c", c.name, "-t", "gate")
	liveRun(t, sock, "select-window", "-t", "gate:mover")
	check("move-window, then select-window", "gate:mover")

	// 7. A fresh client attaching to a session nobody has looked at.
	liveRun(t, sock, "new-session", "-d", "-s", "fourth")
	attachClient(t, sock, "fourth")
	check("client attach", "fourth:")

	// Every window any route SHOWED has exactly one; none has two.
	for _, w := range allWindows(sock) {
		if all, _ := observers(sock, w); all > 1 {
			t.Errorf("window %s has %d observers\n%s", w, all, livePaneDump(sock))
		}
	}

	// scope=off: every window that is shown again gives its automatic observer
	// back (orphans are case 15's subject; here, just that none is created).
	e.scope(t, "off")
	liveRun(t, sock, "new-window", "-t", "gate:", "-n", "after-off")
	stays(t, sock, windowID(t, sock, "gate:after-off"), 0, 700*time.Millisecond)
}

// ---------------------------------------------------------------------------
// case 8: our hooks do not eat somebody else's, and theirs do not eat ours
// ---------------------------------------------------------------------------

func TestLiveCase8_OurHooksDoNotEatSomebodyElsesHook(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	// A foreign hook set FIRST, unindexed, the way another plugin would.
	hook := "session-window-changed"
	if d.name == "p10" {
		hook = "after-new-window"
	}
	liveRun(t, sock, "set-hook", "-g", hook, "set-option -g @other_hook_ran 1")
	e.source(t)
	e.source(t) // re-sourcing must not duplicate ours
	e.scope(t, "all")
	c := attachClient(t, sock, "gate")

	liveRun(t, sock, "new-window", "-t", "gate:")
	w := windowID(t, sock, "gate:")
	settled(t, sock, w, 1)
	if got := liveRun(t, sock, "show", "-gv", "@other_hook_ran"); got != "1" {
		t.Fatalf("the pre-existing %s hook did not run (@other_hook_ran=%q): ours ate it", hook, got)
	}
	// Exactly one of ours per hook name, after two sources.
	// window-resized is a WINDOW hook, listed by -gw rather than -g.
	listing := liveRun(t, sock, "show-hooks", "-g") + "\n" + liveRun(t, sock, "show-hooks", "-gw")
	if d.name == "current" {
		want := map[string]int{
			"session-window-changed": 2, // the foreign one at [0], ours appended once
			"client-session-changed": 1,
			"client-attached":        1,
			"window-resized":         1,
		}
		for name, n := range want {
			if got := strings.Count(listing, name+"["); got != n {
				t.Errorf("%s has %d entries after two sources, want %d:\n%s", name, got, n, listing)
			}
		}
	}
	_ = c
}

// ---------------------------------------------------------------------------
// case 9: a window too narrow for a useful sidebar gets NONE
// ---------------------------------------------------------------------------

// The threshold is @tt_obs_width + 1 (the border) + @tt_obs_min_width, i.e.
// 46 + 1 + 80 = 127 by default. P10 measured that tmux treats a split width as
// a request and squeezes the OTHER pane (a 20-column window came out 1 + 18),
// so without the check a narrow window loses the user's pane, not the sidebar.
func TestLiveCase9_ANarrowWindowGetsNoObserverAndAWideOneGetsOne(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")

	liveRun(t, sock, "new-session", "-d", "-s", "narrow", "-x", "126", "-y", "30")
	liveRun(t, sock, "new-window", "-d", "-t", "narrow:", "-n", "n2")
	w := windowID(t, sock, "narrow:n2")
	liveRun(t, sock, "resize-window", "-t", w, "-x", "126")
	liveRun(t, sock, "select-window", "-t", w)
	stays(t, sock, w, 0, 800*time.Millisecond)
	for _, p := range windowPanes(sock, w) {
		if p.width < 126 {
			t.Fatalf("a 126-column window was squeezed: pane %s is %d columns\n%s", p.id, p.width, livePaneDump(sock))
		}
	}

	// Exactly at the threshold it gets one, and the user's pane keeps 80.
	liveRun(t, sock, "resize-window", "-t", w, "-x", "127")
	settled(t, sock, w, 1)
	for _, p := range windowPanes(sock, w) {
		if !p.observer() && p.width < 80 {
			t.Fatalf("at 127 columns the user's pane is %d, want >= 80\n%s", p.width, livePaneDump(sock))
		}
	}

	// And shrinking below it takes the automatic one away again.
	liveRun(t, sock, "resize-window", "-t", w, "-x", "126")
	liveWaitFor(t, sock, "the automatic observer to retire below the threshold", func() bool {
		all, _ := observers(sock, w)
		return all == 0
	})
}

// ---------------------------------------------------------------------------
// case 11: the status line — option -> rendered status bar, NO refresh-client
// ---------------------------------------------------------------------------

func TestLiveCase11_TheOptionReachesTheRenderedStatusLine(t *testing.T) {
	sock := liveServer(t)
	liveRun(t, sock, "set-option", "-g", "status", "on")
	// status-interval 0: tmux never repaints on a timer, so anything that
	// appears was repainted by the set-option itself.
	liveRun(t, sock, "set-option", "-g", "status-interval", "0")
	liveRun(t, sock, "set-option", "-g", "status-right", statusRightFromConf(t))

	// A status line exists only in what a CLIENT paints: nest a client in a
	// pane and capture the OUTER pane (P10's technique; TMUX= forces the nest).
	liveRun(t, sock, "new-window", "-d", "-n", "nested",
		"sh -c 'TMUX= exec tmux -f /dev/null -L "+sock+" attach -t gate'")
	liveWaitFor(t, sock, "the nested client to attach", func() bool {
		return liveQuiet(sock, "list-clients", "-F", "#{client_name}") != ""
	})

	liveRun(t, sock, "set-option", "-gq", "@tt_status", "3 waiting · 1 error")
	liveWaitFor(t, sock, "the indicator to appear in the rendered status line WITHOUT refresh-client", func() bool {
		return strings.Contains(liveQuiet(sock, "capture-pane", "-p", "-t", "gate:nested"), "3 waiting")
	})

	liveRun(t, sock, "set-option", "-gu", "@tt_status")
	liveWaitFor(t, sock, "the indicator to disappear when the option is unset", func() bool {
		return !strings.Contains(liveQuiet(sock, "capture-pane", "-p", "-t", "gate:nested"), "3 waiting")
	})
}

// statusRightFromConf is the tt part of the status-right moox ships
// (share/moox/conf/status.conf): the conditional placeholder, so this case
// tests the shape the user will run.
func statusRightFromConf(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(tmuxConfPath(t)), "status.conf"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^set -g status-right "(#\{\?@tt_status,#\{E:@tt_status\} ,\})`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("the user's status-right does not start with the #{?@tt_status,…} placeholder")
	}
	return m[1] + "|rest"
}

// ---------------------------------------------------------------------------
// case 13: ONE event that fires SEVERAL hooks creates ONE observer
// ---------------------------------------------------------------------------

// P10's defect 1: its three hooks double-fired (new-window fires both
// after-new-window and window-linked), and two splits raced into one window.
// Here: a client attaching fires client-attached, client-session-changed and
// window-resized for the same window at once; a client's new-window fires
// session-window-changed (plus after-new-window and window-linked, which P10's
// set used). Each must end with ONE observer, stably.
func TestLiveCase13_OneEventThatFiresSeveralHooksCreatesOneObserver(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")

	liveRun(t, sock, "new-session", "-d", "-s", "attachme", "-x", "80", "-y", "24")
	attachClient(t, sock, "attachme") // 80 -> 220 columns: three hooks at once
	w := windowID(t, sock, "attachme:")
	if d.name == "p10" {
		// P10's hooks do not react to an attach at all; its double-fire is on
		// creation, so drive the creation route it does handle.
		liveRun(t, sock, "new-window", "-t", "attachme:")
		w = windowID(t, sock, "attachme:")
	}
	liveWaitFor(t, sock, "an observer after the multi-hook event", func() bool {
		all, _ := observers(sock, w)
		return all >= 1
	})
	stays(t, sock, w, 1, 1500*time.Millisecond)

	liveRun(t, sock, "new-window", "-t", "attachme:")
	w2 := windowID(t, sock, "attachme:")
	liveWaitFor(t, sock, "an observer in the new window", func() bool {
		all, _ := observers(sock, w2)
		return all >= 1
	})
	stays(t, sock, w2, 1, 1500*time.Millisecond)
}

// ---------------------------------------------------------------------------
// case 14: re-firing on a window that HAS an observer adds none
// ---------------------------------------------------------------------------

func TestLiveCase14_ReFiringOnAWindowThatHasAnObserverAddsNone(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")
	c := attachClient(t, sock, "gate")

	liveRun(t, sock, "new-window", "-t", "gate:", "-n", "target")
	w := windowID(t, sock, "gate:target")
	settled(t, sock, w, 1)

	liveRun(t, sock, "new-session", "-d", "-s", "elsewhere")
	for i := 0; i < 5; i++ {
		liveRun(t, sock, "select-window", "-t", "gate:0")
		liveRun(t, sock, "select-window", "-t", w)
		liveRun(t, sock, "switch-client", "-c", c.name, "-t", "elsewhere")
		liveRun(t, sock, "switch-client", "-c", c.name, "-t", "gate")
		liveRun(t, sock, "resize-window", "-t", w, "-x", strconv.Itoa(200+i))
		liveRun(t, sock, "resize-window", "-t", w, "-A")
		// Linking the window into another session is the event P10's set
		// reacted to; it must not add a pane either.
		liveRun(t, sock, "link-window", "-d", "-s", w, "-t", "elsewhere:")
		liveRun(t, sock, "unlink-window", "-t", "elsewhere:"+w)
	}
	stays(t, sock, w, 1, 1500*time.Millisecond)
}

// ---------------------------------------------------------------------------
// case 15: scope -> off leaves no orphan observer panes
// ---------------------------------------------------------------------------

// The decision, stated: automatic observers are RETIRED by the same reconcile
// that creates them, the next time their window is shown or resized — so
// turning scope off empties every window you look at, and the reboot path
// (resurrect's `exit` for a saved observer, case 10) never brings one back. A
// manual observer — one the user splits in with `tt observe --pane`, which is
// no longer what prefix C-o makes — is the user's, and scope does not touch it.
func TestLiveCase15_ScopeOffLeavesNoOrphanObservers(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	c := attachClient(t, sock, "gate")

	// A MANUAL observer first, while scope is still off: that one is the
	// user's, and turning scope off later must leave it alone. It is split in
	// by hand, because the key press is the scope's switch now and would open
	// an --auto pane (and turn the scope on).
	manual := windowID(t, sock, "gate:0")
	if d.name == "current" {
		liveRun(t, sock, "split-window", "-h", "-f", "-d", "-l", "46", "-t", manual,
			"/bin/sh", "-c", "tt observe --pane; exit")
	} else {
		c.toggle(t, d)
	}
	settled(t, sock, manual, 1)

	e.scope(t, "all")
	var windows []string
	for i := 0; i < 3; i++ {
		liveRun(t, sock, "new-window", "-t", "gate:")
		w := windowID(t, sock, "gate:")
		settled(t, sock, w, 1)
		windows = append(windows, w)
	}

	e.scope(t, "off")
	for _, w := range windows {
		liveRun(t, sock, "select-window", "-t", w)
	}
	liveRun(t, sock, "select-window", "-t", manual)
	for _, w := range windows {
		liveWaitFor(t, sock, "the automatic observer in "+w+" to retire at scope=off", func() bool {
			all, _ := observers(sock, w)
			return all == 0
		})
	}
	if d.name == "current" {
		stays(t, sock, manual, 1, 500*time.Millisecond)
	}
	// Nothing left running a renderer that nobody can see.
	for _, w := range windows {
		for _, p := range windowPanes(sock, w) {
			if p.cmd == "tt" {
				t.Fatalf("an orphan renderer is still running in %s\n%s", w, livePaneDump(sock))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// case 16: the width SETTING takes effect
// ---------------------------------------------------------------------------

func TestLiveCase16_TheWidthSettingTakesEffect(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	liveRun(t, sock, "set-option", "-g", "@tt_obs_width", "30")
	if d.name == "p10" {
		// §5.5(b2)'s documented hook, the one that carried the width as a
		// format. P10's own gate had replaced it with a literal 46.
		liveRun(t, sock, "set-hook", "-g", "after-new-window[50]",
			`if-shell -F "#{==:#{E:@tt_obs_scope},all}" "split-window -h -f -l #{E:@tt_obs_width} -d \"tt observe --pane\""`)
	}
	e.scope(t, "all")
	attachClient(t, sock, "gate")

	liveRun(t, sock, "new-window", "-t", "gate:")
	w := windowID(t, sock, "gate:")
	settled(t, sock, w, 1)
	for _, p := range windowPanes(sock, w) {
		if p.observer() && p.width != 30 {
			t.Fatalf("the observer is %d columns with @tt_obs_width=30\n%s", p.width, livePaneDump(sock))
		}
	}
}

// ---------------------------------------------------------------------------
// case 17: a fresh server keeps resurrect's "restoring from scratch" signal
// ---------------------------------------------------------------------------

// tmux-resurrect restores "from scratch" only when the server has EXACTLY ONE
// pane, and its handle_session_0 kills the bootstrap session "0" only then.
// Both boot paths on this machine (continuum-safe-restore.sh and tt's own
// restore) start a lone session "0", attach a client, and run the restore a
// moment later. An observer created in that moment would make it two panes:
// no from-scratch restore, and a session "0" left behind after every boot.
func TestLiveCase17_AFreshServerKeepsResurrectsFromScratchSignal(t *testing.T) {
	sock := liveServerSession(t, "0")
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")

	attachClient(t, sock, "0") // the boot client: attach + resize fire
	liveRun(t, sock, "resize-window", "-t", "0:", "-x", "200")
	stays(t, sock, "0:", 0, 1200*time.Millisecond)
	if n := len(strings.Fields(liveRun(t, sock, "list-panes", "-a", "-F", "#{pane_id}"))); n != 1 {
		t.Fatalf("the fresh server has %d panes, want exactly 1 (resurrect's from-scratch test)\n%s", n, livePaneDump(sock))
	}

	// Resurrect then creates the saved sessions DETACHED while "0" is still the
	// lone bootstrap; showing one of them must not create an observer yet...
	liveRun(t, sock, "new-session", "-d", "-s", "restored")
	liveRun(t, sock, "split-window", "-t", "restored:")
	liveRun(t, sock, "resize-window", "-t", "restored:", "-x", "210")
	stays(t, sock, "restored:", 0, 800*time.Millisecond)

	// ...and once "0" is gone (handle_session_0), showing it does.
	liveRun(t, sock, "kill-session", "-t", "0")
	liveRun(t, sock, "resize-window", "-t", "restored:", "-x", "220")
	settled(t, sock, windowID(t, sock, "restored:"), 1)
}

// ---------------------------------------------------------------------------
// cases 19–20: scope="sessions" — code review round 2, G2
// ---------------------------------------------------------------------------

// gateSession creates a detached session and returns its ID. Every command
// that follows targets the ID: a NAME given to -t is matched exactly, then as
// a prefix, then AS A GLOB (man tmux, "target-session"), which is the very
// hazard these cases are about.
func gateSession(t *testing.T, sock, name string) string {
	t.Helper()
	return liveRun(t, sock, "new-session", "-d", "-P", "-F", "#{session_id}", "-s", name)
}

// showSession switches the attached client to a session, which fires
// client-session-changed there — the route by which a session's window is
// shown.
func showSession(t *testing.T, sock string, c *liveClient, id string) {
	t.Helper()
	liveRun(t, sock, "switch-client", "-c", c.name, "-t", id)
}

// TestLiveCase19_ScopeSessionsPutsAnObserverOnlyInTheListedSessions: two
// sessions listed and one not; each is shown; observers appear in exactly the
// listed ones — in every window of them — and nowhere else; and taking a
// session off the list retires its automatic observer the next time it is
// shown, like scope off does (case 15).
func TestLiveCase19_ScopeSessionsPutsAnObserverOnlyInTheListedSessions(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name == "p10" {
		t.Skip("P10's design had no scope=sessions")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	liveRun(t, sock, "set-option", "-g", "@tt_obs_sessions", "alpha gamma")
	e.scope(t, "sessions")

	alpha := gateSession(t, sock, "alpha")
	beta := gateSession(t, sock, "beta")
	gamma := gateSession(t, sock, "gamma")
	c := attachClient(t, sock, "gate")

	// The session the client attached to is not listed.
	stays(t, sock, windowID(t, sock, "gate:"), 0, 700*time.Millisecond)

	for _, s := range []struct {
		id, name string
		want     int
	}{{alpha, "alpha", 1}, {beta, "beta", 0}, {gamma, "gamma", 1}} {
		showSession(t, sock, c, s.id)
		w := windowID(t, sock, s.id+":")
		if s.want == 1 {
			settled(t, sock, w, 1)
		}
		stays(t, sock, w, s.want, 700*time.Millisecond)
		t.Logf("session %-6s (listed=%v) -> window %s: %d observer(s)", s.name, s.want == 1, w, s.want)
	}

	// A second window of a listed session gets its own when shown; one of the
	// unlisted session does not.
	liveRun(t, sock, "new-window", "-t", gamma+":")
	settled(t, sock, windowID(t, sock, gamma+":"), 1)
	showSession(t, sock, c, beta)
	liveRun(t, sock, "new-window", "-t", beta+":")
	stays(t, sock, windowID(t, sock, beta+":"), 0, 700*time.Millisecond)

	// Off the list: alpha's automatic observer retires when alpha is next
	// shown, and gamma's stays.
	liveRun(t, sock, "set-option", "-g", "@tt_obs_sessions", "gamma")
	showSession(t, sock, c, alpha)
	liveWaitFor(t, sock, "alpha's automatic observer to retire once alpha is off the list", func() bool {
		all, _ := observers(sock, windowID(t, sock, alpha+":"))
		return all == 0
	})
	showSession(t, sock, c, gamma)
	stays(t, sock, windowID(t, sock, gamma+":"), 1, 700*time.Millisecond)

	// Nowhere else, ever: exactly the three observers the listed sessions'
	// shown windows should have had, minus alpha's.
	total := 0
	for _, w := range allWindows(sock) {
		all, _ := observers(sock, w)
		total += all
	}
	if total != 2 {
		t.Fatalf("%d observers on the server, want gamma's 2\n%s", total, livePaneDump(sock))
	}
}

// TestLiveCase20_SessionNamesAreMatchedExactlyNotAsGlobs: session names that
// contain glob metacharacters. The block as it shipped put the session name on
// the PATTERN side of `#{m:* #{session_name} *, #{@tt_obs_sessions} }`, so an
// unlisted `al*` or `alph?` matched a listed "alpha", a listed `w[1]` did NOT
// match itself (its [1] is a bracket expression matching "1"), and an unlisted
// name with a space matched two adjacent listed words.
func TestLiveCase20_SessionNamesAreMatchedExactlyNotAsGlobs(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name == "p10" {
		t.Skip("P10's design had no scope=sessions")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	liveRun(t, sock, "set-option", "-g", "@tt_obs_sessions", "alpha gamma w[1]")
	e.scope(t, "sessions")

	cases := []struct {
		name string
		want bool
	}{
		{"alpha", true},
		{"gamma", true},
		{"w[1]", true},         // a bracket expression if it were a pattern
		{"alph?", false},       // `?` would match the "a" of alpha
		{"al*", false},         // `*` would match "pha"
		{"[ag]lpha", false},    // would match alpha
		{"w1", false},          // what w[1] matched as a pattern, reversed
		{"alpha gamma", false}, // two adjacent listed words
		{"Alpha", false},       // tmux names are case-sensitive, and so is this
		{"beta", false},
	}
	ids := map[string]string{}
	for _, cse := range cases {
		ids[cse.name] = gateSession(t, sock, cse.name)
		// tmux keeps the name as given: the test is about THESE names.
		if got := liveRun(t, sock, "display-message", "-p", "-t", ids[cse.name], "#{session_name}"); got != cse.name {
			t.Fatalf("tmux named the session %q, want %q", got, cse.name)
		}
	}

	// The predicate itself, evaluated by tmux in each session — and the
	// escaped name it matches with, so the log shows what q: produced.
	for _, cse := range cases {
		got := liveRun(t, sock, "display-message", "-p", "-t", ids[cse.name], "#{E:@tt_obs_in_scope}")
		quoted := liveRun(t, sock, "display-message", "-p", "-t", ids[cse.name], "#{q:session_name}")
		t.Logf("in_scope(%-13q) = %s   (#{q:session_name} = %s)", cse.name, got, quoted)
		if (got == "1") != cse.want {
			t.Errorf("session %q: @tt_obs_in_scope = %s, want in scope = %v", cse.name, got, cse.want)
		}
	}

	// And the behaviour it decides: an observer exactly where the name is
	// listed, verbatim.
	c := attachClient(t, sock, "gate")
	for _, cse := range cases {
		showSession(t, sock, c, ids[cse.name])
		w := windowID(t, sock, ids[cse.name]+":")
		want := 0
		if cse.want {
			want = 1
			settled(t, sock, w, 1)
		}
		stays(t, sock, w, want, 500*time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// case 21: the TUI's ^o panel as a real terminal paints it — code review
// round 2, A9
// ---------------------------------------------------------------------------

// TestLiveCase21_TheTUIPaintsItsPanelInsideTheTerminal runs the bare `tt` TUI
// — the thing the user runs daily — in a pane of the isolated server, presses
// ^o on a pty client, and reads back what tmux PAINTED. panel_fit_test.go
// proves the layout against a model of Bubble Tea's renderer; this is the
// renderer itself, at the two sizes the review named. The panel reads this
// machine's REAL ~/.claude, read-only (as the resource case does), so how many
// rows need attention is whatever is true right now; what is asserted is what
// must hold for any data: both titles on the first painted line, the panel's
// column exactly where the layout put it, and nothing painted past the edge.
func TestLiveCase21_TheTUIPaintsItsPanelInsideTheTerminal(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("the panel under test is this tree's")
	}
	e := newGateEnv(t, sock, d)
	home, _ := os.UserHomeDir()
	liveRun(t, sock, "set-environment", "-g", "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	e.window(t, "tui", "")
	c := attachClient(t, sock, "gate")
	liveRun(t, sock, "select-window", "-t", "gate:tui")

	for i, size := range []struct{ w, h int }{{140, 30}, {100, 24}} {
		liveRun(t, sock, "resize-window", "-t", "gate:tui", "-x", strconv.Itoa(size.w), "-y", strconv.Itoa(size.h))
		if i == 0 {
			liveWaitFor(t, sock, "the TUI to draw its list", func() bool {
				return strings.Contains(liveQuiet(sock, "capture-pane", "-p", "-t", "gate:tui"), "tmux sessions")
			})
			if _, err := c.in.Write([]byte{0x0f}); err != nil { // ctrl+o
				t.Fatalf("typing ^o: %v", err)
			}
		}
		panelCol := size.w - panelColumns(size.w)
		var painted []string
		defer func(w, h int) {
			if t.Failed() && painted != nil {
				t.Logf("%dx%d: the last frame tmux painted:\n%s", w, h, strings.Join(painted, "\n"))
			}
		}(size.w, size.h)
		liveWaitForIn(t, sock, fmt.Sprintf("%dx%d: the panel's title at column %d of the first painted line", size.w, size.h, panelCol),
			15*time.Second, func() bool {
				raw, err := liveTmux(sock, "capture-pane", "-p", "-t", "gate:tui").Output()
				if err != nil {
					return false
				}
				painted = strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
				if len(painted) < 2 {
					return false
				}
				// The panel's title is its row of grouping tabs, centred over
				// a baseline that spans the panel exactly: the baseline's first
				// rune is the panel's column.
				first, base := []rune(painted[0]), []rune(painted[1])
				col := len(base) - len([]rune(strings.TrimLeft(painted[1], " ")))
				return len(first) > panelCol && col == panelCol &&
					(base[col] == '─' || base[col] == '━') &&
					strings.Contains(string(first[:panelCol]), "tmux sessions") &&
					strings.HasPrefix(strings.TrimLeft(string(first[panelCol:]), " "), "Project │ Status")
			})
		frame := strings.Join(painted, "\n")
		// The panel has data (a snapshot arrived) rather than "waiting…".
		liveWaitForIn(t, sock, "the panel's first snapshot", 15*time.Second, func() bool {
			out := liveQuiet(sock, "capture-pane", "-p", "-t", "gate:tui")
			frame = out
			return !strings.Contains(out, "waiting for the observer")
		})
		for n, l := range strings.Split(frame, "\n") {
			if w := len([]rune(l)); w > size.w {
				t.Errorf("%dx%d: painted line %d is %d columns wide", size.w, size.h, n, w)
			}
		}
		if n := len(strings.Split(frame, "\n")); n > size.h {
			t.Errorf("%dx%d: %d painted lines", size.w, size.h, n)
		}
		t.Logf("%dx%d, painted by tmux (real ~/.claude, read-only):\n%s", size.w, size.h, frame)
	}
	if _, err := c.in.Write([]byte{0x1b}); err != nil { // esc: quit, attach nothing
		t.Fatalf("typing esc: %v", err)
	}
}

// ---------------------------------------------------------------------------
// case 10: TWO CONSECUTIVE REBOOT + RESTORE ROUNDS, with the REAL resurrect
// ---------------------------------------------------------------------------

// resurrectScripts is the installed tmux-resurrect, or "" if there is none.
func resurrectScripts() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".tmux", "plugins", "tmux-resurrect", "scripts")
	if _, err := os.Stat(filepath.Join(dir, "restore.sh")); err != nil {
		return ""
	}
	return dir
}

// setResurrect mirrors the user's resurrect settings that matter here, with
// the snapshot directory PRIVATE to this test. Nothing in this test can read
// or write the user's own snapshots: @resurrect-dir points into t.TempDir(),
// and the scripts also run with HOME pointing there, so even resurrect's
// default directory is a temp one.
func (e *gateEnv) setResurrect(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(e.root, "resurrect")
	liveRun(t, e.sock, "set-option", "-g", "@resurrect-dir", dir)
	liveRun(t, e.sock, "set-option", "-g", "@resurrect-save-command-strategy", "pgrep")
	liveRun(t, e.sock, "set-option", "-g", "@resurrect-capture-pane-contents", "on")
	return dir
}

// runResurrect runs one of resurrect's own scripts against the PRIVATE server.
// Its environment is built from nothing: $TMUX names the private socket, PATH
// starts with a `tmux` wrapper that forces both guards, and HOME is a temp dir.
func (e *gateEnv) runResurrect(t *testing.T, script string, args ...string) {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrap := filepath.Join(e.root, "resurrect-wrap")
	if err := os.MkdirAll(wrap, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + real + " -f /dev/null -L " + e.sock + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrap, "tmux"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(e.root, "resurrect-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := liveRun(t, e.sock, "display-message", "-p", "#{socket_path}")
	cmd := exec.Command("bash", append([]string{filepath.Join(resurrectScripts(), script)}, args...)...)
	cmd.Env = []string{
		"PATH=" + wrap + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"TMUX=" + socketPath + ",0,0",
		"HOME=" + home,
		"TERM=xterm-256color",
	}
	done := make(chan struct{})
	var out []byte
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("resurrect %s did not finish in 90s", script)
	}
	if err != nil {
		t.Fatalf("resurrect %s: %v\n%s", script, err, out)
	}
}

// savedWindow is one window of the workspace, keyed by session:index (ids do
// not survive a reboot; resurrect restores by index).
type savedWindow struct {
	key    string
	titles []string // user panes, in pane-index order
	side   bool     // the user panes were laid out side by side
}

func TestLiveCase10_TwoRestoreRoundsConvergeToOneObserverPerWindow(t *testing.T) {
	if resurrectScripts() == "" {
		t.Skip("tmux-resurrect is not installed")
	}
	sock := liveServerSession(t, "work")
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	resDir := e.setResurrect(t)
	e.source(t)
	e.scope(t, "all")

	// The workspace: work:0 (2 panes), work:1 (1 pane), dev:0 (3 panes, side
	// by side so a layout that failed to apply is visible). Every user pane is
	// titled, so a pane that lands in someone else's index is caught.
	liveRun(t, sock, "split-window", "-h", "-t", "work:0")
	liveRun(t, sock, "new-window", "-d", "-t", "work:1")
	liveRun(t, sock, "new-session", "-d", "-s", "dev")
	liveRun(t, sock, "split-window", "-h", "-t", "dev:0")
	liveRun(t, sock, "split-window", "-h", "-t", "dev:0")
	liveRun(t, sock, "select-layout", "-t", "dev:0", "even-horizontal")
	windows := []*savedWindow{{key: "work:0"}, {key: "work:1"}, {key: "dev:0", side: true}}
	for _, w := range windows {
		for _, p := range windowPanes(sock, w.key) {
			title := fmt.Sprintf("user-%s-%d", strings.ReplaceAll(w.key, ":", "-"), p.index)
			liveRun(t, sock, "select-pane", "-t", p.id, "-T", title)
			w.titles = append(w.titles, title)
		}
	}

	// Show every window, so each gets its observer.
	c := attachClient(t, sock, "work")
	liveRun(t, sock, "select-window", "-t", "work:1")
	liveRun(t, sock, "select-window", "-t", "work:0")
	liveRun(t, sock, "switch-client", "-c", c.name, "-t", "dev")
	for _, w := range windows {
		settled(t, sock, w.key, 1)
	}

	verify := func(round int) {
		t.Helper()
		for _, w := range windows {
			w := w
			var last string
			defer func() {
				if t.Failed() {
					t.Logf("round %d %s: last evaluation: %s", round, w.key, last)
				}
			}()
			liveWaitForIn(t, sock, fmt.Sprintf("round %d: %s restored with its users and ONE observer", round, w.key),
				20*time.Second, func() bool {
					ps := windowPanes(sock, w.key)
					var users []paneInfo
					obs, flagged := 0, 0
					for _, p := range ps {
						if p.observer() {
							obs++
							if p.flag == "1" {
								flagged++
							}
							continue
						}
						users = append(users, p)
					}
					last = fmt.Sprintf("observers=%d flagged=%d users=%d (saved %d) panes=%d", obs, flagged, len(users), len(w.titles), len(ps))
					if obs != 1 || flagged != 1 || len(users) != len(w.titles) {
						return false
					}
					return true
				})
			stays(t, sock, w.key, 1, 1000*time.Millisecond)

			// Every user pane is back IN ITS OWN INDEX: an observer created
			// mid-restore takes a saved pane's slot, and resurrect then skips
			// that saved pane ("pane exists, no need to create it").
			var got []string
			for _, p := range windowPanes(sock, w.key) {
				if !p.observer() {
					got = append(got, liveRun(t, sock, "display-message", "-p", "-t", p.id, "#{pane_title}"))
				}
			}
			if strings.Join(got, ",") != strings.Join(w.titles, ",") {
				t.Fatalf("round %d: %s user panes by index are %v, saved %v\n%s",
					round, w.key, got, w.titles, livePaneDump(sock))
			}
			// The saved LAYOUT was applied: an observer that existed before
			// select-layout makes it fail silently, and dev:0 would come back
			// stacked (resurrect's own splits) instead of side by side.
			if w.side {
				for _, p := range windowPanes(sock, w.key) {
					if top := liveRun(t, sock, "display-message", "-p", "-t", p.id, "#{pane_top}"); !p.observer() && top != "0" {
						t.Fatalf("round %d: %s was not restored side by side (pane %s top=%s): select-layout did not apply\n%s",
							round, w.key, p.id, top, livePaneDump(sock))
					}
				}
			}
		}
		// Resurrect saw a from-scratch server and removed its bootstrap.
		if strings.Contains(" "+strings.Join(strings.Fields(liveRun(t, sock, "list-sessions", "-F", "#{session_name}")), " ")+" ", " 0 ") {
			t.Fatalf("round %d: session 0 survived the restore: resurrect did not see a from-scratch server", round)
		}
		total := 0
		for _, w := range allWindows(sock) {
			all, _ := observers(sock, w)
			total += all
		}
		if total != len(windows) {
			t.Fatalf("round %d: %d observers over %d windows\n%s", round, total, len(windows), livePaneDump(sock))
		}
		t.Logf("round %d: %d windows, %d observers, every user pane in its saved index, layouts applied",
			round, len(windows), total)
	}

	for round := 1; round <= 2; round++ {
		e.runResurrect(t, "save.sh", "quiet")
		last, err := os.ReadFile(filepath.Join(resDir, "last"))
		if err != nil {
			t.Fatalf("round %d: no snapshot: %v", round, err)
		}
		if n := bytes.Count(last, []byte("tt observe --pane")); n != len(windows) {
			t.Fatalf("round %d: the snapshot names %d observer processes, want %d (one per window) — "+
				"without them the exit mapping cannot fire:\n%s", round, n, len(windows), last)
		}

		// REBOOT: the server and its client go away; a fresh server starts
		// with resurrect's bootstrap session "0" and a client attached to it,
		// which is exactly the shape both boot paths on this machine produce.
		c.kill()
		_ = liveTmux(sock, "kill-server").Run()
		liveWaitFor(t, sock, "the old server to be gone", func() bool {
			return liveTmux(sock, "list-sessions").Run() != nil
		})
		startLiveServer(t, sock, "0")
		e.install(t)
		e.setResurrect(t)
		e.source(t)
		e.scope(t, "all")
		c = attachClient(t, sock, "0")

		e.runResurrect(t, "restore.sh")
		verify(round)
	}
}

// ---------------------------------------------------------------------------
// case 18: the status line follows REAL state changes, with no timer, and is
// cleared when the observer stops
// ---------------------------------------------------------------------------

// writeRegistryRecord writes one interactive session record into the fixture
// tree, pointing at a real process and a real pane of the private server, so
// the observer's liveness gate (kill -0 AND the pane exists) says live.
func (e *gateEnv) writeRegistryRecord(t *testing.T, pid, ref, name, status string) {
	t.Helper()
	now := time.Now().UnixMilli()
	waiting := ""
	if status == "waiting" {
		waiting = `, "waitingFor": "input needed"`
	}
	rec := fmt.Sprintf(`{"pid": %s, "sessionId": "00000000-0000-4000-8000-%012s", "cwd": "/tmp", `+
		`"startedAt": %d, "version": "2.1.276", "kind": "interactive", "entrypoint": "cli", "pidDomain": "darwin", `+
		`"tmux": %q, "name": %q, "nameSource": "user", "updatedAt": %d, "status": %q, "statusUpdatedAt": %d%s}`,
		pid, pid, now, ref, name, now, status, now, waiting)
	if err := os.WriteFile(filepath.Join(e.claude, "sessions", pid+".json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeSession is a sleeping pane standing in for a Claude process: its pid and
// its pane reference are what a registry record carries.
func (e *gateEnv) fakeSession(t *testing.T, name string) (pid, ref string) {
	t.Helper()
	liveRun(t, e.sock, "new-window", "-d", "-t", "gate:", "-n", name, "sleep 600")
	pid = liveRun(t, e.sock, "display-message", "-p", "-t", "gate:"+name, "#{pane_pid}")
	ref = liveRun(t, e.sock, "display-message", "-p", "-t", "gate:"+name, "#{session_name}:#{window_id}.#{pane_id}")
	return pid, ref
}

func countLines(path, needle string) int {
	data, _ := os.ReadFile(path)
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, needle) {
			n++
		}
	}
	return n
}

func TestLiveCase18_TheStatusLineFollowsRealStateChangesAndClearsOnExit(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	e := newGateEnv(t, sock, d)
	calls := e.callLog()
	liveRun(t, sock, "set-option", "-g", "status", "on")
	liveRun(t, sock, "set-option", "-g", "status-interval", "0")
	liveRun(t, sock, "set-option", "-g", "status-right", statusRightFromConf(t))
	liveRun(t, sock, "new-window", "-d", "-n", "nested",
		"sh -c 'TMUX= exec tmux -f /dev/null -L "+sock+" attach -t gate'")
	liveWaitFor(t, sock, "the nested client", func() bool {
		return liveQuiet(sock, "list-clients", "-F", "#{client_name}") != ""
	})
	painted := func(s string) func() bool {
		return func() bool { return strings.Contains(liveQuiet(sock, "capture-pane", "-p", "-t", "gate:nested"), s) }
	}
	option := func() string { return liveQuiet(sock, "show-options", "-gqv", "@tt_status") }

	pid1, ref1 := e.fakeSession(t, "claude1")
	e.writeRegistryRecord(t, pid1, ref1, "first", "waiting")

	// remain-on-exit for the server's window only, so a server that dies at
	// startup leaves its message on screen for the failure report.
	e.window(t, "server", "observe")
	liveRun(t, sock, "set-option", "-w", "-t", "gate:server", "remain-on-exit", "on")
	defer func() {
		if t.Failed() {
			t.Logf("server pane:\n%s", liveQuiet(sock, "capture-pane", "-p", "-t", "gate:server"))
			t.Logf("tmux calls:\n%s", mustRead(calls))
		}
	}()
	// The counter is a badge now: a glyph and a count on a fill, so what is
	// asserted is the count inside the waiting badge rather than a sentence.
	waitingBadge := func(n int) func() bool {
		return func() bool { return strings.Contains(option(), fmt.Sprintf("bold] \uf0f3 %d #[default]", n)) }
	}
	liveWaitForIn(t, sock, "@tt_status to carry a waiting badge of 1", 15*time.Second, waitingBadge(1))
	liveWaitFor(t, sock, "the status bar to paint it", painted("\uf0f3 1"))

	// A second session starts waiting: a registry file APPEARS.
	pid2, ref2 := e.fakeSession(t, "claude2")
	e.writeRegistryRecord(t, pid2, ref2, "second", "waiting")
	liveWaitFor(t, sock, "a waiting badge of 2", waitingBadge(2))
	liveWaitFor(t, sock, "the status bar to paint 2", painted("\uf0f3 2"))

	// The first goes busy: a registry file is rewritten IN PLACE.
	e.writeRegistryRecord(t, pid1, ref1, "first", "busy")
	liveWaitFor(t, sock, "back to a waiting badge of 1", waitingBadge(1))

	// Nothing changes for three seconds: nothing is written. No timer, no poll.
	before := countLines(calls, "@tt_status")
	time.Sleep(3 * time.Second)
	if after := countLines(calls, "@tt_status"); after != before {
		t.Fatalf("%d @tt_status writes in a quiet 3 s — something is polling", after-before)
	}
	writes := countLines(calls, "set-option -gq @tt_status")
	if writes != 3 {
		t.Fatalf("%d set-option calls for 3 distinct states, want exactly 3\n%s", writes, mustRead(calls))
	}
	if d.name == "current" {
		if n := countLines(calls, "refresh-client"); n != 0 {
			t.Fatalf("%d refresh-client calls; the set-option alone repaints", n)
		}
	}

	// A clean stop clears it.
	e.window(t, "stopper", "observe --stop")
	liveWaitFor(t, sock, "@tt_status to be unset after --stop", func() bool { return option() == "" })
	liveWaitFor(t, sock, "the status bar to be clean", func() bool { return !painted("waiting")() })
	if n := countLines(calls, "set-option -gu @tt_status"); n != 1 {
		t.Fatalf("%d unsets on stop, want 1", n)
	}
	t.Logf("tmux calls made by tt observe:\n%s", mustRead(calls))
}

func mustRead(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// ---------------------------------------------------------------------------
// the resource cost of scope="all": 8 windows, one server, thin renderers
// ---------------------------------------------------------------------------

// procStat is one process's footprint. treeFDs counts descriptors open on the
// Claude tree — the watch set. kqueues alone does not say "watching": every Go
// process on darwin holds one for the runtime's netpoller, and bubbletea's
// cancelable stdin reader opens a second, so a renderer's baseline is 2
// (measured) and the watch shows up as tree descriptors, not as a kqueue.
type procStat struct {
	pid          string
	rssKB, fds   int
	kqueues, uds int
	treeFDs      int
	cpu          string
}

// measureTree is the Claude root whose descriptors measureProc counts.
var measureTree string

func measureProc(t *testing.T, pid string) procStat {
	t.Helper()
	s := procStat{pid: pid}
	out, _ := exec.Command("ps", "-o", "rss=,time=", "-p", pid).Output()
	if f := strings.Fields(string(out)); len(f) == 2 {
		s.rssKB, _ = strconv.Atoi(f[0])
		s.cpu = f[1]
	}
	lsof, _ := exec.Command("/usr/sbin/lsof", "-n", "-P", "-p", pid).Output()
	for i, l := range strings.Split(string(lsof), "\n") {
		if i == 0 || strings.TrimSpace(l) == "" {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		// Only numbered descriptors count; cwd/txt/rtd are not descriptors.
		if fd := f[3]; len(fd) == 0 || fd[0] < '0' || fd[0] > '9' {
			continue
		}
		s.fds++
		switch f[4] {
		case "KQUEUE":
			s.kqueues++
		case "unix":
			s.uds++
		}
		if measureTree != "" && strings.Contains(l, measureTree) {
			s.treeFDs++
		}
	}
	return s
}

// childTT is the tt process under an observer pane's /bin/sh.
func childTT(pid string) string {
	out, _ := exec.Command("pgrep", "-P", pid).Output()
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func TestLiveResourceCost_ScopeAllAcrossEightWindows(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("the measurement is of this change's design")
	}
	e := newGateEnv(t, sock, d)
	// The SERVER watches the REAL Claude tree — read-only, per AC13 — so its
	// numbers are this machine's, not a fixture's.
	home, _ := os.UserHomeDir()
	liveRun(t, sock, "set-environment", "-g", "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	measureTree = filepath.Join(home, ".claude") + "/"
	defer func() { measureTree = "" }()
	e.source(t)
	e.scope(t, "all")

	e.window(t, "server", "observe")
	liveWaitForIn(t, sock, "the observer socket", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(e.ttDir(), "observer.sock"))
		return err == nil
	})
	c := attachClient(t, sock, "gate")

	var wins []string
	for i := 0; i < 8; i++ {
		liveRun(t, sock, "new-window", "-t", "gate:")
		w := windowID(t, sock, "gate:")
		settled(t, sock, w, 1)
		wins = append(wins, w)
	}
	// Every renderer is ATTACHED (its footer says so), not waiting.
	for _, w := range wins {
		w := w
		liveWaitFor(t, sock, "the renderer in "+w+" to attach", func() bool {
			for _, p := range windowPanes(sock, w) {
				if p.observer() && liveAttached(liveQuiet(sock, "capture-pane", "-p", "-t", p.id)) {
					return true
				}
			}
			return false
		})
	}
	_ = c

	// The window's shell execs tt, so the pane's pid IS the server.
	serverPid := liveRun(t, sock, "display-message", "-p", "-t", "gate:server", "#{pane_pid}")
	server := measureProc(t, serverPid)

	var shells, renderers []procStat
	for _, w := range wins {
		for _, p := range windowPanes(sock, w) {
			if !p.observer() {
				continue
			}
			sh := liveRun(t, sock, "display-message", "-p", "-t", p.id, "#{pane_pid}")
			shells = append(shells, measureProc(t, sh))
			renderers = append(renderers, measureProc(t, childTT(sh)))
		}
	}
	if len(renderers) != 8 {
		t.Fatalf("measured %d renderers, want 8", len(renderers))
	}
	if server.treeFDs == 0 {
		t.Fatalf("the server holds no descriptor on the Claude tree; the measurement is not seeing its watch")
	}
	for _, r := range renderers {
		if r.treeFDs != 0 {
			t.Fatalf("renderer pid %s holds %d descriptors on the Claude tree: it is watching for itself (MEDIUM 5)", r.pid, r.treeFDs)
		}
		if r.uds < 1 {
			t.Fatalf("renderer pid %s holds no unix socket: it is not attached to the server", r.pid)
		}
	}

	// Contrast: ONE pane that watches for itself (scope off, no server in its
	// socket dir) — the per-pane cost §5.5(b2) exists to avoid.
	e.scope(t, "off")
	empty := t.TempDir()
	e.window(t, "local", "observe --pane --socket-dir "+empty)
	var local procStat
	liveWaitForIn(t, sock, "the self-watching pane to arm its watch", 15*time.Second, func() bool {
		pid := liveQuiet(sock, "display-message", "-p", "-t", "gate:local", "#{pane_pid}")
		if pid == "" {
			return false
		}
		local = measureProc(t, pid)
		return local.treeFDs > 0
	})

	sum := func(ps []procStat) (rss, fds int) {
		for _, p := range ps {
			rss += p.rssKB
			fds += p.fds
		}
		return
	}
	rRSS, rFDs := sum(renderers)
	sRSS, sFDs := sum(shells)
	t.Logf("RESOURCE COST, scope=all over 8 windows (processes, descriptors, RSS):")
	t.Logf("  server   tt observe       pid %-6s rss %6d KB  fds %3d  on-tree %3d  kqueues %d  unix %d",
		server.pid, server.rssKB, server.fds, server.treeFDs, server.kqueues, server.uds)
	for i, r := range renderers {
		t.Logf("  pane %d   tt --pane --auto pid %-6s rss %6d KB  fds %3d  on-tree %3d  kqueues %d  unix %d | sh pid %-6s rss %5d KB fds %d",
			i+1, r.pid, r.rssKB, r.fds, r.treeFDs, r.kqueues, r.uds, shells[i].pid, shells[i].rssKB, shells[i].fds)
	}
	t.Logf("  TOTAL    1 server + 8 renderers + 8 sh = 17 processes; RSS %d KB (renderers %d + sh %d + server %d); fds %d (renderers %d + sh %d + server %d)",
		rRSS+sRSS+server.rssKB, rRSS, sRSS, server.rssKB, rFDs+sFDs+server.fds, rFDs, sFDs, server.fds)
	t.Logf("  CONTRAST one self-watching pane: pid %s rss %d KB fds %d on-tree %d kqueues %d — x8 would be %d fds, %d on the tree, 8 watch sets",
		local.pid, local.rssKB, local.fds, local.treeFDs, local.kqueues, 8*local.fds, 8*local.treeFDs)

	// Idle CPU over 20 s: the renderers tick their clock once a second while on
	// screen (the one permitted clock, §5.5a); the server does nothing.
	cpu0 := map[string]string{}
	for _, r := range renderers {
		cpu0[r.pid] = measureProc(t, r.pid).cpu
	}
	cpu0[server.pid] = measureProc(t, server.pid).cpu
	time.Sleep(20 * time.Second)
	for _, r := range renderers {
		t.Logf("  idle 20s cpu: renderer %s %s -> %s", r.pid, cpu0[r.pid], measureProc(t, r.pid).cpu)
	}
	t.Logf("  idle 20s cpu: server   %s %s -> %s", server.pid, cpu0[server.pid], measureProc(t, server.pid).cpu)
}
func TestLiveCase12_WithNoPlaceholderTheWriterIssuesZeroSetOptionCalls(t *testing.T) {
	sock := liveServer(t)
	liveRun(t, sock, "set-option", "-g", "status-right", "no placeholder here")

	// A counting wrapper named `tmux`, first on PATH, so every call the writer
	// makes is recorded before reaching the real binary.
	dir := t.TempDir()
	log := dir + "/calls.log"
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not on PATH")
	}
	// The wrapper forwards to the ISOLATED server (both guards, again), so
	// that even a writer which wrongly wrote could not reach the user's own
	// tmux — the assertion is that it writes nothing, and the guard is that it
	// could not matter if it did.
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + log +
		"\nexec " + real + " -f /dev/null -L " + sock + " \"$@\"\n"
	if err := os.WriteFile(dir+"/tmux", []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The writer, through its real constructor and its real publish path.
	w := newStatusLineWriter(statusLineAuto)
	if w.Enabled() {
		t.Fatal("the writer enabled itself with no @tt_status in status-right")
	}
	w.Publish(nil)
	w.Clear()

	data, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "set-option") || strings.Contains(line, "refresh-client") {
			t.Fatalf("a non-opted-in user's tmux received a write: %q", line)
		}
	}
}

// TestLiveAC8_SigilsSurviveTheRoundTripAndAStrippedIDDoesNotResolve is AC8, and
// it is P10's, not P9's: it needs a tmux server of its own.
//
// Measured fact 7 is that the registry's `tmux` field is
// `#{session_name}:#{window_id}.#{pane_id}` and that `@` and `%` are PART of the
// identifiers. The failure mode it guards is the nastiest kind: a reader that
// strips the sigils produces ids tmux will not match, and tmux does not
// complain — `list-panes -t` on an unknown target simply reports no such pane,
// and a liveness gate built on it would quietly report every session's pane as
// gone. The second half of this test is therefore the important half: it proves
// the silent failure is REAL rather than theoretical, which is what turns
// "keep the sigils" from a style note into a requirement.
func TestLiveAC8_SigilsSurviveTheRoundTripAndAStrippedIDDoesNotResolve(t *testing.T) {
	sock := liveServer(t)

	// Push the window and pane IDS far past any index, which is the shape the
	// live registry actually carries (measured: `madbench:@53.%109` on a server
	// with 36 windows). CORRECTED AFTER THE FIRST RUN: with @1/%1 on a
	// one-window server, the sigil-stripped reference `gate:1.1` resolved to
	// the same pane by coincidence — a bare number is an INDEX, and index 1
	// existed. The test passed the stripping and proved nothing. IDs never get
	// reused, so creating and destroying windows is how they are made to
	// diverge from indexes the way they do on a real server.
	for i := 0; i < 20; i++ {
		liveRun(t, sock, "new-window", "-d", "-n", fmt.Sprintf("churn-%d", i))
		liveRun(t, sock, "kill-window", "-t", fmt.Sprintf("gate:churn-%d", i))
	}
	liveRun(t, sock, "new-window", "-d", "-n", "ac8")

	ref := liveRun(t, sock, "display-message", "-p", "-t", "gate:ac8",
		"#{session_name}:#{window_id}.#{pane_id}")
	t.Logf("the reference tmux reports: %s", ref)

	// 1. The shape, and the sigils in it.
	parts := liveRefPattern.FindStringSubmatch(ref)
	if parts == nil {
		t.Fatalf("the reference %q does not match the anchored pattern %s", ref, liveRefPattern)
	}
	session, window, pane := parts[1], parts[2], parts[3]
	if !strings.HasPrefix(window, "@") || !strings.HasPrefix(pane, "%") {
		t.Fatalf("the sigils did not survive: window=%q pane=%q", window, pane)
	}
	if session != "gate" {
		t.Fatalf("session name round-tripped as %q, want gate", session)
	}

	// 2. The reference tmux gave us resolves back to the same pane.
	back := liveRun(t, sock, "display-message", "-p", "-t", ref, "#{pane_id}")
	if back != pane {
		t.Fatalf("the reference did not resolve back to its own pane: %q -> %q", ref, back)
	}

	// 3. THE REFERENCE A NAIVE READER PRODUCES — the whole thing with both
	//    sigils removed, which is what `strings.ReplaceAll(ref, "@", "")` and
	//    friends give you — does not resolve to this pane.
	//
	//    The bare pane number is deliberately NOT the test, and finding that out
	//    is why this ran before it was believed: `display-message -t 1` DOES
	//    resolve, because a bare number is a window INDEX, and on a one-window
	//    server it happened to answer with the same pane. A test written against
	//    the bare id would have "passed" by coincidence and proved nothing.
	strippedRef := session + ":" + strings.TrimPrefix(window, "@") + "." + strings.TrimPrefix(pane, "%")
	out, err := liveTmux(sock, "display-message", "-p", "-t", strippedRef, "#{pane_id}").CombinedOutput()
	got := strings.TrimSpace(string(out))
	if err == nil && got == pane {
		t.Fatalf("the stripped reference %q resolved to %s; fact 7's hazard would not exist", strippedRef, pane)
	}
	t.Logf("stripped reference %q: err=%v output=%q — fact 7's silent failure, reproduced",
		strippedRef, err, got)

	// 4. And with only the WINDOW sigil removed, because a reader could strip
	//    either one and the pane half alone would still look right.
	windowStripped := session + ":" + strings.TrimPrefix(window, "@") + "." + pane
	out, err = liveTmux(sock, "display-message", "-p", "-t", windowStripped, "#{pane_id}").CombinedOutput()
	got = strings.TrimSpace(string(out))
	if err == nil && got == pane {
		t.Fatalf("the window-stripped reference %q resolved; fact 7 covers both sigils", windowStripped)
	}

	// 5. And the pane gate's real input — `list-panes -a` — reports the
	//    reference in exactly the form the registry stores, so the comparison
	//    the gate makes is a string equality and not a normalisation.
	listed := liveRun(t, sock, "list-panes", "-a", "-F", "#{session_name}:#{window_id}.#{pane_id}")
	found := false
	for _, line := range strings.Split(listed, "\n") {
		if strings.TrimSpace(line) == ref {
			found = true
		}
	}
	if !found {
		t.Fatalf("list-panes -a does not report %q verbatim:\n%s", ref, listed)
	}
}

// TestLiveGuardRefusesAnUnsafeSocket proves the guard can fail, which is the
// only thing that makes its passing mean anything. It starts no server, so it
// runs under the build tag without the env gate.
func TestLiveGuardRefusesAnUnsafeSocket(t *testing.T) {
	for _, unsafe := range []string{
		"",                      // -L "" IS the default socket
		"   ",                   // and so is whitespace
		"default",               // tmux's own name for it
		"tmux",                  // the socket directory's own name
		"/tmp/tmux-501/default", // a -S path passed to -L by mistake
		"tt-obs test",           // a name that would word-split
	} {
		if err := checkPrivateSocket(unsafe); err == nil {
			t.Errorf("the guard accepted %q", unsafe)
		}
	}
	if err := checkPrivateSocket("tt-obs-test-12345-67890"); err != nil {
		t.Errorf("the guard rejected a legitimate private socket: %v", err)
	}
}

// ---------------------------------------------------------------------------
// case 22: the block ships @tt_obs_scope = all
// ---------------------------------------------------------------------------

// Every other case pins the scope to "off" first (see gateEnv.source), so this
// one sources the block AS SHIPPED and reads the option back: the user asked
// for the panel in every window, and that is what the file has to set.
func TestLiveCase22_TheShippedBlockTurnsThePanelOnEverywhere(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's design shipped a different default")
	}
	e := newGateEnv(t, sock, d)
	e.sourceAsShipped(t)

	if got := scopeNow(t, sock); got != "all" {
		t.Fatalf("the shipped block sets @tt_obs_scope %q, want all", got)
	}
	// And it takes effect without anybody setting anything: attaching shows a
	// window, and showing a window is what creates its sidebar. The extra
	// window is there only to get past @tt_obs_hold, which holds the hooks
	// back while the server has a single pane (resurrect's from-scratch
	// marker, case 17).
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "filler", "/bin/sh")
	attachClient(t, sock, "gate")
	settled(t, sock, windowID(t, sock, "gate:"), 1)
}

// ---------------------------------------------------------------------------
// case 23: a sidebar never holds a window open
// ---------------------------------------------------------------------------

// The other half of the reported bug: "when i exit last plane in the windw -
// te plane block window to close as well". A window whose own panes have all
// ended must close, exactly as it would have with no sidebar in it.
//
// Every route is measured, because the two hooks that carry this resolve their
// target differently (see the block's comment): the pane ends by `exit` or by
// kill-pane, in the window being shown or in one that is not.
func TestLiveCase23_ASidebarNeverHoldsAWindowOpen(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's design has no last-pane guard")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")
	// An attached client is what makes a window "shown": tmux sizes windows to
	// it and fires the hooks for it. This case never types into it.
	attachClient(t, sock, "gate")

	// The window the client is looking at keeps two of the user's own panes for
	// the whole case: nothing here may touch them. It is a NEW window, which
	// both puts a second pane on the server (@tt_obs_hold holds the hooks back
	// while there is only one) and fires the hook that gives it a sidebar.
	liveRun(t, sock, "new-window", "-t", "gate:", "-n", "home", "/bin/sh")
	home := windowID(t, sock, "gate:home")
	liveRun(t, sock, "split-window", "-h", "-d", "-t", home, "/bin/sh")
	settled(t, sock, home, 1)
	homePanes := len(windowPanes(sock, home))
	if homePanes != 3 {
		t.Fatalf("the home window has %d panes, want 2 of the user's and a sidebar\n%s", homePanes, livePaneDump(sock))
	}

	// end is how the user's pane goes: typed `exit`, or killed.
	for _, tc := range []struct {
		name string
		show bool
		end  func(t *testing.T, pane string)
	}{
		{"exit in the window being shown", true, func(t *testing.T, pane string) {
			liveRun(t, sock, "send-keys", "-t", pane, "exit", "Enter")
		}},
		{"exit in a window nobody is showing", false, func(t *testing.T, pane string) {
			liveRun(t, sock, "send-keys", "-t", pane, "exit", "Enter")
		}},
		{"kill-pane in the window being shown", true, func(t *testing.T, pane string) {
			liveRun(t, sock, "kill-pane", "-t", pane)
		}},
		{"kill-pane in a window nobody is showing", false, func(t *testing.T, pane string) {
			liveRun(t, sock, "kill-pane", "-t", pane)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "work", "/bin/sh")
			w := windowID(t, sock, "gate:work")
			// Shown once, so the scope gives it a sidebar; then left showing or
			// switched away from, which is the axis under test.
			liveRun(t, sock, "select-window", "-t", w)
			settled(t, sock, w, 1)
			if !tc.show {
				liveRun(t, sock, "select-window", "-t", home)
			}
			var user string
			for _, pn := range windowPanes(sock, w) {
				if !pn.observer() {
					user = pn.id
				}
			}
			if user == "" {
				t.Fatalf("no pane of the user's own in %s\n%s", w, livePaneDump(sock))
			}

			tc.end(t, user)

			liveWaitFor(t, sock, "the window to close once the user's last pane ended", func() bool {
				for _, l := range allWindows(sock) {
					if strings.HasSuffix(l, ":"+w) {
						return false
					}
				}
				return true
			})
			// Not by killing the renderer's process and leaving a husk: the
			// pane is gone with the window.
			for _, pn := range windowPanes(sock, w) {
				t.Fatalf("pane %s outlived its window\n%s", pn.id, livePaneDump(sock))
			}
			if n := len(windowPanes(sock, home)); n != homePanes {
				t.Fatalf("the window being shown has %d panes, want %d — the guard reached the wrong window\n%s",
					n, homePanes, livePaneDump(sock))
			}
			liveRun(t, sock, "select-window", "-t", home)
		})
	}

	// A window with work still in it keeps its sidebar when ONE pane ends.
	t.Run("a window with work left keeps its sidebar", func(t *testing.T) {
		liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "two", "/bin/sh")
		w := windowID(t, sock, "gate:two")
		liveRun(t, sock, "split-window", "-h", "-d", "-t", w, "/bin/sh")
		liveRun(t, sock, "select-window", "-t", w)
		settled(t, sock, w, 1)
		var user string
		for _, pn := range windowPanes(sock, w) {
			if !pn.observer() {
				user = pn.id
			}
		}
		liveRun(t, sock, "send-keys", "-t", user, "exit", "Enter")
		liveWaitFor(t, sock, "the pane that exited to go", func() bool {
			return len(windowPanes(sock, w)) == 2
		})
		stays(t, sock, w, 1, 800*time.Millisecond)
		liveRun(t, sock, "select-window", "-t", home)
	})

	// A window whose only pane is a MANUAL observer is the user's own choice,
	// and the guard leaves it alone (`auto`, not `is`, on the pane it kills).
	t.Run("a manual observer alone in a window survives", func(t *testing.T) {
		liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "solo",
			"/bin/sh", "-c", "tt observe --pane; exit")
		w := windowID(t, sock, "gate:solo")
		settled(t, sock, w, 1)
		liveRun(t, sock, "select-window", "-t", w)
		liveRun(t, sock, "select-pane", "-t", w)
		stays(t, sock, w, 1, 1500*time.Millisecond)
		found := false
		for _, l := range allWindows(sock) {
			if strings.HasSuffix(l, ":"+w) {
				found = true
			}
		}
		if !found {
			t.Fatalf("the guard closed a window whose only pane the user made\n%s", livePaneDump(sock))
		}
		liveRun(t, sock, "kill-window", "-t", w)
		liveRun(t, sock, "select-window", "-t", home)
	})
}

// ---------------------------------------------------------------------------
// case 24: `q` in a sidebar closes the panel, not just that pane
// ---------------------------------------------------------------------------

// The renderer's own quit key means the same thing the toggle means, because
// the panel is one thing: pressing `q` in an automatic sidebar sets the scope
// off for the server, so the other windows retire theirs when next shown
// instead of sitting there with nothing having happened to them.
func TestLiveCase24_QuittingASidebarClosesThePanelEverywhere(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's renderer had no global close")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")
	// The second window BEFORE the client attaches: with one pane on the server
	// @tt_obs_hold holds the hooks back (case 17), so nothing would be created
	// to quit — and the attach is the event that gives the first window its
	// sidebar.
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "other", "/bin/sh")
	c := attachClient(t, sock, "gate")

	first := windowID(t, sock, "gate:")
	second := windowID(t, sock, "gate:other")
	settled(t, sock, first, 1)
	liveRun(t, sock, "select-window", "-t", second)
	settled(t, sock, second, 1)
	liveRun(t, sock, "select-window", "-t", first)
	settled(t, sock, first, 1)

	var sidebar string
	for _, pn := range windowPanes(sock, first) {
		if pn.observer() {
			sidebar = pn.id
		}
	}
	if sidebar == "" {
		t.Fatalf("no sidebar to quit\n%s", livePaneDump(sock))
	}
	// Typed INTO the sidebar's pane, which is what a user does.
	liveRun(t, sock, "send-keys", "-t", sidebar, "q")

	settled(t, sock, first, 0)
	liveWaitFor(t, sock, "the scope to go off", func() bool { return scopeNow(t, sock) == "off" })
	liveRun(t, sock, "select-window", "-t", second)
	settled(t, sock, second, 0)
	// And the toggle brings it back, so `q` is a close and not a corner the
	// user has to dig out of.
	c.toggle(t, d)
	settled(t, sock, second, 1)
}

// ---------------------------------------------------------------------------
// case 25: the start button starts the observer
// ---------------------------------------------------------------------------

// The sidebar used to name a command and leave it there: "observer not running
// - start `tt observe`". It now carries a button, and this case presses it the
// way a user does — a keystroke into that pane — and then requires a real
// server to exist and the panel to attach to it.
//
// The server it starts is DETACHED (setsid), which is the point: it must
// outlive the pane that started it and serve every other window. That also
// means killing this test's tmux server would not kill it, so the pid file is
// read and the process signalled in a cleanup.
func TestLiveCase25_TheStartButtonStartsTheObserver(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's panel had no start button")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")

	pidFile := filepath.Join(e.ttDir(), "observer.pid")
	t.Cleanup(func() {
		// Whatever the outcome: a server started here does not belong to the
		// machine, and nothing else will reap it.
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid := pidFromRecord(string(data)); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	})

	// Nothing is running: the sidebar must say so and draw the button.
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "filler", "/bin/sh")
	attachClient(t, sock, "gate")
	w := windowID(t, sock, "gate:")
	settled(t, sock, w, 1)
	var sidebar string
	for _, p := range windowPanes(sock, w) {
		if p.observer() {
			sidebar = p.id
		}
	}
	if sidebar == "" {
		t.Fatalf("no sidebar\n%s", livePaneDump(sock))
	}
	liveWaitFor(t, sock, "the sidebar to offer a start button", func() bool {
		painted := liveQuiet(sock, "capture-pane", "-p", "-t", sidebar)
		return strings.Contains(painted, "observer not running") && strings.Contains(painted, "start the observer")
	})
	// And it no longer tells the user to go and type the command itself.
	if painted := liveQuiet(sock, "capture-pane", "-p", "-t", sidebar); strings.Contains(painted, "`tt observe`") {
		t.Fatalf("the sidebar still names a command to type:\n%s", painted)
	}

	// Press it.
	liveRun(t, sock, "send-keys", "-t", sidebar, "s")

	liveWaitForIn(t, sock, "a server to bind the socket", 20*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(e.ttDir(), "observer.sock"))
		return err == nil
	})
	liveWaitForIn(t, sock, "the panel to attach to it", 20*time.Second, func() bool {
		return liveAttached(liveQuiet(sock, "capture-pane", "-p", "-t", sidebar))
	})
	// The button is gone with the state that justified it.
	painted := liveQuiet(sock, "capture-pane", "-p", "-t", sidebar)
	if strings.Contains(painted, "start the observer") {
		t.Fatalf("the button is still there with a server attached:\n%s", painted)
	}

	// It survives the pane that started it: that is why it is setsid.
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading the pid file: %v", err)
	}
	pid := pidFromRecord(string(data))
	if pid <= 0 {
		t.Fatalf("the pid file names no process: %s", data)
	}
	liveRun(t, sock, "kill-pane", "-t", sidebar)
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the server died with the pane that started it: %v", err)
	}

	// F1 (D8): ...but not its tmux SERVER. A per-server observer exits when
	// the server it was started for is gone; before the fix it ran on,
	// parentless, its socket and server long gone (pids 15233, 28121).
	for _, id := range strings.Fields(liveQuiet(sock, "list-sessions", "-F", "#{session_id}")) {
		_ = liveTmux(sock, "kill-session", "-t", id).Run()
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("observer pid %d still runs 10 s after its tmux server ended", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pidFromRecord reads the pid out of the server's pid-file record, which is
// JSON. The gate parses it by hand rather than importing the wire package: this
// file tests what the outside can see, and the outside sees a file.
func pidFromRecord(record string) int {
	i := strings.Index(record, `"pid":`)
	if i < 0 {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(record[i+6:], "%d", &pid); err != nil {
		return 0
	}
	return pid
}

// ---------------------------------------------------------------------------
// case 26: prefix g is the same toggle as prefix C-o
// ---------------------------------------------------------------------------

// Two keys, one command alias. bind-key expands an alias at bind time, so the
// only way they can differ is if one of them is not bound at all — which is
// what this presses for.
func TestLiveCase26_TheShortKeyIsTheSameToggle(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's design had one binding")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	c := attachClient(t, sock, "gate")
	w := windowID(t, sock, "gate:")

	c.press(t, d, 'g') // on
	settled(t, sock, w, 1)
	if got := scopeNow(t, sock); got == "off" {
		t.Fatal("prefix g did not turn the panel on for the server")
	}
	c.press(t, d, 'g') // off
	settled(t, sock, w, 0)
	if got := scopeNow(t, sock); got != "off" {
		t.Fatalf("prefix g left @tt_obs_scope %q, want off", got)
	}

	// And the two keys are interchangeable, in either order.
	c.press(t, d, 'g')
	settled(t, sock, w, 1)
	c.toggle(t, d)
	settled(t, sock, w, 0)
}

// ---------------------------------------------------------------------------
// case 27: the sidebar keeps its own width
// ---------------------------------------------------------------------------

// Reported: "when i change the size from small to big and gabe two vetically
// split lane, plust our plane with session - then this lane changes the width
// as wll and they all thre have equal widht". Both halves are tmux behaving as
// designed — a window that grows scales every pane proportionally, and
// even-horizontal makes them equal — so the renderer puts itself back.
func TestLiveCase27_TheSidebarKeepsItsWidthThroughResizesAndLayouts(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10's renderer did not keep its width")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "filler", "/bin/sh")
	attachClient(t, sock, "gate")

	// A window with two panes of the user's own, plus the sidebar: the shape
	// the report describes.
	liveRun(t, sock, "new-window", "-t", "gate:", "-n", "work", "/bin/sh")
	w := windowID(t, sock, "gate:work")
	liveRun(t, sock, "split-window", "-h", "-d", "-t", w, "/bin/sh")
	settled(t, sock, w, 1)

	sidebarWidth := func() int {
		for _, p := range windowPanes(sock, w) {
			if p.observer() {
				return p.width
			}
		}
		return -1
	}
	const want = 46
	waitForWidth := func(what string) {
		t.Helper()
		liveWaitFor(t, sock, what, func() bool { return sidebarWidth() == want })
	}
	waitForWidth("the sidebar to start at its configured width")

	// 1. the window grows: tmux scales every pane, including this one.
	liveRun(t, sock, "resize-window", "-t", w, "-x", "260", "-y", "50")
	waitForWidth("the sidebar to return to 46 after the window grew")

	// 2. and shrinks again.
	liveRun(t, sock, "resize-window", "-t", w, "-x", "160", "-y", "50")
	waitForWidth("the sidebar to return to 46 after the window shrank")

	// 3. EVERY preset layout the default keys apply. Each one lays out the
	// sidebar with everything else — tmux has no pinned pane — and each time
	// it must come back to a full-height column at the right edge, with the
	// user's two panes still there and still theirs.
	docked := func() bool {
		for _, p := range windowPanes(sock, w) {
			if !p.observer() {
				continue
			}
			at := liveQuiet(sock, "display-message", "-p", "-t", p.id,
				"#{pane_at_right}#{pane_at_top}#{pane_at_bottom}")
			return at == "111" && p.width == want
		}
		return false
	}
	// And the user's OWN panes must end up in the layout they asked for. This
	// is the assertion the first version of this case did not make, and the
	// screenshot that followed showed why: joining the sidebar back gives its
	// columns to whichever pane it lands beside, so `even-horizontal` came out
	// 57 / 46 / 115. The renderer breaks out, re-applies the layout to the
	// panes left behind, and joins back.
	userWidths := func() []int {
		var out []int
		for _, p := range windowPanes(sock, w) {
			if !p.observer() {
				out = append(out, p.width)
			}
		}
		return out
	}
	for _, layout := range []string{
		"even-horizontal", "even-vertical", "main-horizontal", "main-vertical",
		"tiled", "main-horizontal-mirrored", "main-vertical-mirrored",
	} {
		liveRun(t, sock, "select-layout", "-t", w, layout)
		// Docked AND all three panes home: the re-dock parks the sidebar in a
		// window of its own for a moment, and a count taken then is a count of
		// a window mid-repair rather than a result.
		liveWaitFor(t, sock, "the sidebar to re-dock after "+layout, func() bool {
			return docked() && len(windowPanes(sock, w)) == 3
		})
		// And it settles there: one layout must not produce a second re-dock.
		stays(t, sock, w, 1, 400*time.Millisecond)
		if n := len(windowPanes(sock, w)); n != 3 {
			t.Fatalf("%s settled with %d panes, want the user's two and the sidebar\n%s",
				layout, n, livePaneDump(sock))
		}
		// even-horizontal is the one with an exact answer: two panes side by
		// side in the columns the sidebar left, within a column of each other.
		if layout == "even-horizontal" {
			liveWaitFor(t, sock, "the user's panes to come out even", func() bool {
				got := userWidths()
				return len(got) == 2 && got[0] >= got[1]-1 && got[0] <= got[1]+1
			})
		}
	}
	// And cycling, which is prefix Space — `select-layout -n` rather than
	// `next-layout`, because that is what the binding runs and the only one of
	// the two that fires after-select-layout.
	liveRun(t, sock, "select-layout", "-n", "-t", w)
	liveWaitFor(t, sock, "the sidebar to re-dock after cycling the layout", docked)

	// The user's own panes are still there, and still theirs: the columns come
	// back from the neighbour, not from the far pane, and nothing is closed.
	panes := windowPanes(sock, w)
	if len(panes) != 3 {
		t.Fatalf("the window has %d panes, want 3\n%s", len(panes), livePaneDump(sock))
	}
	stays(t, sock, w, 1, 700*time.Millisecond)
	if got := sidebarWidth(); got != want {
		t.Fatalf("the sidebar settled at %d columns, want %d", got, want)
	}

	// 4. a window too narrow to spare the columns is NOT fought over: the
	// width rule says it should not have a sidebar at all, and the reconcile
	// retires it when the window is next shown.
	liveRun(t, sock, "resize-window", "-t", w, "-x", "100", "-y", "50")
	liveRun(t, sock, "select-window", "-t", w)
	liveWaitFor(t, sock, "the narrow window to lose its sidebar", func() bool {
		all, _ := observers(sock, w)
		return all == 0
	})
}

// ---------------------------------------------------------------------------
// case 28: the counter paints as BADGES, not as a sentence
// ---------------------------------------------------------------------------

// "instead of verbose 4 waiting 1 error … implement badges with colour
// background". A badge is only a badge if a terminal actually paints a
// background, so this asserts the escape sequence a client emits — capture-pane
// -e keeps the styling that capture-pane -p throws away.
func TestLiveCase28_TheCounterPaintsAsColouredBadges(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10 had no status line")
	}
	e := newGateEnv(t, sock, d)
	liveRun(t, sock, "set-option", "-g", "status", "on")
	liveRun(t, sock, "set-option", "-g", "status-interval", "0")
	liveRun(t, sock, "set-option", "-g", "status-right", statusRightFromConf(t))
	// The mode marker the user's theme script publishes. tt owns the badge
	// colours; this is what tells it which set to paint, live.
	liveRun(t, sock, "set-option", "-g", "@theme_mode", "light")

	liveRun(t, sock, "new-window", "-d", "-n", "nested",
		"sh -c 'TMUX= exec tmux -f /dev/null -L "+sock+" attach -t gate'")
	liveWaitFor(t, sock, "the nested client to attach", func() bool {
		return liveQuiet(sock, "list-clients", "-F", "#{client_name}") != ""
	})

	e.source(t)
	pid, ref := e.fakeSession(t, "claude1")
	e.writeRegistryRecord(t, pid, ref, "first", "waiting")
	e.window(t, "server", "observe")

	liveWaitForIn(t, sock, "the badge to reach the option", 15*time.Second, func() bool {
		return strings.Contains(liveQuiet(sock, "show-options", "-gqv", "@tt_status"), "bg=")
	})
	option := liveQuiet(sock, "show-options", "-gqv", "@tt_status")
	// A glyph and a number on a colour, and the colour is chosen by the theme
	// markers when the bar is painted rather than baked in here. The
	// @theme_mode branch is the fallback of the combined conditional
	// (statusLineByMode), byte for byte what tt emitted before @theme_dark.
	for _, want := range []string{"\U000F06A9 #[", "#{?#{==:#{@theme_mode},light},#d79921,#fab387}", "bold] \uf0f3 1 #[default]"} {
		if !strings.Contains(option, want) {
			t.Fatalf("@tt_status is %q; it is missing %q", option, want)
		}
	}
	if strings.Contains(option, "waiting") {
		t.Fatalf("@tt_status still spells out the state: %q", option)
	}

	// What the client PAINTS: the LIGHT fill (#d79921 -> 215;153;33), because
	// @theme_mode says light, with the dark ink measured against it.
	liveWaitFor(t, sock, "the badge to be painted with its light-mode background", func() bool {
		painted := liveQuiet(sock, "capture-pane", "-e", "-p", "-t", "gate:nested")
		return strings.Contains(painted, "48;2;215;153;33")
	})
	painted := liveQuiet(sock, "capture-pane", "-e", "-p", "-t", "gate:nested")
	if !strings.Contains(painted, "38;2;17;17;27") {
		t.Fatalf("the badge is not inked with the dark ink:\n%s", painted)
	}
	// Flipping the marker repaints it in the dark fill (#fab387), with no
	// republish and no state change — which is the point of the conditional.
	liveRun(t, sock, "set-option", "-g", "@theme_mode", "dark")
	liveWaitFor(t, sock, "the badge to follow the theme switch", func() bool {
		return strings.Contains(liveQuiet(sock, "capture-pane", "-e", "-p", "-t", "gate:nested"), "48;2;250;179;135")
	})
	// The robot leads the row, and the glyph and the number are there to read.
	if painted := liveQuiet(sock, "capture-pane", "-p", "-t", "gate:nested"); !strings.Contains(painted, "\U000F06A9") ||
		!strings.Contains(painted, "\uf0f3 1") {
		t.Fatalf("the painted row is missing the lead glyph or the badge:\n%s", painted)
	}

	// The per-client predicate theme.sh publishes (architecture §5.1) takes
	// over from @theme_mode the moment it is set, with no republish: the
	// conditional in @tt_status already carries both layers. @theme_mode is
	// put back to LIGHT first and left there, so every fill below is decided by
	// @theme_dark and none by the old marker. The predicate is evaluated for
	// the client that paints the bar (the nested one), so its own entry in
	// @theme_screens counts, and without one @theme_default decides.
	paints := func(what, rgb string) {
		t.Helper()
		liveWaitFor(t, sock, what, func() bool {
			return strings.Contains(liveQuiet(sock, "capture-pane", "-e", "-p", "-t", "gate:nested"), rgb)
		})
	}
	liveRun(t, sock, "set-option", "-g", "@theme_mode", "light")
	paints("the badge to go back to the light fill", "48;2;215;153;33")
	liveRun(t, sock, "set-option", "-g", "@theme_default", "dark")
	liveRun(t, sock, "set-option", "-g", "@theme_dark", perClientThemePredicate)
	paints("the predicate's dark default to win over @theme_mode light", "48;2;250;179;135")
	liveRun(t, sock, "set-option", "-g", "@theme_default", "light")
	paints("the predicate to follow @theme_default to light", "48;2;215;153;33")
	tty := liveRun(t, sock, "list-clients", "-F", "#{client_tty}")
	if tty == "" || strings.ContainsAny(tty, " \n") {
		t.Fatalf("want exactly the nested client, list-clients gave %q", tty)
	}
	liveRun(t, sock, "set-option", "-g", "@theme_screens", " "+tty+"=dark ")
	paints("the drawing screen's own entry (dark) to win over the light default", "48;2;250;179;135")
}

// ---------------------------------------------------------------------------
// case 29: one grouping, for every sidebar
// ---------------------------------------------------------------------------

// `g` typed into one sidebar regroups that sidebar at once, saves the choice
// in tt's state directory, and every other sidebar follows it — the user asked
// for one grouping, not one per window. A sidebar with nothing to show ticks
// once a minute, so the follower is given the whole minute.
func TestLiveCase29_OneGroupingForEverySidebar(t *testing.T) {
	sock := liveServer(t)
	d := designFor(t)
	if d.name != "current" {
		t.Skip("P10 had one grouping")
	}
	e := newGateEnv(t, sock, d)
	e.source(t)
	e.scope(t, "all")
	liveRun(t, sock, "new-window", "-d", "-t", "gate:", "-n", "other", "/bin/sh")
	_ = attachClient(t, sock, "gate")

	first := windowID(t, sock, "gate:")
	second := windowID(t, sock, "gate:other")
	settled(t, sock, first, 1)
	liveRun(t, sock, "select-window", "-t", second)
	settled(t, sock, second, 1)
	liveRun(t, sock, "select-window", "-t", first)
	settled(t, sock, first, 1)

	sidebarOf := func(w string) string {
		for _, pn := range windowPanes(sock, w) {
			if pn.observer() {
				return pn.id
			}
		}
		t.Fatalf("no sidebar in %s\n%s", w, livePaneDump(sock))
		return ""
	}
	a, b := sidebarOf(first), sidebarOf(second)
	// The header is a row of grouping tabs over a baseline whose heavy run
	// (━) sits under the active tab, pads included; the tab read off the
	// columns of that run is the grouping the sidebar draws.
	title := func(id string) string { return liveActiveTab(liveQuiet(sock, "capture-pane", "-p", "-t", id)) }
	liveWaitFor(t, sock, "both sidebars to draw the default grouping", func() bool {
		return title(a) == "Project" && title(b) == "Project"
	})

	liveRun(t, sock, "send-keys", "-t", a, "g")
	liveWaitFor(t, sock, "the sidebar g was typed into to regroup", func() bool {
		return title(a) == "Status"
	})
	saved := filepath.Join(e.ttDir(), "panel-mode")
	if data, err := os.ReadFile(saved); err != nil || string(data) != "status\n" {
		t.Fatalf("%s = %q (%v), want \"status\\n\"", saved, data, err)
	}
	liveWaitForIn(t, sock, "the other sidebar to follow", 75*time.Second, func() bool {
		return title(b) == "Status"
	})
}

// liveActiveTab is the label of the active grouping tab in a painted
// sidebar: the first line's text over the columns of the second line's heavy
// (━) run, trimmed. "" when the header carries no baseline.
//
// The painter never writes the blank cells around the centred tabs, so the
// capture loses them; the tabs' column is put back the way tabsLineIn centres
// them: left = (width - (text + its two outer pads)) / 2, the text one in.
func liveActiveTab(painted string) string {
	lines := strings.SplitN(painted, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	text, base := []rune(strings.TrimSpace(lines[0])), []rune(strings.TrimSpace(lines[1]))
	left := (len(base)-(len(text)+2))/2 + 1
	tabs := append([]rune(strings.Repeat(" ", max(left, 0))), text...)
	start, end := -1, -1
	for i, r := range base {
		if r == '━' {
			if start < 0 {
				start = i
			}
			end = i + 1
		}
	}
	if start < 0 || start >= len(tabs) {
		return ""
	}
	return strings.TrimSpace(string(tabs[start:min(end, len(tabs))]))
}

// liveAttached reports whether a painted sidebar is attached to an observer.
// FR9 dropped the "via tt observe (pid N)" note: an attached sidebar at rest
// says exactly "connected" between the ══ of its footer, a focused one shows
// the keys there. Only the footer (the last non-blank line) is read, so a
// session named "remove" cannot answer; a focused one also must carry neither
// the offline note nor the start button (matched by its first word, which a
// narrow pane does not cut).
func liveAttached(painted string) bool {
	lines := strings.Split(strings.TrimRight(painted, " \n"), "\n")
	foot := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(foot, "═") {
		return false
	}
	if strings.Trim(foot, "═ ") == panelConnected {
		return true
	}
	return strings.Contains(foot, "move") &&
		!strings.Contains(painted, noteObserverNotRunning) && !strings.Contains(painted, "start the")
}
