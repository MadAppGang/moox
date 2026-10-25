package blackbox_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This package deliberately imports no moox packages. Its only product interfaces
// are the supplied executables, tmux's public commands, and documented state files.
type result struct {
	code int
	out  string
	err  string
}

type sandbox struct {
	t                    *testing.T
	base, home, config   string
	state, data, runtime string
	tmp, share, moox, tt string
	tmux, nonce          string
	realTmux             string    // the tmux the guard wraps; cleanup addresses own sockets with it
	timing               [3]string // @moox-save-debounce, -max-wait, -backup for new servers
	env                  map[string]string
	servers              []*server
	processes            map[int]string // pid -> birth time; avoids PID-reuse kills
}

type server struct {
	e                *sandbox
	name, path, conf string
	pid              int
	stopped          bool
}

func newSandbox(t *testing.T, needs ...string) *sandbox {
	t.Helper()
	values := map[string]string{}
	for _, key := range needs {
		value := os.Getenv(key)
		if value == "" {
			t.Skipf("%s is unset; supply the orchestrator-built artifact", key)
		}
		abs, err := filepath.Abs(value)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("%s=%q: %v", key, abs, err)
		}
		if key == "MOOX_SHARE" {
			if !info.IsDir() {
				t.Fatalf("%s is not a directory", key)
			}
		} else if info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatalf("%s is not an executable file", key)
		}
		values[key] = abs
	}
	realTmux := os.Getenv("TMUX_BIN")
	if realTmux == "" {
		realTmux = "tmux"
	}
	realTmux, err := exec.LookPath(realTmux)
	if err != nil {
		t.Skipf("tmux prerequisite unavailable: %v", err)
	}
	realTmux, err = filepath.Abs(realTmux)
	if err != nil {
		t.Fatal(err)
	}
	// A short temp prefix avoids macOS's Unix socket pathname limit.
	// Keep it short: the canonical "/private" prefix plus XS/tt/sockets/<key>/
	// observer.sock must stay under the 104-byte Unix socket path limit.
	lexical, err := os.MkdirTemp("/tmp", "mb")
	if err != nil {
		t.Fatal(err)
	}
	// macOS /tmp is a symlink to /private/tmp, and tmux reports #{socket_path}
	// (hence TMUX and MOOX_SOCKET) canonically. Use the canonical root everywhere
	// so the guard and requireInside recognise the test's own sockets; the guard
	// still denies every socket outside this one directory.
	base, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		_ = os.RemoveAll(lexical)
		t.Fatal(err)
	}
	// Registered first, so it runs last. The directory holds every server's
	// socket: removing it while a server still answers would orphan that
	// server where nothing can address it. Keep it for TestMain's sweep.
	var e *sandbox
	t.Cleanup(func() {
		if e != nil {
			if alive := e.liveServers(); len(alive) != 0 {
				t.Errorf("own tmux server(s) %q still answer after cleanup; keeping %s for the final sweep", alive, base)
				keepForSweep(base)
				return
			}
		}
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("remove isolated test directory: %v", err)
		}
	})
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	e = &sandbox{
		t: t, base: base, nonce: hex.EncodeToString(token), realTmux: realTmux,
		home: filepath.Join(base, "home"), config: filepath.Join(base, "config"),
		state: filepath.Join(base, "state"), data: filepath.Join(base, "data"),
		runtime: filepath.Join(base, "runtime"), tmp: filepath.Join(base, "tmp"),
		share: values["MOOX_SHARE"], processes: map[int]string{},
	}
	for _, dir := range []string{e.home, e.config, e.state, e.data, e.runtime, e.tmp,
		filepath.Join(base, "tmux"), filepath.Join(base, "bin"), filepath.Join(base, "control")} {
		mustMkdir(t, dir)
	}
	// Do not inherit TMUX, shell startup injection, continuity paths, credentials,
	// or the user's XDG paths. Only PATH is inherited for packaged dependencies.
	e.env = map[string]string{
		"HOME": e.home, "XDG_CONFIG_HOME": e.config, "XDG_STATE_HOME": e.state,
		"XDG_DATA_HOME": e.data, "XDG_RUNTIME_DIR": e.runtime,
		"TMUX_TMPDIR": filepath.Join(base, "tmux"), "TMPDIR": e.tmp,
		"SHELL": "/bin/sh", "ENV": "", "BASH_ENV": "", "ZDOTDIR": e.home,
		"PATH": filepath.Join(base, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TERM": "xterm-256color", "LANG": "C", "LC_ALL": "C",
		"USER": "moox-blackbox", "LOGNAME": "moox-blackbox", "ACCESSIBLE": "1",
		"CC_NO_NUDGE": "1", "MOOX_STUB_COMMANDS": "1",
	}
	e.tmux = filepath.Join(base, "bin", "tmux")
	// The guard protects the real server even if the product drops its socket
	// argument. It is a safety boundary, not an assertion about internal calls.
	guard := "#!/bin/sh\nroot=" + shQuote(base) + "\nreal=" + shQuote(realTmux) + "\n" + `
argv=; for a do argv="$argv '$a'"; done
deny() { printf '%s | argv:%s | TMUX=%s | parent=%s\n' "$*" "$argv" "${TMUX-}" "$(ps -o command= -p "$PPID" 2>/dev/null | cut -c1-200)" >> "$root/control/guard-denials"; exit 97; }
if [ "$#" -eq 1 ] && [ "$1" = "-V" ]; then exec "$real" "$@"; fi
owned=0
case "${TMUX-}" in "$root"/tmux/tmux-*/moox-bb-*,*) owned=1 ;; esac
inherited=$owned
check_name() { case "$1" in moox-bb-NONCE-*) owned=1 ;; *) deny "foreign socket name: $1" ;; esac; }
check_path() { case "$1" in "$root"/*) owned=1 ;; *) deny "foreign socket path: $1" ;; esac; }
# tmux honours -L/-S only among the global options before the first command
# word; "resize-pane -L 5" is a subcommand flag. Parse the global options
# exactly, deny any cluster that could hide -L/-S, then scan every remaining
# word (all commands of a ";" chain, bind-key bodies) for forbidden verbs.
expect=
global=1
for arg do
  if [ -n "$expect" ]; then
    case "$expect" in name) check_name "$arg" ;; path) check_path "$arg" ;; esac
    expect=
    continue
  fi
  if [ "$global" -eq 1 ]; then
    case "$arg" in
      --) global=0; continue ;;
      -L) expect=name; continue ;;
      -L?*) check_name "${arg#-L}"; continue ;;
      -S) expect=path; continue ;;
      -S?*) check_path "${arg#-S}"; continue ;;
      -c|-f|-T) expect=other; continue ;;
      -*[LScfT]*) deny "unparsed global option cluster: $arg" ;;
      -*) continue ;;
      *) global=0 ;;
    esac
  fi
  case "$arg" in
    kill-server) deny "kill-server is prohibited" ;;
    run-shell|run) [ "$inherited" -eq 1 ] || deny "tool-client run-shell is prohibited" ;;
  esac
done
[ "$owned" -eq 1 ] || deny "tmux without an owned socket"
exec "$real" "$@"
`
	guard = strings.ReplaceAll(guard, "NONCE", e.nonce)
	writeFile(t, e.tmux, guard, 0700)
	e.env["MOOX_TMUX_BIN"], e.env["TT_TMUX_BIN"] = e.tmux, e.tmux
	// Give nested scripts the required canonical executable names even when the
	// orchestrator's input executable has a temporary, noncanonical filename.
	for key, name := range map[string]string{"MOOX_BIN": "moox", "TT_BIN": "tt"} {
		if binary := values[key]; binary != "" {
			target := filepath.Join(base, "bin", name)
			if err := os.Symlink(binary, target); err != nil {
				t.Fatal(err)
			}
			// The orchestrator's MOOX_BIN/TT_BIN name input files. In the
			// product's own environment MOOX_BIN overrides @moox-bin, which
			// §12.4 defines as an absolute *directory* (moox.conf runs the
			// tools from it), so export the directory holding moox and tt.
			if name == "moox" {
				e.env["MOOX_BIN"] = filepath.Join(base, "bin")
			}
			if name == "moox" {
				e.moox = target
			} else {
				e.tt = target
			}
		}
	}
	if e.share != "" {
		e.env["MOOX_SHARE"], e.env["MOOX_ROOT"] = e.share, e.share
	}
	// Even CLI-only commands receive a unique, nonexistent socket, never default.
	socket := filepath.Join(base, "unstarted.sock")
	e.env["MOOX_TMUX_SOCKET"], e.env["TT_TMUX_SOCKET"] = socket, socket
	// A buggy metadata/setup command could unexpectedly create this server.
	// Register it too, so even that failure is isolated and cleaned up.
	unstarted := &server{e: e, path: socket}
	e.servers = append(e.servers, unstarted)
	track(unstarted)
	t.Cleanup(e.cleanup)
	return e
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// tmux source-file uses its own quoting rules. In particular literal # and ;
// must not become format expansion or command separators.
func tmuxQuote(s string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$").Replace(s) + "\""
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, data string, mode fs.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (e *sandbox) run(binary string, env map[string]string, input string, args ...string) result {
	e.t.Helper()
	r, err := e.runE(binary, env, input, args...)
	if err != nil {
		e.t.Fatal(err.Error())
	}
	return r
}

// runE is run without t.Fatal, for cleanup: a fatal cleanup would skip the
// rest of the teardown and leave servers behind.
func (e *sandbox) runE(binary string, env map[string]string, input string, args ...string) (result, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = e.base
	merged := make(map[string]string, len(e.env)+len(env))
	for key, value := range e.env {
		merged[key] = value
	}
	for key, value := range env {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cmd.Env = append(cmd.Env, key+"="+merged[key])
	}
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	e.collectProcesses()
	if ctx.Err() != nil {
		return result{}, fmt.Errorf("command deadline (harness limit, not a product performance oracle): %s %q\nstdout: %s\nstderr: %s",
			binary, args, stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			return result{}, fmt.Errorf("could not execute %s %q: %v", binary, args, err)
		}
	}
	return result{code, stdout.String(), stderr.String()}, nil
}

func requireCode(t *testing.T, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, want, r.out, r.err)
	}
}

