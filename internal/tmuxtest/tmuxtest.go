// Package tmuxtest starts ISOLATED tmux servers for tests (architecture §12.2).
//
// It is imported only from _test.go files (audit.Imports) and it is the
// reason no test in this module needs its own tmux exec. The safety contract,
// every clause of which has a history on this machine:
//
//   - Exactly two tmux exec forms exist here, both audited literally
//     (audit.TmuxtestExecForms): Start's `tmux -f /dev/null -L moox-test-…`
//     and StartCold's `<tmux> -f <harness> -f <share>/moox.conf -S <temp root>/…`
//     with an explicit, non-inherited environment. Everything else goes
//     through tmuxtarget, which always passes -u -S <this server's socket>.
//   - A socket name always starts moox-test- or moox-val- (Start), or the
//     socket lives under the test's own temp root (StartCold). "default", the
//     real tmux directory (StartCold) and $TMUX_TMPDIR are refused.
//   - Panes run /bin/sh: SHELL, default-shell and default-command are pinned.
//     A zsh pane of a scratch server running the user's rc deleted real
//     launch recordings keyed by pane number on 2026-09-19. The only other
//     shells are the HookZsh/HookBash fixtures, whose HOME and ZDOTDIR are
//     under t.TempDir() and whose rc sources only the vendored hook.
//   - HOME and XDG_* point at scratch dirs; TMUX, TMUX_TMPDIR, MOOX_*, ENV
//     and BASH_ENV are never passed on.
//   - No run-shell is ever issued, and kill-server is never used (the
//     terminal hook blocks it): Close kills this server's sessions and lets
//     exit-empty end it.
//   - Every file this package writes goes through statedir (the module's one
//     writer), including the temp root, so the write audit holds here too.
package tmuxtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Server is one isolated tmux server.
type Server struct {
	T tmuxtarget.Target
	// Home, XS, XD, XC are the scratch HOME and XDG_STATE_HOME,
	// XDG_DATA_HOME, XDG_CONFIG_HOME the server was started with.
	Home, XS, XD, XC string
	// Name is the socket's basename (what @moox-sockets matches).
	Name string
	// Session is the name of the session the server was started with.
	Session string
	// Root is the scratch directory everything of this server lives under.
	Root string
	// Bin is MOOX_BIN: built binaries (WithBinaries) or an empty scratch dir.
	Bin string
	// Share is the share root the server's config comes from (StartCold,
	// Prepare).
	Share string

	tb        testing.TB
	closeOnce sync.Once
}

// Shell is a pane shell for default-command. Only this package constructs
// one with a command (HookZsh, HookBash); the zero Shell means /bin/sh.
type Shell struct {
	cmd  string
	kind string
}

// Kind is "sh", "zsh" or "bash".
func (s Shell) Kind() string {
	if s.kind == "" {
		return "sh"
	}
	return s.kind
}

// Command is the default-command string.
func (s Shell) Command() string {
	if s.cmd == "" {
		return "/bin/sh"
	}
	return s.cmd
}

type config struct {
	name      string
	session   string
	shell     Shell
	env       []string
	harness   []string
	binaries  bool
	width     int
	height    int
	configErr error
	snapshot  string
	prepare   []func(*Server)
	localConf bool
}

// Option configures Start or StartCold.
type Option func(*config)

// WithName sets the socket name. It must start moox-test- or moox-val-.
func WithName(name string) Option { return func(c *config) { c.name = name } }

// WithSession sets the starting session's name (default "boot").
func WithSession(name string) Option { return func(c *config) { c.session = name } }

// WithShell sets the panes' default-command to a hook-shell fixture.
func WithShell(s Shell) Option { return func(c *config) { c.shell = s } }

// WithEnv adds NAME=VALUE pairs to the environment that starts the server
// (StartCold: MOOX_STUB_COMMANDS, MOOX_NO_AUTO_RESTORE). TMUX, TMUX_TMPDIR,
// HOME, XDG_*, SHELL and PATH cannot be overridden this way.
func WithEnv(kv ...string) Option {
	return func(c *config) {
		for _, e := range kv {
			name, _, ok := strings.Cut(e, "=")
			if !ok || protectedEnv(name) {
				c.configErr = fmt.Errorf("tmuxtest: WithEnv(%q) is not allowed", e)
				return
			}
			c.env = append(c.env, e)
		}
	}
}

