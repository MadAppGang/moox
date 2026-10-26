//go:build tmuxe2e

package tmuxtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// realStateUnchanged snapshots whether the real moox/tt state roots exist and
// their mtimes, and fails the test if a run changed either (read-only: only
// lstat is used on real paths).
func realStateUnchanged(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	paths := []string{
		filepath.Join(home, ".local", "state", "moox"),
		filepath.Join(home, ".config", "moox"),
		filepath.Join(home, ".config", "tmux-claude", "launch"),
	}
	type st struct {
		exists bool
		mtime  time.Time
	}
	before := map[string]st{}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		before[p] = st{err == nil, modTime(fi, err)}
	}
	t.Cleanup(func() {
		for _, p := range paths {
			fi, err := os.Lstat(p)
			now := st{err == nil, modTime(fi, err)}
			if p != paths[2] && now != before[p] { // launch/ is written by the LIVE server's real panes
				t.Errorf("real state %s changed during the test: %+v -> %+v", p, before[p], now)
			}
		}
	})
}

func modTime(fi os.FileInfo, err error) time.Time {
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func globalEnv(t *testing.T, s *tmuxtest.Server) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(s.Run(t, "show-environment", "-g")), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

// paneEcho runs a command in a new window of the server and returns what it
// printed to a scratch file (the pane then sleeps until Close).
func paneEcho(t *testing.T, s *tmuxtest.Server, target, script string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	args := []string{"new-window", "-d"}
	if target != "" {
		args = append(args, "-t", target)
	}
	s.Run(t, append(args, script+" > "+tmuxtest.ConfQuote(out)+"; exec sleep 120")...)
	var got string
	if !tmuxtest.Poll(5*time.Second, 20*time.Millisecond, func() bool {
		b, err := os.ReadFile(out)
		got = string(b)
		return err == nil && len(b) > 0
	}) {
		t.Fatalf("pane never wrote %s (script %q)", out, script)
	}
	return got
}

func TestStart_IsolatedServer(t *testing.T) {
	realStateUnchanged(t)
	t.Setenv("MOOX_STATE_DIR", "/should/not/leak")
	t.Setenv("TMUX_TMPDIR", t.TempDir()) // must not move the -L socket
	s := tmuxtest.Start(t)

	if !strings.HasPrefix(s.Name, "moox-test-") {
		t.Fatalf("socket name %q", s.Name)
	}
	if got := strings.TrimSpace(s.Run(t, "display", "-p", "#{socket_path}")); got != s.T.Socket {
		t.Fatalf("socket_path %q, Target %q", got, s.T.Socket)
	}
	if got := strings.TrimSpace(s.Run(t, "display", "-p", "#{config_files}")); strings.Contains(got, ".tmux.conf") {
		t.Fatalf("a user config was loaded: %q", got)
	}
	env := globalEnv(t, s)
	for k, want := range map[string]string{"HOME": s.Home, "XDG_STATE_HOME": s.XS, "SHELL": "/bin/sh", "MOOX_BIN": s.Bin} {
		if env[k] != want {
			t.Errorf("global env %s = %q, want %q", k, env[k], want)
		}
	}
	for k := range env {
		if k == "TMUX" || k == "TMUX_TMPDIR" || k == "MOOX_STATE_DIR" || k == "ENV" {
			t.Errorf("global env carries %s", k)
		}
	}
	if s.Opt(t, "default-shell") != "/bin/sh" || s.Opt(t, "default-command") != tmuxtest.PaneShell {
		t.Fatalf("panes are not pinned to /bin/sh")
	}
	if got := paneEcho(t, s, "", `printf '%s|%s' "$HOME" "${MOOX_STATE_DIR-unset}"`); got != s.Home+"|unset" {
		t.Fatalf("pane env HOME|MOOX_STATE_DIR = %q", got)
	}
	s.Close()
	if s.Alive() {
		t.Fatal("server still answers after Close")
	}
}

func TestStartCold_IsolatedAndRealConf(t *testing.T) {
	realStateUnchanged(t)
	share, err := tmuxtest.ShareRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOOX_STATE_DIR", "/should/not/leak")
	s := tmuxtest.StartCold(t, tmuxtest.WithName("moox-val-cold"))

	if !strings.HasPrefix(s.T.Socket, s.Root+string(filepath.Separator)) || filepath.Base(s.T.Socket) != "moox-val-cold" {
		t.Fatalf("socket %q not under the temp root %q", s.T.Socket, s.Root)
	}
	files := strings.TrimSpace(s.Run(t, "display", "-p", "#{config_files}"))
	if want := filepath.Join(s.Root, "harness.conf") + "," + filepath.Join(share, "moox.conf"); files != want {
		t.Fatalf("config_files = %q, want %q", files, want)
	}
	// The real entry file ran: root from #{d:current_file} (PV1), bin from
	// $MOOX_BIN in the server env (E6), and -o kept the harness's sockets.
	if got := s.Opt(t, "@moox-root"); got != share {
		t.Errorf("@moox-root = %q, want %q", got, share)
	}
	if got := s.Opt(t, "@moox-bin"); got != s.Bin {
		t.Errorf("@moox-bin = %q, want MOOX_BIN %q", got, s.Bin)
	}
	if got := s.Opt(t, "@moox-sockets"); got != "moox-val-cold" {
		t.Errorf("@moox-sockets = %q: moox.conf's -o overwrote the harness", got)
	}
	if s.Opt(t, "exit-empty") != "on" || s.Opt(t, "default-command") != tmuxtest.PaneShell {
		t.Errorf("harness options missing")
	}
	env := globalEnv(t, s)
	for k, want := range map[string]string{"HOME": s.Home, "XDG_STATE_HOME": s.XS, "XDG_DATA_HOME": s.XD,
		"XDG_CONFIG_HOME": s.XC, "SHELL": "/bin/sh", "MOOX_BIN": s.Bin} {
		if env[k] != want {
			t.Errorf("global env %s = %q, want %q", k, env[k], want)
		}
	}
	for k := range env {
		switch {
		case k == "TMUX", k == "TMUX_TMPDIR", k == "MOOX_STATE_DIR", strings.HasPrefix(k, "TT_"):
			t.Errorf("explicit env leaked %s", k)
		}
	}
	if got := paneEcho(t, s, "", `printf '%s|%s|%s' "$HOME" "$XDG_STATE_HOME" "${TMUX%%,*}"`); got != s.Home+"|"+s.XS+"|"+s.T.Socket {
		t.Fatalf("pane env = %q", got)
	}
	s.Close()
	if s.Alive() {
		t.Fatal("server still answers after Close")
	}
}

func TestStartCold_RemovesItsTempRoot(t *testing.T) {
	var root string
	t.Run("cold", func(t *testing.T) {
		s := tmuxtest.StartCold(t)
		root = s.Root
		if _, err := os.Lstat(root); err != nil {
			t.Fatal(err)
		}
	})
	// the subtest's cleanups (Close, then the root's removal) have run
	if _, err := os.Lstat(root); err == nil {
		t.Fatalf("temp root %s was not removed", root)
	}
}

func TestHookShells_RunWithScratchHome(t *testing.T) {
	for _, mk := range []func(testing.TB) tmuxtest.Shell{tmuxtest.HookZsh, tmuxtest.HookBash} {
		sh := mk(t)
		if _, err := lookPath(sh.Kind()); err != nil {
			t.Logf("skip %s: %v", sh.Kind(), err)
			continue
		}
		t.Run(sh.Kind(), func(t *testing.T) {
			s := tmuxtest.Start(t, tmuxtest.WithShell(sh))
			if got := s.Opt(t, "default-command"); got != sh.Command() {
				t.Fatalf("default-command = %q", got)
			}
			s.Run(t, "new-window", "-d", "-n", "hook")
			ok := tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
				out, _ := s.RunErr("display", "-p", "-t", ":hook", "#{pane_current_command}")
				return strings.TrimSpace(out) == sh.Kind()
			})
			if !ok {
				out, _ := s.RunErr("display", "-p", "-t", ":hook", "#{pane_current_command}")
				t.Fatalf("hook pane runs %q, want %s", strings.TrimSpace(out), sh.Kind())
			}
		})
	}
}

// recTB records a Fatalf instead of stopping, so the gate-ran check's failure
// path can itself be tested.
type recTB struct {
	testing.TB
	fatal string
}

func (r *recTB) Helper() {}
func (r *recTB) Fatalf(format string, args ...any) {
	if r.fatal == "" {
		r.fatal = strings.TrimSpace(strings.ReplaceAll(format, "%", "")) // shape only
	}
}
func (r *recTB) Fatal(args ...any) { r.fatal = "fatal" }

func TestSourceMoox_FailsWhenTheGateDidNotRun(t *testing.T) {
	// P1's moox.conf has no gate yet, so @moox-active stays unset: exactly the
	// "gate never ran" case SourceMoox must turn into a failure (§3.1).
	s := tmuxtest.Start(t)
	rec := &recTB{TB: t}
	s.SourceMoox(rec)
	if !strings.Contains(rec.fatal, "the gate did not run") {
		t.Fatalf("SourceMoox did not fail on an unset @moox-active (got %q)", rec.fatal)
	}
	if s.Opt(t, "@moox-root") == "" {
		t.Fatal("moox.conf was not sourced on the scratch socket")
	}
}