func (e *sandbox) mooxRun(args ...string) result {
	e.t.Helper()
	return e.run(e.moox, nil, "", args...)
}

func (s *server) mooxRun(args ...string) result {
	s.e.t.Helper()
	// §8.1 places the selector after the verb (after show/unpin for snapshot).
	// All servers made by this harness have owned -L names; do not rely on an
	// undocumented global -S prefix for save/restore/doctor/snapshot.
	at := 1
	if len(args) > 1 && args[0] == "snapshot" {
		at = 2
	}
	selected := append([]string{}, args[:at]...)
	selected = append(selected, "-L", s.name)
	selected = append(selected, args[at:]...)
	return s.e.run(s.e.moox, map[string]string{"MOOX_TMUX_SOCKET": s.name}, "", selected...)
}

func (s *server) tmuxRun(args ...string) result {
	s.e.t.Helper()
	// -u: the sandbox runs with LC_ALL=C, under which the tmux client prints
	// non-ASCII (e.g. "✳") as "_"; oracles must see the server's real bytes.
	selector := []string{"-u", "-L", s.name}
	if s.name == "" {
		selector = []string{"-u", "-S", s.path}
	}
	return s.e.run(s.e.tmux, nil, "", append(selector, args...)...)
}

func (s *server) mustTmux(args ...string) string {
	s.e.t.Helper()
	r := s.tmuxRun(args...)
	requireCode(s.e.t, r, 0)
	return strings.TrimSuffix(r.out, "\n")
}