// WithHarnessLines appends lines to StartCold's harness.conf, which tmux
// loads BEFORE moox.conf. Probes use it for a config-time job or hook. A line
// holding a run-shell must end `>/dev/null 2>&1 || true` plus its closing
// quote (RR12): the same rule the audit holds share/moox to, checked here at
// run time because a test builds these lines from variables.
func WithHarnessLines(lines ...string) Option {
	return func(c *config) {
		for _, l := range lines {
			if strings.Contains(l, "run-shell") || strings.HasPrefix(strings.TrimSpace(l), "run ") {
				t := strings.TrimRight(strings.TrimSpace(l), `'"`)
				if !strings.HasSuffix(t, ">/dev/null 2>&1 || true") {
					c.configErr = fmt.Errorf("tmuxtest: harness run-shell is not silent (RR12): %s", l)
					return
				}
			}
		}
		c.harness = append(c.harness, lines...)
	}
}

// WithBinaries builds this tree's commands (cmd/tt, and cmd/moox once it
// exists) into a scratch dir and passes it as MOOX_BIN. Without it MOOX_BIN
// is an empty scratch dir, so nothing installed on the machine is reachable.
func WithBinaries() Option { return func(c *config) { c.binaries = true } }

// WithSnapshot pre-seeds StartCold's snapshot directory (the one the new
// socket's key resolves to, §4.2) with every moox-* file and moox-last of
// dir, before the server starts, so the boot gate finds them (§12.2).
func WithSnapshot(dir string) Option { return func(c *config) { c.snapshot = dir } }

// WithPrepare runs fn on the laid-out Server before StartCold starts tmux:
// seeding a freeze store, a registry or recordings under the scratch roots,
// which the boot restore reads at once.
func WithPrepare(fn func(*Server)) Option {
	return func(c *config) { c.prepare = append(c.prepare, fn) }
}