func (e *sandbox) newServer(label, sockets string, cold bool) *server {
	e.t.Helper()
	s := &server{e: e, name: "moox-bb-" + e.nonce + "-" + label}
	if sockets == "SELF" {
		sockets = s.name
	}
	s.conf = filepath.Join(e.base, "control", label+".conf")
	// Save timing is fixed when the daemon starts (daemon.log SAVER-START), and
	// the contract does not promise live re-reads, so event tests set it here.
	timing := e.timing
	if timing == [3]string{} {
		timing = [3]string{"1h", "1h", "0"}
	}
	policy := ""
	if sockets != "DEFAULT_POLICY" {
		policy = "set -g @moox-sockets " + tmuxQuote(sockets) + "\n"
	}
	config := "set -g default-shell /bin/sh\nset -g default-command /bin/sh\n" +
		"set -g exit-empty on\nset -g exit-unattached off\nset -g status off\n" +
		"set -g automatic-rename off\nset -g allow-rename off\n" +
		policy +
		"set -g @moox-save-debounce " + timing[0] + "\nset -g @moox-save-max-wait " + timing[1] +
		"\nset -g @moox-save-backup " + timing[2] + "\n" +
		"set -g @moox-capture-contents off\n" +
		"set -g @claude-continuity-by-pid-dir " + tmuxQuote(filepath.Join(e.base, "by-pid", label)) + "\n" +
		"source-file " + tmuxQuote(filepath.Join(e.share, "moox.conf")) + "\n" +
		"set -g default-shell /bin/sh\nset -g default-command /bin/sh\n" +
		"set -g automatic-rename off\nset -g allow-rename off\nset -g status off\n" +
		"set -g base-index 0\nset -g pane-base-index 0\nset -g exit-empty on\nset -g exit-unattached off\n"
	writeFile(e.t, s.conf, config, 0600)
	e.servers = append(e.servers, s) // register before starting, even if startup fails
	track(s)                         // and for TestMain's final sweep
	e.t.Cleanup(s.stopForCleanup)    // each server still cleans up if another cleanup fails
	env := map[string]string{"MOOX_TMUX_SOCKET": s.name, "TT_TMUX_SOCKET": s.name}
	if !cold {
		env["MOOX_NO_AUTO_RESTORE"] = "1"
	}
	r := e.run(e.tmux, env, "", "-L", s.name, "-f", s.conf,
		"new-session", "-d", "-s", "bootstrap", "-x", "180", "-y", "50", "-c", e.home, "/bin/sh")
	requireCode(e.t, r, 0)
	s.path = s.mustTmux("display-message", "-p", "#{socket_path}")
	retrack(s)
	e.requireInside(s.path)
	pidText := s.mustTmux("display-message", "-p", "#{pid}")
	s.pid, _ = strconv.Atoi(pidText)
	e.rememberPID(pidText)
	e.rememberPanePIDs(s)
	if got := s.mustTmux("show-options", "-gqv", "default-shell"); got != "/bin/sh" {
		e.t.Fatalf("unsafe pane default shell %q, want /bin/sh", got)
	}
	eventually(e.t, 20*time.Second, "the shipped config's socket gate", func() (bool, string) {
		r := s.tmuxRun("show-options", "-gqv", "@moox-active")
		v := strings.TrimSpace(r.out)
		return r.code == 0 && (v == "0" || v == "1"), fmt.Sprintf("active=%q stderr=%s", v, r.err)
	})
	e.collectDaemonPID(s)
	return s
}