// Paths is the server's per-socket layout under its scratch roots.
func (s *Server) Paths(tb testing.TB) sockets.Paths {
	tb.Helper()
	p, err := sockets.For(s.T.Socket, tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS,
		"XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC})
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

// seedSnapshots copies the moox snapshot files of src into the server's
// snapshot dir, through statedir.
func seedSnapshots(tb testing.TB, s *Server, src string) {
	tb.Helper()
	claude := filepath.Join(s.Home, ".claude")
	from, err := statedir.OpenExisting(src, claude)
	if err != nil {
		tb.Fatalf("tmuxtest: snapshot source: %v", err)
	}
	defer func() { _ = from.Close() }()
	to, err := statedir.Open(s.Paths(tb).Snapshots, claude)
	if err != nil {
		tb.Fatalf("tmuxtest: snapshot dir: %v", err)
	}
	defer func() { _ = to.Close() }()
	names, err := from.ReadDirNames()
	if err != nil {
		tb.Fatal(err)
	}
	n := 0
	for _, name := range names {
		if !strings.HasPrefix(name, "moox-") {
			continue
		}
		b, _, err := from.ReadFileLimit(name, 512<<20)
		if err != nil {
			tb.Fatalf("tmuxtest: reading %s: %v", name, err)
		}
		if err := to.WriteFile(name, b, 0o600); err != nil {
			tb.Fatalf("tmuxtest: seeding %s: %v", name, err)
		}
		n++
	}
	if n == 0 {
		tb.Fatalf("tmuxtest: WithSnapshot(%s): no moox-* file", src)
	}
}

// WithSize sets the starting session's -x/-y (default 200x50).
func WithSize(x, y int) Option { return func(c *config) { c.width, c.height = x, y } }

func protectedEnv(name string) bool {
	switch name {
	case "TMUX", "TMUX_TMPDIR", "TMUX_PANE", "HOME", "PATH", "SHELL", "ZDOTDIR", "ENV", "BASH_ENV",
		"XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME":
		return true
	}
	return false
}

func newConfig(opts []Option) config {
	c := config{session: "boot", width: 200, height: 50}
	for _, o := range opts {
		o(&c)
	}
	return c
}

var seq atomic.Int64

// CheckName is the Start socket-name rule.
func CheckName(name string) error {
	if name == "" || name == "default" || strings.ContainsAny(name, "/ \t") {
		return fmt.Errorf("tmuxtest: refusing socket name %q", name)
	}
	if !strings.HasPrefix(name, "moox-test-") && !strings.HasPrefix(name, "moox-val-") {
		return fmt.Errorf("tmuxtest: socket name %q must start moox-test- or moox-val-", name)
	}
	return nil
}

// CheckColdSocket is the StartCold socket rule: under the test's own temp
// root, never named default, never in tmux's own directory or $TMUX_TMPDIR,
// and short enough for sockaddr_un (104 bytes on macOS).
func CheckColdSocket(sock, root string) error {
	if filepath.Base(sock) == "default" {
		return fmt.Errorf("tmuxtest: refusing socket %q named default", sock)
	}
	if len(sock) >= 104 {
		return fmt.Errorf("tmuxtest: socket path %q is %d bytes, over sockaddr_un's limit", sock, len(sock))
	}
	if !within(sock, root) || filepath.Clean(sock) == filepath.Clean(root) {
		return fmt.Errorf("tmuxtest: socket %q is not under the temp root %q", sock, root)
	}
	forbidden := []string{}
	if p, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, "x"); err == nil {
		forbidden = append(forbidden, filepath.Dir(p))
	}
	forbidden = append(forbidden, filepath.Join("/tmp", "tmux-"+strconv.Itoa(os.Getuid())))
	if d := os.Getenv("TMUX_TMPDIR"); d != "" {
		forbidden = append(forbidden, d)
		if r, err := filepath.EvalSymlinks(d); err == nil {
			forbidden = append(forbidden, r)
		}
	}
	real := sock
	if r, err := filepath.EvalSymlinks(filepath.Dir(sock)); err == nil {
		real = filepath.Join(r, filepath.Base(sock))
	}
	for _, f := range forbidden {
		if within(sock, f) || within(real, f) {
			return fmt.Errorf("tmuxtest: socket %q is inside %q", sock, f)
		}
	}
	return nil
}

func within(p, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// scrubbedEnv is the parent environment minus everything that could steer a
// server or a shell at real state, plus the scratch roots.
func scrubbedEnv(s *Server, extra []string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if protectedEnv(name) || strings.HasPrefix(name, "MOOX_") || strings.HasPrefix(name, "TT_TMUX_") ||
			strings.HasPrefix(name, "XDG_") || name == "CLAUDE_CONFIG_DIR" {
			continue
		}
		out = append(out, kv)
	}
	return append(append(out, scratchEnv(s)...), extra...)
}

func scratchEnv(s *Server) []string {
	return []string{
		"HOME=" + s.Home,
		"XDG_STATE_HOME=" + s.XS,
		"XDG_DATA_HOME=" + s.XD,
		"XDG_CONFIG_HOME=" + s.XC,
		"SHELL=/bin/sh",
		"MOOX_BIN=" + s.Bin,
	}
}

// layout creates root/{home,xs,xd,xc,bin} through statedir and fills the
// Server's paths.
func layout(tb testing.TB, s *Server, root string) {
	tb.Helper()
	s.Root = root
	s.Home = filepath.Join(root, "home")
	s.XS = filepath.Join(root, "xs")
	s.XD = filepath.Join(root, "xd")
	s.XC = filepath.Join(root, "xc")
	s.Bin = filepath.Join(root, "bin")
	claude := filepath.Join(s.Home, ".claude")
	for _, p := range []string{s.Home, s.XS, s.XD, s.XC, s.Bin} {
		d, err := statedir.Open(p, claude)
		if err != nil {
			tb.Fatalf("tmuxtest: creating %s: %v", p, err)
		}
		_ = d.Close()
	}
}

// Start launches
//
//	tmux -f /dev/null -L moox-test-<pid>-<n> new-session -d -s boot -x 200 -y 50 /bin/sh
//
// with HOME and XDG_* under t.TempDir(), SHELL=/bin/sh, MOOX_BIN scratch (or
// built, WithBinaries), and TMUX, TMUX_TMPDIR and MOOX_* removed. The server
// is closed by t.Cleanup.
func Start(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	c := newConfig(opts)
	if c.configErr != nil {
		tb.Fatal(c.configErr)
	}
	name := c.name
	if name == "" {
		name = fmt.Sprintf("moox-test-%d-%d", os.Getpid(), seq.Add(1))
	}
	if err := CheckName(name); err != nil {
		tb.Fatal(err)
	}
	sock, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, name)
	if err != nil {
		tb.Fatal(err)
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		tb.Skip("tmuxtest: tmux not installed")
	}
	s := &Server{T: tmuxtarget.Target{Bin: bin, Socket: sock}, Name: name, Session: c.session, tb: tb}
	layout(tb, s, tb.TempDir())
	if c.binaries {
		buildBinaries(tb, s.Bin)
	}

	cmd := exec.Command("tmux", "-f", "/dev/null", "-L", name, "new-session", "-d", "-s", c.session,
		"-x", strconv.Itoa(c.width), "-y", strconv.Itoa(c.height), "/bin/sh")
	cmd.Env = scrubbedEnv(s, c.env)
	cmd.Dir = s.Home
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("tmuxtest: starting %s: %v: %s", name, err, out)
	}
	tb.Cleanup(s.Close)
	s.Run(tb, tmuxtarget.Chain(
		[]string{"set", "-g", "default-shell", "/bin/sh"},
		[]string{"set", "-g", "default-command", c.shell.Command()},
	)...)
	return s
}

// StartCold starts a server COLD on the REAL config (§12.2, R2-M4):
//
//	<tmux> -f <root>/harness.conf -f <repo>/share/moox/moox.conf -S <root>/<name> new-session -d -s boot -x 200 -y 50
//
// with an explicit environment (nothing inherited): PATH, LANG, HOME and
// XDG_* under the temp root, SHELL=/bin/sh, MOOX_BIN, plus WithEnv pairs.
// harness.conf runs first: default-command, @moox-sockets "<name>",
// exit-empty on, then any WithHarnessLines. The temp root comes from
// os.TempDir() + "mx<hex>" (not t.TempDir(), which embeds the test name and
// can push a macOS socket path past 104 bytes); it is removed at cleanup.
func StartCold(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	s, c, tmuxBin, conf, harness := prepare(tb, opts)
	cmd := exec.Command(tmuxBin, "-f", harness, "-f", conf, "-S", s.T.Socket,
		"new-session", "-d", "-s", c.session, "-x", strconv.Itoa(c.width), "-y", strconv.Itoa(c.height))
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=" + langOrDefault(),
	}, append(scratchEnv(s), c.env...)...)
	cmd.Dir = s.Home
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("tmuxtest: cold start of %s: %v: %s", s.T.Socket, err, out)
	}
	return s
}

// Prepare lays out everything StartCold would — the scratch roots, built
// binaries, seeded snapshots, WithPrepare hooks, harness.conf — and the same
// lines as XC/moox/local.conf, but starts NO server: the test starts it
// through a moox binary (`moox`, `moox restore`), which runs `-f
// <share>/moox.conf` only, so the harness settings reach it through
// local.conf. The socket is s.T.Socket (under the temp root, as StartCold's);
// Close ends whatever server the test started there.
func Prepare(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	s, _, _, _, _ := prepare(tb, append(opts, WithLocalConf()))
	return s
}

// WithLocalConf also writes the harness lines to XC/moox/local.conf, so a
// server a moox binary starts later on this socket (moox restart, moox
// restore with no server) runs with the same settings.
func WithLocalConf() Option { return func(c *config) { c.localConf = true } }