func (e *sandbox) requireInside(path string) {
	e.t.Helper()
	rel, err := filepath.Rel(e.base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsAbs(path) {
		e.t.Fatalf("refusing access outside isolated root: %q", path)
	}
	// A product-created symlink must not turn a lexical scratch path into a
	// read of the real home. Resolve the deepest existing ancestor as well.
	root, err := filepath.EvalSymlinks(e.base)
	if err != nil {
		e.t.Fatal(err)
	}
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				e.t.Fatalf("resolve isolated path %q: %v", current, err)
			}
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			e.t.Fatalf("isolated path escapes through a symlink: %q", path)
		}
		return
	}
}

func eventually(t *testing.T, timeout time.Duration, description string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, detail := check()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %s", description, detail)
		}
		// Bounded condition polling, never an unconditional startup sleep.
		timer := time.NewTimer(40 * time.Millisecond)
		<-timer.C
	}
}

func (s *server) waitReady() {
	s.e.t.Helper()
	eventually(s.e.t, 30*time.Second, "boot gate completion and released restore guard", func() (bool, string) {
		r := s.tmuxRun("show-options", "-gqv", "@moox-boot-result")
		guard := s.tmuxRun("show-options", "-gqv", "@moox-restoring")
		return r.code == 0 && strings.TrimSpace(r.out) != "" && strings.TrimSpace(guard.out) == "",
			fmt.Sprintf("boot=%q guard=%q", r.out, guard.out)
	})
}

func (s *server) globalEnv(key string) string {
	s.e.t.Helper()
	r := s.tmuxRun("show-environment", "-g", key)
	if r.code != 0 || strings.HasPrefix(r.out, "-"+key) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(r.out, key+"="), "\n")
}

func (s *server) requireActive(want string) {
	s.e.t.Helper()
	if got := s.mustTmux("show-options", "-gqv", "@moox-active"); got != want {
		s.e.t.Fatalf("@moox-active=%q, want %q", got, want)
	}
	if got := s.globalEnv("MOOX_ACTIVE"); got != want {
		s.e.t.Fatalf("exported MOOX_ACTIVE=%q, want %q", got, want)
	}
	if got := s.globalEnv("MOOX_SOCKET"); got != s.path {
		s.e.t.Fatalf("MOOX_SOCKET=%q, want verbatim socket path %q", got, s.path)
	}
}

func (s *server) makeRestorable() {
	s.e.t.Helper()
	s.waitReady()
	s.mustTmux("rename-session", "-t", "bootstrap", "magus")
	s.mustTmux("new-window", "-d", "-t", "magus:", "-n", "second", "-c", s.e.home, "/bin/sh")
	s.e.rememberPanePIDs(s)
}

type fileImage struct {
	Mode fs.FileMode
	Data string
	Link string
}

// The walker never follows directory symlinks, including links to the real home.
func image(t *testing.T, root string) map[string]fileImage {
	t.Helper()
	got := map[string]fileImage{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		row := fileImage{Mode: info.Mode()}
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			row.Data = string(data)
		case info.Mode()&os.ModeSymlink != 0:
			row.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		case info.IsDir():
		default:
			// Sockets are not config/state payloads; all tmux sockets live in
			// a separately isolated root and are checked by server cleanup.
			return nil
		}
		got[rel] = row
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return got
}