func prepare(tb testing.TB, opts []Option) (*Server, config, string, string, string) {
	tb.Helper()
	c := newConfig(opts)
	if c.configErr != nil {
		tb.Fatal(c.configErr)
	}
	share, err := ShareRoot()
	if err != nil {
		tb.Fatal(err)
	}
	conf := filepath.Join(share, "moox.conf")
	if _, err := os.Stat(conf); err != nil {
		tb.Fatalf("tmuxtest: the real config is missing: %v", err)
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		tb.Skip("tmuxtest: tmux not installed")
	}
	name := c.name
	if name == "" {
		name = fmt.Sprintf("moox-test-cold-%d", seq.Add(1))
	}
	if err := CheckName(name); err != nil {
		tb.Fatal(err)
	}

	root := tempRoot(tb)
	sock := filepath.Join(root, name)
	if err := CheckColdSocket(sock, root); err != nil {
		tb.Fatal(err)
	}
	s := &Server{T: tmuxtarget.Target{Bin: tmuxBin, Socket: sock}, Name: name, Session: c.session, tb: tb, Share: share}
	layout(tb, s, root)
	if c.binaries {
		buildBinaries(tb, s.Bin)
	}

	if c.snapshot != "" {
		seedSnapshots(tb, s, c.snapshot)
	}
	for _, fn := range c.prepare {
		fn(s)
	}

	harness := filepath.Join(root, "harness.conf")
	lines := append([]string{
		"set -g default-shell /bin/sh",
		"set -g default-command " + ConfQuote(c.shell.Command()),
		"set -g @moox-sockets " + ConfQuote(name),
		"set -g exit-empty on",
	}, c.harness...)
	writeFile(tb, root, "harness.conf", strings.Join(lines, "\n")+"\n")
	if c.localConf {
		writeFile(tb, filepath.Join(s.XC, "moox"), "local.conf", strings.Join(lines, "\n")+"\n")
	}
	tb.Cleanup(s.Close)
	return s, c, tmuxBin, conf, harness
}

// Env is the environment to run this tree's binaries in against this
// server: the scratch HOME/XDG roots, SHELL=/bin/sh, MOOX_BIN, MOOX_ROOT (the
// share root, so a moox built into a scratch dir finds its config), PATH
// with the scratch bin dir first (so `tt` is this tree's, never one
// installed on the machine),
// LANG, and TMUX_TMPDIR pointing at an empty scratch dir — so a bare name
// (-L default included) can never reach a real server — plus extra.
func (s *Server) Env(extra ...string) []string {
	tmp := filepath.Join(s.Root, "tmux-tmp")
	claude := filepath.Join(s.Home, ".claude")
	if d, err := statedir.Open(tmp, claude); err == nil {
		_ = d.Close()
	}
	env := append([]string{
		"PATH=" + s.Bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"LANG=" + langOrDefault(),
		"TMUX_TMPDIR=" + tmp,
		"MOOX_ROOT=" + s.Share,
	}, scratchEnv(s)...)
	return append(env, extra...)
}

func langOrDefault() string {
	if l := os.Getenv("LANG"); l != "" {
		return l
	}
	return "en_US.UTF-8"
}

// tempRoot creates os.TempDir()/mx<hex> through statedir and registers its
// removal (which runs after Close, because cleanups run last-in first-out).
func tempRoot(tb testing.TB) string {
	tb.Helper()
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatal(err)
	}
	base := os.TempDir()
	// macOS's per-user TMPDIR (/var/folders/xx/…/T/) is ~50 bytes, which
	// pushes the daemon's observer.sock (<root>/xs/tt/sockets/<key>/observer.sock)
	// past sockaddr_un's 104: the bind fails and the daemon exits. /tmp is
	// short and always present on macOS and Linux.
	if len(base) > 24 {
		if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
			base = "/tmp"
		}
	}
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	name := "mx" + hex.EncodeToString(b[:])
	root := filepath.Join(base, name)
	claude := filepath.Join(root, "home", ".claude")
	d, err := statedir.Open(root, claude)
	if err != nil {
		tb.Fatalf("tmuxtest: temp root: %v", err)
	}
	_ = d.Close()
	tb.Cleanup(func() {
		parent, err := statedir.OpenExisting(base, claude)
		if err != nil {
			tb.Errorf("tmuxtest: removing %s: %v", root, err)
			return
		}
		defer func() { _ = parent.Close() }()
		if err := parent.RemoveTree(name); err != nil {
			tb.Errorf("tmuxtest: removing %s: %v", root, err)
		}
	})
	return root
}

func writeFile(tb testing.TB, dir, name, content string) {
	tb.Helper()
	d, err := statedir.Open(dir, filepath.Join(dir, ".claude-none"))
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := d.WriteFile(name, []byte(content), 0o600); err != nil {
		tb.Fatal(err)
	}
}

// ShareRoot is <module root>/share/moox of the tree the test runs in.
func ShareRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return mooxroot.DevShare(wd)
}