func equalImage(t *testing.T, got, want map[string]fileImage, label string) {
	t.Helper()
	// The guard's own log is harness output, reported once by cleanup with the
	// exact command line; it is not a product write to the isolated tree.
	const guardLog = "control/guard-denials"
	if _, ok := got[guardLog]; ok {
		got = copyImageWithout(got, guardLog)
	}
	if _, ok := want[guardLog]; ok {
		want = copyImageWithout(want, guardLog)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s changed unexpectedly\ngot: %#v\nwant: %#v", label, got, want)
	}
}

func copyImageWithout(in map[string]fileImage, drop string) map[string]fileImage {
	out := make(map[string]fileImage, len(in))
	for key, value := range in {
		if key != drop {
			out[key] = value
		}
	}
	return out
}

func (e *sandbox) pointerFiles() []string {
	e.t.Helper()
	var paths []string
	for _, root := range []string{e.home, e.config, e.state, e.data} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Name() == "moox-last" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	sort.Strings(paths)
	return paths
}

func (e *sandbox) singleSnapshot() (pointer, target string) {
	e.t.Helper()
	paths := e.pointerFiles()
	if len(paths) != 1 {
		e.t.Fatalf("moox-last files=%q, want exactly one after saving one server", paths)
	}
	pointer = paths[0]
	raw := string(readFile(e.t, pointer))
	if !strings.HasSuffix(raw, "\n") || strings.Count(raw, "\n") != 1 {
		e.t.Fatalf("moox-last=%q, want exactly one newline-terminated basename", raw)
	}
	name := strings.TrimSuffix(raw, "\n")
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		e.t.Fatalf("moox-last is not a basename: %q", name)
	}
	target = filepath.Join(filepath.Dir(pointer), name)
	e.requireInside(target)
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		e.t.Fatalf("moox-last target is not a nonempty regular file: %q (%v)", target, err)
	}
	for _, path := range []string{pointer, target} {
		info, err := os.Lstat(path)
		if err != nil {
			e.t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			e.t.Errorf("%s mode=%v, want regular 0600", path, info.Mode())
		}
	}
	info, err = os.Stat(filepath.Dir(pointer))
	if err != nil {
		e.t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		e.t.Errorf("snapshot directory mode=%v, want 0700", info.Mode())
	}
	return pointer, target
}

func (s *server) save() (string, string) {
	s.e.t.Helper()
	s.waitReady()
	requireCode(s.e.t, s.mooxRun("save", "--direct"), 0)
	return s.e.singleSnapshot()
}

func (e *sandbox) rememberPID(raw string) {
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return
	}
	birth, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err == nil && strings.TrimSpace(string(birth)) != "" {
		e.processes[pid] = strings.TrimSpace(string(birth))
	}
}

func (e *sandbox) rememberPanePIDs(s *server) {
	r := s.tmuxRun("list-panes", "-a", "-F", "#{pane_pid}")
	if r.code == 0 {
		for _, pid := range strings.Fields(r.out) {
			e.rememberPID(pid)
		}
	}
}

func (e *sandbox) collectProcesses() {
	if len(e.processes) == 0 {
		return
	}
	// Read only PID/PPID metadata, not another user's command lines or files.
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		e.t.Errorf("cannot inventory test-owned descendants: %v", err)
		return
	}
	for changed := true; changed; {
		changed = false
		for _, row := range strings.Split(string(out), "\n") {
			fields := strings.Fields(row)
			if len(fields) != 2 {
				continue
			}
			pid, _ := strconv.Atoi(fields[0])
			ppid, _ := strconv.Atoi(fields[1])
			_, known := e.processes[pid]
			if _, owned := e.processes[ppid]; owned && !known {
				before := len(e.processes)
				e.rememberPID(fields[0])
				changed = changed || len(e.processes) != before
			}
		}
	}
}

// Status JSON field names are not part of the supplied schema. Read PID-like
// fields solely for safe cleanup, without asserting an undocumented status shape.
func (e *sandbox) collectDaemonPID(s *server) { e.daemonPIDs(s) }

// waitDaemonsGone waits until the given owned pids have exited, so a test
// that ages state files cannot race a daemon's final log write.
func (e *sandbox) waitDaemonsGone(pids []int) {
	e.t.Helper()
	eventually(e.t, 10*time.Second, "stopped servers' daemons to exit", func() (bool, string) {
		for _, pid := range pids {
			if birth, ok := e.processes[pid]; ok && e.sameProcessAlive(pid, birth) {
				return false, fmt.Sprintf("pid %d alive", pid)
			}
		}
		return true, ""
	})
}

func (e *sandbox) daemonPIDs(s *server) (pids []int) {
	if e.tt == "" || s.path == "" {
		return nil
	}
	return e.parseDaemonPIDs(e.run(e.tt, nil, "", "serve", "--status", "--json", "--tmux-socket", s.path))
}

// quietDaemonPIDs is daemonPIDs for cleanup: a failing status call is
// reported, never fatal.
func (e *sandbox) quietDaemonPIDs(s *server) {
	if e.tt == "" || s.path == "" {
		return
	}
	r, err := e.runE(e.tt, nil, "", "serve", "--status", "--json", "--tmux-socket", s.path)
	if err != nil {
		e.t.Errorf("cleanup: own tt status: %v", err)
		return
	}
	e.parseDaemonPIDs(r)
}

func (e *sandbox) parseDaemonPIDs(r result) (pids []int) {
	var value any
	if json.Unmarshal([]byte(r.out), &value) != nil {
		return
	}
	var visit func(any)
	visit = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				if strings.Contains(strings.ToLower(key), "pid") {
					raw := ""
					switch n := value.(type) {
					case float64:
						raw = strconv.FormatInt(int64(n), 10)
					case string:
						raw = n
					}
					if raw != "" {
						e.rememberPID(raw)
						if pid, err := strconv.Atoi(raw); err == nil && pid > 1 {
							pids = append(pids, pid)
						}
					}
				} else {
					visit(value)
				}
			}
		case []any:
			for _, value := range v {
				visit(value)
			}
		}
	}
	visit(value)
	return pids
}

func (e *sandbox) sameProcessAlive(pid int, birth string) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	got, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	return err == nil && strings.TrimSpace(string(got)) == birth
}

// stop ends this server mid-test; a server that will not end fails the test
// here, as before.
func (s *server) stop() {
	s.e.t.Helper()
	if !s.end() {
		s.e.t.Fatalf("own server %s did not exit after its last session", s.label())
	}
}

// stopForCleanup is stop for t.Cleanup: every failure is reported with
// t.Errorf, never t.Fatal, so the rest of the teardown still runs.
func (s *server) stopForCleanup() {
	s.e.t.Helper()
	if !s.end() {
		s.e.t.Errorf("cleanup: own server %s did not exit after its last session", s.label())
	}
}

func (s *server) label() string {
	if s.name != "" {
		return s.name
	}
	return s.socketPath()
}

// socketPath is the server's own socket: the reported path, else where tmux
// puts -L <name> under this sandbox's TMUX_TMPDIR.
func (s *server) socketPath() string {
	if s.path != "" {
		return s.path
	}
	if s.name == "" {
		return ""
	}
	return filepath.Join(s.e.base, "tmux", "tmux-"+strconv.Itoa(os.Getuid()), s.name)
}

// quietTmux runs the real tmux against this server's own socket only,
// bounded, without t.Fatal.
func (s *server) quietTmux(args ...string) (string, bool) {
	return ownTmux(s.e.realTmux, s.e.base, s.socketPath(), args...)
}

// answers reports whether a tmux server still answers on this server's socket.
func (s *server) answers() bool {
	_, ok := s.quietTmux("list-sessions", "-F", "#{session_id}")
	return ok
}