// buildBinaries builds cmd/tt (and cmd/moox when present) into dir.
func buildBinaries(tb testing.TB, dir string) {
	tb.Helper()
	share, err := ShareRoot()
	if err != nil {
		tb.Fatal(err)
	}
	module := filepath.Dir(filepath.Dir(share))
	for _, c := range []string{"tt", "moox"} {
		if _, err := os.Stat(filepath.Join(module, "cmd", c)); err != nil {
			continue
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, c), "./cmd/"+c)
		cmd.Dir = module
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("tmuxtest: building %s: %v: %s", c, err, out)
		}
	}
}

// ConfQuote single-quotes s for a tmux config line.
func ConfQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runTimeout bounds every tmux command a test issues.
const runTimeout = 10 * time.Second

// Run runs one tmux command (or a tmuxtarget.Chain) on THIS server, always
// with -u -S, and fails the test on error.
func (s *Server) Run(tb testing.TB, args ...string) string {
	tb.Helper()
	out, err := s.RunErr(args...)
	if err != nil {
		tb.Fatalf("tmux %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// RunErr is Run for commands whose failure is the thing being measured.
func (s *Server) RunErr(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	out, err := s.T.Run(ctx, args...)
	return string(out), err
}

// Opt reads a global option's value ("" if unset).
func (s *Server) Opt(tb testing.TB, name string) string {
	tb.Helper()
	out, err := s.RunErr("show-options", "-gqv", name)
	if err != nil {
		tb.Fatalf("show-options %s: %v", name, err)
	}
	return strings.TrimRight(out, "\n")
}

// SourceMoox sources <share>/moox.conf on THIS socket (never the default),
// after pinning default-command to the configured shell, and fails the test
// with the daemon.log tail if the gate did not run (@moox-active unset after
// sourcing, TestE2E_GateRanOrFail, §3.1).
func (s *Server) SourceMoox(tb testing.TB) {
	tb.Helper()
	share, err := ShareRoot()
	if err != nil {
		tb.Fatal(err)
		return
	}
	if _, err := s.RunErr("source-file", filepath.Join(share, "moox.conf")); err != nil {
		tb.Fatalf("tmuxtest: source-file moox.conf: %v", err)
		return
	}
	if s.Opt(tb, "@moox-active") == "" {
		tb.Fatalf("tmuxtest: the gate did not run (@moox-active unset after sourcing moox.conf); daemon.log:\n%s",
			s.daemonLogTail())
	}
}

func (s *Server) daemonLogTail() string {
	matches, _ := filepath.Glob(filepath.Join(s.XS, "moox", "sockets", "*", "daemon.log"))
	if len(matches) == 0 {
		return "(no daemon.log under " + s.XS + ")"
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// Alive reports whether the server still answers.
func (s *Server) Alive() bool {
	_, err := s.RunErr("list-sessions", "-F", "#{session_id}")
	return err == nil
}

// Close ends the server WITHOUT kill-server: it kills every session on this
// socket, lets exit-empty end the server, and waits up to 2 s for the socket
// to stop answering. Idempotent; registered with t.Cleanup.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		out, err := s.RunErr("list-sessions", "-F", "#{session_id}")
		if err != nil {
			s.assertNoStragglers() // already gone: its processes must be too
			return
		}
		var cmds [][]string
		for _, id := range strings.Fields(out) {
			cmds = append(cmds, []string{"kill-session", "-t", id})
		}
		if len(cmds) > 0 {
			_, _ = s.RunErr(tmuxtarget.Chain(cmds...)...)
		}
		deadline := time.Now().Add(2 * time.Second)
		for s.Alive() {
			if time.Now().After(deadline) {
				s.tb.Errorf("tmuxtest: %s still answers 2 s after its sessions were killed (a client still attached?)", s.T.Socket)
				return
			}
			pause(20 * time.Millisecond)
		}
		s.assertNoStragglers()
	})
}

// stragglerWait is how long a per-server process (the daemon, a standalone
// observer, a waiter) may take to notice its server is gone (D8, F1): the
// waiter's zero-exit re-arm (100 ms), its confirmation, and the observer's
// shutdown.
const stragglerWait = 10 * time.Second

// assertNoStragglers is the harness's leak check (F1): once the server is
// gone, every process whose argv names THIS server's socket must exit
// within stragglerWait. One that does not is reported as a test failure and
// then SIGKILLed — it is this test's own (its argv names the private
// socket), so nothing else is ever signalled.
func (s *Server) assertNoStragglers() {
	needles := []string{s.T.Socket}
	if !strings.Contains(s.Name, "cold") {
		needles = append(needles, "-L "+s.Name+" ")
	}
	var left []straggler
	Poll(stragglerWait, 100*time.Millisecond, func() bool {
		left = stragglers(needles)
		return len(left) == 0
	})
	for _, p := range left {
		s.tb.Errorf("tmuxtest: pid %d outlived its server %s by %s: %s", p.pid, s.T.Socket, stragglerWait, p.command)
		if proc, err := os.FindProcess(p.pid); err == nil {
			_ = proc.Kill()
		}
	}
}

type straggler struct {
	pid     int
	command string
}

// stragglers lists processes (never this one) whose command line contains a
// needle, from one `ps`.
func stragglers(needles []string) []straggler {
	out, err := exec.Command("ps", "-ww", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var found []straggler
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmd := strings.Join(f[1:], " ") + " "
		if strings.HasPrefix(f[1], "ps") {
			continue
		}
		for _, n := range needles {
			if strings.Contains(cmd, n) {
				found = append(found, straggler{pid, strings.TrimSpace(cmd)})
				break
			}
		}
	}
	return found
}

// pause blocks for d. A one-shot timer armed by the caller and drained at
// once; test-harness code only (the §9.3a timer rule's four clauses hold:
// one-shot, armed by an event, self-disarming, never armed while idle).
func pause(d time.Duration) {
	t := time.NewTimer(d)
	<-t.C
}

// Poll calls cond until it returns true or within passes, and reports which.
func Poll(within, every time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		pause(every)
	}
}

// ErrOutsideTempDir is the hook-shell refusal.
var ErrOutsideTempDir = errors.New("tmuxtest: hook shell HOME/ZDOTDIR must be under t.TempDir()")

// HookZsh is the zsh hook-shell fixture (§12.2): `ZDOTDIR=<tmp> HOME=<tmp>
// zsh -d -i`, where <tmp> is a fresh t.TempDir() whose .zshrc is the single
// line `source <share>/shell/moox.zsh`. -d skips /etc/zsh* rc files.
func HookZsh(tb testing.TB) Shell {
	tb.Helper()
	sh, err := hookShell(tb, tb.TempDir(), "zsh")
	if err != nil {
		tb.Fatal(err)
	}
	return sh
}

// HookBash is the bash fixture: `HOME=<tmp> bash --noprofile --rcfile
// <tmp>/rc -i` with rc sourcing only <share>/shell/moox.bash.
func HookBash(tb testing.TB) Shell {
	tb.Helper()
	sh, err := hookShell(tb, tb.TempDir(), "bash")
	if err != nil {
		tb.Fatal(err)
	}
	return sh
}

// hookShell builds a fixture in dir, refusing unless dir is under this
// test's temp root (the parent of every t.TempDir()).
func hookShell(tb testing.TB, dir, kind string) (Shell, error) {
	testRoot := filepath.Dir(tb.TempDir())
	if !within(dir, testRoot) || filepath.Clean(dir) == filepath.Clean(testRoot) {
		return Shell{}, fmt.Errorf("%w: %s is not under %s", ErrOutsideTempDir, dir, testRoot)
	}
	share, err := ShareRoot()
	if err != nil {
		return Shell{}, err
	}
	q := shQuote
	switch kind {
	case "zsh":
		if err := writeFileErr(dir, ".zshrc", "source "+q(filepath.Join(share, "shell", "moox.zsh"))+"\n"); err != nil {
			return Shell{}, err
		}
		return Shell{kind: "zsh", cmd: "ZDOTDIR=" + q(dir) + " HOME=" + q(dir) + " zsh -d -i"}, nil
	case "bash":
		if err := writeFileErr(dir, "rc", "source "+q(filepath.Join(share, "shell", "moox.bash"))+"\n"); err != nil {
			return Shell{}, err
		}
		return Shell{kind: "bash", cmd: "HOME=" + q(dir) + " bash --noprofile --rcfile " + q(filepath.Join(dir, "rc")) + " -i"}, nil
	}
	return Shell{}, fmt.Errorf("tmuxtest: unknown hook shell %q", kind)
}

func writeFileErr(dir, name, content string) error {
	d, err := statedir.Open(dir, filepath.Join(dir, ".claude-none"))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.WriteFile(name, []byte(content), 0o600)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