// end stops this server's tt, then kills its sessions by stable id until no
// server answers on its socket (a session created meanwhile, e.g. by a restore
// still running, goes on the next pass), then waits for its pid to exit.
// Sessions only: the server itself is never killed, even on a test socket.
func (s *server) end() bool {
	s.e.t.Helper()
	if s.stopped {
		return true
	}
	e := s.e
	if s.path == "" {
		// Startup may fail after the server starts; the isolated name is
		// still enough to find and tear down only that server.
		if out, ok := s.quietTmux("display-message", "-p", "#{socket_path}"); ok && strings.TrimSpace(out) != "" {
			s.path = strings.TrimSpace(out)
			retrack(s)
		}
	}
	if out, ok := s.quietTmux("display-message", "-p", "#{pid}"); ok {
		if pid, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			s.pid = pid
		}
		e.rememberPID(out)
	}
	e.quietDaemonPIDs(s)
	if out, ok := s.quietTmux("list-panes", "-a", "-F", "#{pane_pid}"); ok {
		for _, pid := range strings.Fields(out) {
			e.rememberPID(pid)
		}
	}
	e.collectProcesses()
	if e.tt != "" && s.path != "" {
		r, err := e.runE(e.tt, nil, "", "serve", "--stop", "--tmux-socket", s.path)
		if err != nil || r.code != 0 {
			e.t.Errorf("stop own server's tt: exit %d: %s %s %v", r.code, r.out, r.err, err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, ok := s.quietTmux("list-sessions", "-F", "#{session_id}")
		if !ok {
			break
		}
		for _, id := range strings.Fields(out) {
			s.quietTmux("kill-session", "-t", id)
		}
		if time.Now().After(deadline) {
			e.t.Errorf("own server %s still answers 15s after killing its sessions: %q", s.label(), out)
			return false
		}
		<-time.After(40 * time.Millisecond)
	}
	if birth, ok := e.processes[s.pid]; ok {
		for until := time.Now().Add(8 * time.Second); e.sameProcessAlive(s.pid, birth); {
			if time.Now().After(until) {
				e.t.Errorf("own server %s (pid %d) no longer answers but has not exited", s.label(), s.pid)
				return false
			}
			<-time.After(40 * time.Millisecond)
		}
	}
	s.stopped = true
	return true
}

// liveServers lists this sandbox's servers that still answer on their sockets.
func (e *sandbox) liveServers() (alive []string) {
	for _, s := range e.servers {
		if s.answers() {
			alive = append(alive, s.label())
		}
	}
	return alive
}

func (e *sandbox) cleanup() {
	for _, s := range e.servers {
		s.stopForCleanup()
	}
	if e.t.Failed() {
		// Diagnostics only (the product's documented daemon.log, §12.4); never an oracle.
		_ = filepath.WalkDir(e.state, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == "daemon.log" {
				if data, err := os.ReadFile(path); err == nil {
					lines := strings.Split(strings.TrimSpace(string(data)), "\n")
					if len(lines) > 30 {
						lines = lines[len(lines)-30:]
					}
					e.t.Logf("diagnostic %s (last %d lines):\n%s", path, len(lines), strings.Join(lines, "\n"))
				}
			}
			return nil
		})
	}
	e.collectProcesses()
	deadline := time.Now().Add(8 * time.Second)
	for {
		var alive []int
		for pid, birth := range e.processes {
			if e.sameProcessAlive(pid, birth) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			break
		}
		if time.Now().After(deadline) {
			sort.Ints(alive)
			e.t.Errorf("test-started processes still alive after session/tt cleanup: %v", alive)
			// A leak is already a failure. Terminate only birth-checked owned
			// descendants, never a tmux server (which ends via its sessions).
			for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
				for _, pid := range alive {
					isServer := false
					for _, s := range e.servers {
						isServer = isServer || s.pid == pid
					}
					if !isServer && e.sameProcessAlive(pid, e.processes[pid]) {
						if err := syscall.Kill(pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
							e.t.Errorf("terminate own leaked process %d: %v", pid, err)
						}
					}
				}
				until := time.Now().Add(2 * time.Second)
				for time.Now().Before(until) {
					remaining := false
					for _, pid := range alive {
						remaining = remaining || e.sameProcessAlive(pid, e.processes[pid])
					}
					if !remaining {
						break
					}
					<-time.After(40 * time.Millisecond)
				}
			}
			for _, pid := range alive {
				if e.sameProcessAlive(pid, e.processes[pid]) {
					e.t.Errorf("owned process %d is still alive at final cleanup assertion", pid)
				}
			}
			break
		}
		<-time.After(50 * time.Millisecond)
	}
	if data, err := os.ReadFile(filepath.Join(e.base, "control", "guard-denials")); err == nil && len(data) != 0 {
		e.t.Errorf("safety guard blocked a forbidden tmux invocation:\n%s", data)
	}
}

// Views compare observable geometry instead of raw layout checksums/pane IDs,
// which correctly change on a new tmux server.
func (s *server) view() []string {
	s.e.t.Helper()
	// tmux 3.8 prints a TAB in -F output as "_", so use a printable separator.
	format := strings.Join([]string{"#{session_name}", "#{window_index}", "#{window_name}", "#{pane_index}",
		"#{pane_current_path}", "#{pane_title}", "#{pane_left},#{pane_top},#{pane_width},#{pane_height}"}, viewSep)
	rows := strings.Split(s.mustTmux("list-panes", "-a", "-F", format), "\n")
	sort.Strings(rows)
	return rows
}

// viewSep separates -F fields. tmux replaces control characters such as TAB
// with "_" in format output, so a printable, format-inert marker is used.
const viewSep = "<|>"

func sequence(s *server) int {
	s.e.t.Helper()
	raw := s.mustTmux("show-options", "-gqv", "@moox_save_seq")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		s.e.t.Fatalf("@moox_save_seq is not an integer: %q", raw)
	}
	return n
}

func containsHook(hooks string, slot int) bool {
	return regexp.MustCompile(fmt.Sprintf(`(?m)^[^\n]*\[%d\]`, slot)).MatchString(hooks)
}

// ---------------------------------------------------------------------------
// The run-wide registry and TestMain's final sweep. A server that outlives its
// test (a cleanup that could not end it, an interrupted run, a timeout) is
// ended here by its own socket, and the run fails if any was left.

type runServer struct {
	nonce, base, realTmux, path string
}

var (
	runMu      sync.Mutex
	runServers = map[*server]*runServer{}
	runKept    []string // sandbox roots kept because a server there still answered
)

func track(s *server) {
	runMu.Lock()
	defer runMu.Unlock()
	runServers[s] = &runServer{nonce: s.e.nonce, base: s.e.base, realTmux: s.e.realTmux, path: s.socketPath()}
}

func retrack(s *server) {
	runMu.Lock()
	defer runMu.Unlock()
	if r := runServers[s]; r != nil && s.path != "" {
		r.path = s.path
	}
}

func keepForSweep(base string) {
	runMu.Lock()
	defer runMu.Unlock()
	runKept = append(runKept, base)
}

// ownTmux runs realTmux -u -S path args, only for an existing socket inside
// base (one of this run's sandbox roots), with a scrubbed environment and a
// deadline. No caller's verb starts a server on a dead socket.
func ownTmux(realTmux, base, path string, args ...string) (string, bool) {
	if realTmux == "" || path == "" || !strings.HasPrefix(path, base+string(filepath.Separator)) {
		return "", false
	}
	if _, err := os.Lstat(path); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, realTmux, append([]string{"-u", "-S", path}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + base}
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err == nil
}

// sweepRun ends every server of this run that still answers on its socket
// (its sessions, by id; the server exits when empty) and every leftover
// process whose command line carries one of this run's nonces. It returns
// what was left.
func sweepRun() (left []string) {
	runMu.Lock()
	servers := make([]runServer, 0, len(runServers))
	for _, r := range runServers {
		servers = append(servers, *r)
	}
	kept := append([]string(nil), runKept...)
	runMu.Unlock()

	nonces := map[string]bool{}
	for _, r := range servers {
		nonces[r.nonce] = true
		out, ok := ownTmux(r.realTmux, r.base, r.path, "list-sessions", "-F", "#{session_id}")
		if !ok {
			continue
		}
		left = append(left, "tmux server "+r.path)
		for deadline := time.Now().Add(15 * time.Second); ok && time.Now().Before(deadline); {
			for _, id := range strings.Fields(out) {
				ownTmux(r.realTmux, r.base, r.path, "kill-session", "-t", id)
			}
			<-time.After(50 * time.Millisecond)
			out, ok = ownTmux(r.realTmux, r.base, r.path, "list-sessions", "-F", "#{session_id}")
		}
		if ok {
			left = append(left, "tmux server "+r.path+" could not be ended")
		}
	}

	// Processes of this run: the nonce is random per sandbox, so a command
	// line carrying "moox-bb-<nonce>" was started by this run.
	ours := func() map[int]string {
		found := map[int]string{}
		out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
		if err != nil {
			return found
		}
		for _, row := range strings.Split(string(out), "\n") {
			fields := strings.Fields(row)
			if len(fields) < 2 {
				continue
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil || pid == os.Getpid() {
				continue
			}
			for nonce := range nonces {
				if strings.Contains(row, "moox-bb-"+nonce) {
					found[pid] = strings.Join(fields[1:], " ")
				}
			}
		}
		return found
	}
	procs := ours()
	for until := time.Now().Add(5 * time.Second); len(procs) != 0 && time.Now().Before(until); procs = ours() {
		<-time.After(100 * time.Millisecond)
	}
	for pid, command := range procs {
		left = append(left, fmt.Sprintf("process %d: %s", pid, command))
		if filepath.Base(strings.Fields(command)[0]) != "tmux" { // servers end by socket, above
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}

	for _, base := range kept {
		_ = os.RemoveAll(base)
	}
	return left
}

func TestMain(m *testing.M) {
	flag.Parse()
	// An interrupted run never reaches the cleanups: end this run's servers.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-interrupted
		fmt.Fprintf(os.Stderr, "blackbox: %v: ending this run's tmux servers\n", sig)
		for _, l := range sweepRun() {
			fmt.Fprintf(os.Stderr, "blackbox: left behind: %s\n", l)
		}
		os.Exit(1)
	}()
	// Neither does one that hits -test.timeout (its panic skips every
	// cleanup): sweep shortly before it.
	if f := flag.Lookup("test.timeout"); f != nil {
		if g, ok := f.Value.(flag.Getter); ok {
			if d, ok := g.Get().(time.Duration); ok && d > time.Minute {
				time.AfterFunc(d-30*time.Second, func() {
					fmt.Fprintln(os.Stderr, "blackbox: -test.timeout is near: ending this run's tmux servers")
					sweepRun()
				})
			}
		}
	}
	code := m.Run()
	if left := sweepRun(); len(left) != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: the black-box run left %d server(s)/process(es) behind (now ended):\n", len(left))
		for _, l := range left {
			fmt.Fprintf(os.Stderr, "  %s\n", l)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
