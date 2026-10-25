// Package doctor is `moox doctor` (architecture §8.4, D9): every check is a
// function of an Env returning Results, and the three fixes (stale per-socket
// dirs, legacy hooks/keys/options, an unparsable @moox-restoring) change
// exactly what the checks reported. Checks never write; fixes write through
// statedir (files) or the server's own socket (tmux).
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Level orders results.
type Level int

const (
	OK Level = iota
	Info
	Warn
	Fail
)

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Info:
		return "info"
	case Warn:
		return "warn"
	}
	return "FAIL"
}

// Result is one check's finding.
type Result struct {
	Check   string
	Level   Level
	Message string
	// Fix is the command that repairs it ("" if none).
	Fix string
	// Server is the socket the result is about ("" for machine checks).
	Server string
}

// Line is the one-line form.
func (r Result) Line() string {
	s := fmt.Sprintf("[%s] %s: %s", r.Level, r.Check, r.Message)
	if r.Server != "" {
		s = fmt.Sprintf("[%s] %s (%s): %s", r.Level, r.Check, filepath.Base(r.Server), r.Message)
	}
	if r.Fix != "" {
		s += " — fix: " + r.Fix
	}
	return s
}

// StaleAge is D9's bound: a per-socket dir older than this, whose socket is
// not live, is stale.
const StaleAge = 14 * 24 * time.Hour

// Env is what the checks read.
type Env struct {
	Getenv   func(string) string
	Environ  []string
	LookPath func(string) (string, error)
	// Root is the share root ("" if it could not be found; RootErr says why).
	Root        string
	RootErr     error
	MooxVersion string
	Tmux        tmuxtarget.Target // Bin is used for -V; Socket may be ""
	// Servers are the servers to check (none: machine checks only).
	Servers []tmuxtarget.Target
	// LiveSockets are the socket files found in tmux's dir, for staleness.
	LiveSockets []string
	// Mode is the manifest's mode ("" = setup has not run).
	Mode string
	// Hook reports the shell hook (setup's one-level detection).
	Hook func() (ok bool, msg string)
	Now  func() time.Time
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) roots() (sockets.Roots, error) { return sockets.RootsFrom(envFunc(e.Getenv)) }

type envFunc func(string) string

func (f envFunc) Getenv(k string) string { return f(k) }

// Run runs every check.
func Run(ctx context.Context, e Env) []Result {
	var out []Result
	out = append(out, checkTmux(ctx, e)...)
	out = append(out, checkRoot(e)...)
	out = append(out, checkConfig(e)...)
	out = append(out, checkHook(e)...)
	out = append(out, checkTools(ctx, e)...)
	out = append(out, checkStale(e)...)
	for _, t := range e.Servers {
		out = append(out, checkServer(ctx, e, t)...)
	}
	return out
}

func checkTmux(ctx context.Context, e Env) []Result {
	if e.Tmux.Bin == "" {
		return []Result{{Check: "tmux", Level: Fail, Message: "tmux not found on PATH", Fix: "brew install tmux"}}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := e.Tmux.Version(cctx)
	switch {
	case err != nil:
		return []Result{{Check: "tmux", Level: Fail, Message: err.Error()}}
	case !v.AtLeast(3, 6):
		return []Result{{Check: "tmux", Level: Fail, Message: fmt.Sprintf("%s at %s is older than 3.6", v, e.Tmux.Bin),
			Fix: "brew upgrade tmux"}}
	}
	return []Result{{Check: "tmux", Level: OK, Message: fmt.Sprintf("%s at %s", v, e.Tmux.Bin)}}
}

func checkRoot(e Env) []Result {
	if e.Root == "" {
		msg := "the moox share root was not found"
		if e.RootErr != nil {
			msg += ": " + e.RootErr.Error()
		}
		return []Result{{Check: "root", Level: Fail, Message: msg, Fix: "reinstall moox"}}
	}
	if _, err := os.Stat(filepath.Join(e.Root, "moox.conf")); err != nil {
		return []Result{{Check: "root", Level: Fail, Message: e.Root + " has no moox.conf"}}
	}
	b, err := os.ReadFile(filepath.Join(e.Root, "VERSION"))
	switch {
	case err != nil:
		return []Result{{Check: "root", Level: Info, Message: e.Root + " (no VERSION stamp: a dev tree)"}}
	case strings.TrimSpace(strings.TrimPrefix(string(b), "v")) != e.MooxVersion:
		return []Result{{Check: "root", Level: Warn, Message: fmt.Sprintf("%s is moox %s but this binary is %s",
			e.Root, strings.TrimSpace(string(b)), e.MooxVersion), Fix: "reinstall moox (brew reinstall moox)"}}
	}
	return []Result{{Check: "root", Level: OK, Message: e.Root + " (" + e.MooxVersion + ")"}}
}

func checkConfig(e Env) []Result {
	r, err := e.roots()
	if err != nil {
		return []Result{{Check: "config", Level: Fail, Message: err.Error()}}
	}
	conf := filepath.Join(r.Home, ".tmux.conf")
	switch e.Mode {
	case "":
		return []Result{{Check: "config", Level: Warn, Message: "moox setup has not run here", Fix: "moox setup"}}
	case "separate":
		return []Result{{Check: "config", Level: OK, Message: "separate mode: your tmux config stays; moox runs on -L moox"}}
	}
	b, err := os.ReadFile(conf)
	if err != nil {
		return []Result{{Check: "config", Level: Warn, Message: conf + " is missing: plain tmux does not load moox",
			Fix: "moox setup"}}
	}
	if !strings.Contains(string(b), "moox.conf") {
		return []Result{{Check: "config", Level: Warn, Message: conf + " does not source moox.conf", Fix: "moox setup --mode replace"}}
	}
	return []Result{{Check: "config", Level: OK, Message: conf + " sources moox.conf"}}
}

func checkHook(e Env) []Result {
	if e.Hook == nil {
		return nil
	}
	ok, msg := e.Hook()
	if ok {
		return []Result{{Check: "shell-hook", Level: OK, Message: msg}}
	}
	return []Result{{Check: "shell-hook", Level: Warn, Message: msg, Fix: "moox setup"}}
}

func checkTools(ctx context.Context, e Env) []Result {
	var out []Result
	look := e.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if _, err := look("tmux-fingers"); err != nil {
		out = append(out, Result{Check: "tmux-fingers", Level: Info,
			Message: "not on PATH: prefix F (hint mode) is off", Fix: "brew install tmux-fingers"})
	} else {
		out = append(out, Result{Check: "tmux-fingers", Level: OK, Message: "on PATH"})
	}
	if _, err := look("jq"); err != nil {
		out = append(out, Result{Check: "jq", Level: Warn,
			Message: "not on PATH: the Claude SessionStart hook reads the session id with jq and does nothing without it",
			Fix:     "brew install jq"})
	} else {
		out = append(out, Result{Check: "jq", Level: OK, Message: "on PATH"})
	}
	out = append(out, checkTT(ctx, e, look)...)
	return out
}

// ttVersion runs `<tt> --version`; ours prints "tt <semver> (moox)".
func ttVersion(ctx context.Context, e Env, path string) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, "--version")
	cmd.Env = e.Environ
	// No controlling terminal: an old tt that does not know --version would
	// start its TUI on /dev/tty; in its own session that open fails.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func checkTT(ctx context.Context, e Env, look func(string) (string, error)) []Result {
	var out []Result
	p, err := look("tt")
	if err != nil {
		return []Result{{Check: "tt", Level: Warn, Message: "tt is not on PATH (the sidebar and `prefix g` need it)",
			Fix: "put moox's bin dir on PATH"}}
	}
	v := ttVersion(ctx, e, p)
	switch {
	case !strings.Contains(v, "(moox)"):
		out = append(out, Result{Check: "tt", Level: Warn, Message: fmt.Sprintf("%s is not moox's tt (it says %q; Tarantool's tt?)", p, firstLine(v)),
			Fix: "put moox's bin dir before it on PATH (brew: `brew unlink tarantool` or reorder PATH)"})
	case !strings.Contains(v, " "+e.MooxVersion+" "):
		out = append(out, Result{Check: "tt", Level: Warn, Message: fmt.Sprintf("%s is %q but moox is %s (VERSION-SKEW)", p, v, e.MooxVersion),
			Fix: "remove the old tt or reorder PATH"})
	default:
		out = append(out, Result{Check: "tt", Level: OK, Message: p + " (" + v + ")"})
	}
	// An old hand-built ~/.local/bin/tt (the pre-moox `task tt`) behind or in
	// front of the packaged one.
	if r, err := e.roots(); err == nil {
		local := filepath.Join(r.Home, ".local", "bin", "tt")
		if fi, err := os.Stat(local); err == nil && fi.Mode().IsRegular() && local != p {
			lv := ttVersion(ctx, e, local)
			if !strings.Contains(lv, " "+e.MooxVersion+" ") {
				out = append(out, Result{Check: "tt", Level: Warn, Message: fmt.Sprintf("%s is a different tt build (%q)", local, firstLine(lv)),
					Fix: "remove " + local})
			}
		}
	}
	return out
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// ---------------------------------------------------------------------------
// stale per-socket state (D9)
// ---------------------------------------------------------------------------

// Stale is one stale per-socket directory.
type Stale struct {
	Parent string // XS/moox/sockets or XS/tt/sockets
	Key    string
	Newest time.Time
}

// Path is the directory.
func (s Stale) Path() string { return filepath.Join(s.Parent, s.Key) }

// FindStale lists XS/moox/sockets/<k> and XS/tt/sockets/<k> whose socket is
// not live and whose newest entry is older than StaleAge. "default" and the
// legacy paths are never candidates.
func FindStale(e Env) []Stale {
	r, err := e.roots()
	if err != nil {
		return nil
	}
	live := map[string]bool{}
	for _, s := range e.LiveSockets {
		live[sockets.Key(s)] = true
	}
	for _, t := range e.Servers {
		live[sockets.Key(t.Socket)] = true
	}
	var out []Stale
	mooxSockets := filepath.Join(r.State, "moox", "sockets")
	for _, parent := range []string{mooxSockets, filepath.Join(r.State, "tt", "sockets")} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, de := range entries {
			k := de.Name()
			if k == sockets.LegacyKey || !de.IsDir() || live[k] {
				continue
			}
			// A key that holds a saved workspace is never debris, however
			// long its server has been down (a reboot, two weeks away): its
			// moox-last and pins are the only copy (G2-H9).
			if holdsWorkspace(filepath.Join(mooxSockets, k)) {
				continue
			}
			newest := newestMtime(filepath.Join(parent, k), 0)
			if e.now().Sub(newest) > StaleAge {
				out = append(out, Stale{Parent: parent, Key: k, Newest: newest})
			}
		}
	}
	return out
}

// holdsWorkspace: XS/moox/sockets/<k> has a published snapshot (a
// moox-*.json or moox-last under snapshots/) or a non-empty pins file.
func holdsWorkspace(keyDir string) bool {
	if fi, err := os.Stat(filepath.Join(keyDir, "pins")); err == nil && fi.Size() > 0 {
		return true
	}
	entries, err := os.ReadDir(filepath.Join(keyDir, "snapshots"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if n == "moox-last" || strings.HasPrefix(n, "moox-") && strings.HasSuffix(n, ".json") {
			return true
		}
	}
	return false
}

func newestMtime(p string, depth int) time.Time {
	fi, err := os.Lstat(p)
	if err != nil {
		return time.Time{}
	}
	n := fi.ModTime()
	if !fi.IsDir() || depth > 8 {
		return n
	}
	entries, _ := os.ReadDir(p)
	for _, de := range entries {
		if t := newestMtime(filepath.Join(p, de.Name()), depth+1); t.After(n) {
			n = t
		}
	}
	return n
}

func checkStale(e Env) []Result {
	st := FindStale(e)
	if len(st) == 0 {
		return []Result{{Check: "stale-state", Level: OK, Message: "no stale per-socket state"}}
	}
	var out []Result
	for _, s := range st {
		out = append(out, Result{Check: "stale-state", Level: Warn,
			Message: fmt.Sprintf("%s: no live socket, untouched since %s", s.Path(), s.Newest.Format("2006-01-02")),
			Fix:     "moox doctor --fix stale"})
	}
	return out
}

// FixStale removes the stale dirs through statedir.RemoveTree.
func FixStale(e Env, dry bool) ([]string, error) {
	r, err := e.roots()
	if err != nil {
		return nil, err
	}
	claude := filepath.Join(r.Home, ".claude")
	var report []string
	for _, s := range FindStale(e) {
		if dry {
			report = append(report, "would remove "+s.Path())
			continue
		}
		d, err := statedir.OpenExisting(s.Parent, claude)
		if err != nil {
			return report, err
		}
		err = d.RemoveTree(s.Key)
		_ = d.Close()
		if err != nil {
			return report, err
		}
		report = append(report, "removed "+s.Path())
	}
	return report, nil
}

// ---------------------------------------------------------------------------
// per-server checks
// ---------------------------------------------------------------------------

func run(ctx context.Context, t tmuxtarget.Target, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := t.Run(cctx, args...)
	return string(out), err
}

const sep = "\x1f"

// serverFields is ONE display-message.
var serverFields = []string{"@moox-active", "@moox-restoring", "@moox-boot-result", "@moox-sockets", "@moox_status",
	"@moox-warning", "@claude-continuity-boot-warning", "version", "pid"}

func readFields(ctx context.Context, t tmuxtarget.Target) (map[string]string, error) {
	var b strings.Builder
	for i, f := range serverFields {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString("#{" + f + "}")
	}
	out, err := run(ctx, t, "display-message", "-p", b.String())
	if err != nil {
		return nil, err
	}
	vals := strings.SplitN(strings.TrimRight(out, "\n"), sep, len(serverFields))
	m := map[string]string{}
	for i, f := range serverFields {
		if i < len(vals) {
			m[f] = vals[i]
		}
	}
	return m, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// parseRestoring: "<kind>:<pid>:<epoch>"; valid reports whether it parses.
func parseRestoring(v string) (kind string, pid int, valid bool) {
	f := strings.Split(v, ":")
	if len(f) != 3 {
		return "", 0, false
	}
	p, err1 := strconv.Atoi(f[1])
	_, err2 := strconv.ParseInt(f[2], 10, 64)
	if err1 != nil || err2 != nil || (f[0] != "boot" && f[0] != "restore" && f[0] != "shutdown") {
		return "", 0, false
	}
	return f[0], p, true
}

func checkServer(ctx context.Context, e Env, t tmuxtarget.Target) []Result {
	srv := t.Socket
	res := func(check string, l Level, msg, fix string) Result {
		return Result{Check: check, Level: l, Message: msg, Fix: fix, Server: srv}
	}
	var out []Result
	if n := len(srv); n >= 104 {
		out = append(out, res("socket", Fail, fmt.Sprintf("the socket path is %d bytes; unix sockets allow 103 on macOS", n), ""))
	}
	f, err := readFields(ctx, t)
	if err != nil {
		return append(out, res("server", Info, "not running", ""))
	}
	out = append(out, res("server", OK, "tmux "+f["version"]+", pid "+f["pid"], ""))
	for _, lerr := range sockets.Lint(f["@moox-sockets"]) {
		out = append(out, res("moox-sockets", Warn, lerr.Error(), "edit @moox-sockets in ~/.config/moox/local.conf"))
	}
	paths, perr := sockets.For(t.Socket, envFunc(e.Getenv))
	switch f["@moox-active"] {
	case "":
		out = append(out, res("gate", Fail, "@moox-active is unset: the gate (`tt serve --ensure`) never ran on this server",
			"check that "+filepath.Join("<@moox-bin>", "tt")+" exists; then prefix r"))
	case "0":
		out = append(out, res("gate", Info, "not matched by @moox-sockets ("+nonEmpty(f["@moox-sockets"], "default")+"): no continuity here", ""))
	case "1":
		out = append(out, res("gate", OK, "matched; continuity active", ""))
	default:
		out = append(out, res("gate", Warn, "@moox-active is "+strconv.Quote(f["@moox-active"]), ""))
	}
	if perr == nil {
		_, merr := os.Stat(paths.Marker)
		switch {
		case f["@moox-active"] == "1" && merr != nil:
			out = append(out, res("marker", Warn, "@moox-active is 1 but "+paths.Marker+" is missing", "prefix r"))
		case f["@moox-active"] == "0" && merr == nil:
			out = append(out, res("marker", Warn, "@moox-active is 0 but "+paths.Marker+" exists", "prefix r"))
		}
	}
	// @moox-restoring.
	if v := f["@moox-restoring"]; v != "" {
		kind, pid, valid := parseRestoring(v)
		switch {
		case !valid:
			out = append(out, res("restoring", Fail, "@moox-restoring "+strconv.Quote(v)+" does not parse: every save is refused",
				"moox doctor --fix restoring"))
		case !pidAlive(pid):
			out = append(out, res("restoring", Warn, fmt.Sprintf("@moox-restoring names a %s by pid %d, which is gone", kind, pid),
				"moox doctor --fix restoring"))
		default:
			out = append(out, res("restoring", Info, fmt.Sprintf("a %s is running (pid %d)", kind, pid), ""))
		}
	}
	switch br := f["@moox-boot-result"]; {
	case br == "SKIPPED:no-bootstrap":
		out = append(out, res("boot", Warn, "the boot restore found no bootstrap session in time (SKIPPED:no-bootstrap)",
			"moox restore"))
	case br == "INCOMPLETE":
		out = append(out, res("boot", Warn, "the boot restore was INCOMPLETE", "moox snapshot show"))
	case br != "":
		out = append(out, res("boot", OK, "boot result "+br, ""))
	}
	if w := f["@claude-continuity-boot-warning"]; w != "" {
		out = append(out, res("boot", Warn, w, ""))
	}
	if perr != nil {
		return out
	}
	if f["@moox-active"] == "1" {
		out = append(out, checkDaemon(e, paths, srv)...)
		out = append(out, checkSnapshots(e, t, paths, f["@moox-restoring"], srv)...)
	}
	out = append(out, checkLegacy(ctx, e, t, srv)...)
	return out
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func checkDaemon(e Env, p sockets.Paths, srv string) []Result {
	r := func(l Level, msg, fix string) Result {
		return Result{Check: "daemon", Level: l, Message: msg, Fix: fix, Server: srv}
	}
	b, err := os.ReadFile(filepath.Join(p.State, "saver.state"))
	if err != nil {
		return []Result{r(Fail, "no saver.state: the daemon is not running", "prefix r (or `tt serve --ensure`)")}
	}
	f := strings.Fields(string(b))
	pid := 0
	if len(f) > 0 {
		pid, _ = strconv.Atoi(f[0])
	}
	var out []Result
	if !pidAlive(pid) {
		out = append(out, r(Fail, fmt.Sprintf("the daemon (pid %d) is gone", pid), "prefix r"))
	} else {
		state := ""
		if len(f) > 1 {
			state = f[1]
		}
		out = append(out, r(OK, fmt.Sprintf("pid %d, state %s", pid, state), ""))
	}
	// Waiter health and version skew from the tail of daemon.log.
	if log, err := os.ReadFile(filepath.Join(p.State, "daemon.log")); err == nil {
		tail := string(log)
		if len(tail) > 64<<10 {
			tail = tail[len(tail)-64<<10:]
		}
		for _, w := range []string{"WAITER-GAVE-UP", "VERSION-SKEW", "WAITER-RESPAWN"} {
			if i := strings.LastIndex(tail, w); i >= 0 {
				line := tail[i:]
				line, _, _ = strings.Cut(line, "\n")
				lvl := Warn
				if w == "WAITER-RESPAWN" {
					lvl = Info
				}
				out = append(out, r(lvl, "daemon.log: "+line, ""))
			}
		}
	}
	return out
}

func checkSnapshots(e Env, t tmuxtarget.Target, p sockets.Paths, restoring, srv string) []Result {
	r := func(check string, l Level, msg, fix string) Result {
		return Result{Check: check, Level: l, Message: msg, Fix: fix, Server: srv}
	}
	eng, err := engine.New(engine.Options{Target: t, Env: envFunc(e.Getenv)})
	if err != nil {
		return []Result{r("snapshot", Warn, err.Error(), "")}
	}
	info, err := eng.Inspect()
	if err != nil {
		return []Result{r("snapshot", Warn, err.Error(), "")}
	}
	var out []Result
	switch {
	case info.LastErr != nil:
		out = append(out, r("snapshot", Info, "no valid moox snapshot yet in "+info.Dir, "prefix C-s"))
	case info.Healed:
		out = append(out, r("snapshot", Warn, fmt.Sprintf("moox-last (%q) does not name a valid snapshot; newest valid is %s", info.Pointer, info.Last), ""))
	default:
		age := "age unknown"
		if d, ok := engine.SnapshotAge(info.LastSnapshot, e.now()); ok {
			age = d.Round(time.Second).String() + " old"
		}
		out = append(out, r("snapshot", OK, fmt.Sprintf("%s (%d windows, %s)", info.Last, len(info.LastSnapshot.Windows), age), ""))
	}
	for i, pin := range info.Pins {
		out = append(out, r("pin", Warn, "pinned after a non-PASS restore: "+info.PinPaths[i],
			"moox restart --snapshot "+info.PinPaths[i]+"  (or: moox snapshot unpin "+pin+")"))
	}
	if info.RestoringFrom != "" && restoring == "" {
		out = append(out, r("restoring-from", Warn, "a leftover restoring-from names "+info.RestoringFrom+" with no restore running",
			"it becomes a pin at the next cold start; `moox snapshot show`"))
	}
	return out
}

// ---------------------------------------------------------------------------
// legacy hooks, keys and options (PR-M9; the D15 cutover tool)
// ---------------------------------------------------------------------------

// Legacy is what checkLegacy found on one server.
type Legacy struct {
	Hooks   []string    // "name[i]"
	HookCmd []string    // their commands
	Keys    [][2]string // {table, key}
	KeyCmd  []string
	Options []string
}

// Empty reports whether nothing legacy is left.
func (l Legacy) Empty() bool { return len(l.Hooks)+len(l.Keys)+len(l.Options) == 0 }

// legacyText is a legacy HOOK entry (§8.4): its text references ~/.tmux/,
// tmux-resurrect or continuum (the old dotfiles' theme, cc reconcile and
// tt-obs hooks all ran from ~/.tmux/).
func legacyText(s, home string) bool {
	if legacyKeyText(s) || strings.Contains(s, "~/.tmux/") {
		return true
	}
	return home != "" && strings.Contains(s, filepath.Join(home, ".tmux")+"/")
}

// legacyKeyText is a legacy KEY binding (§8.4): only one whose command
// references tmux-resurrect or continuum (e.g. prefix C-r). A binding that
// merely runs a script under ~/.tmux/ is the user's own and is never
// unbound (G2-H13).
func legacyKeyText(s string) bool {
	return strings.Contains(s, "tmux-resurrect") || strings.Contains(s, "continuum")
}

// FindLegacy reads show-hooks -g, list-keys and show-options -g.
func FindLegacy(ctx context.Context, e Env, t tmuxtarget.Target) (Legacy, error) {
	home := ""
	if e.Getenv != nil {
		home = e.Getenv("HOME")
	}
	var l Legacy
	hooks, err := run(ctx, t, "show-hooks", "-g")
	if err != nil {
		return l, err
	}
	for _, line := range strings.Split(hooks, "\n") {
		name, cmd, ok := strings.Cut(line, " ")
		if !ok || !strings.HasSuffix(name, "]") || !legacyText(cmd, home) {
			continue
		}
		l.Hooks = append(l.Hooks, name)
		l.HookCmd = append(l.HookCmd, cmd)
	}
	keys, err := run(ctx, t, "list-keys", "-F", "#{key_table}"+sep+"#{key_string}"+sep+"#{key_command}")
	if err == nil {
		for _, line := range strings.Split(keys, "\n") {
			f := strings.SplitN(line, sep, 3)
			if len(f) == 3 && legacyKeyText(f[2]) {
				l.Keys = append(l.Keys, [2]string{f[0], f[1]})
				l.KeyCmd = append(l.KeyCmd, f[2])
			}
		}
	} else if keys, err = run(ctx, t, "list-keys"); err == nil {
		// tmux without list-keys -F: "bind-key [-r] -T <table> <key> <command>".
		for _, line := range strings.Split(keys, "\n") {
			f := strings.Fields(line)
			i := 1
			for i < len(f) && f[i] != "-T" {
				i++
			}
			if i+3 > len(f) || !legacyKeyText(strings.Join(f[i+3:], " ")) {
				continue
			}
			l.Keys = append(l.Keys, [2]string{f[i+1], f[i+2]})
			l.KeyCmd = append(l.KeyCmd, strings.Join(f[i+3:], " "))
		}
	}
	opts, err := run(ctx, t, "show-options", "-g")
	if err != nil {
		return l, err
	}
	for _, line := range strings.Split(opts, "\n") {
		name, _, _ := strings.Cut(line, " ")
		if strings.HasPrefix(name, "@resurrect-") || strings.HasPrefix(name, "@continuum-") {
			l.Options = append(l.Options, name)
		}
	}
	sort.Strings(l.Options)
	return l, nil
}

func checkLegacy(ctx context.Context, e Env, t tmuxtarget.Target, srv string) []Result {
	l, err := FindLegacy(ctx, e, t)
	if err != nil {
		return []Result{{Check: "legacy", Level: Warn, Message: err.Error(), Server: srv}}
	}
	if l.Empty() {
		return []Result{{Check: "legacy", Level: OK, Message: "no resurrect/continuum hooks, keys or options", Server: srv}}
	}
	var out []Result
	fix := "moox doctor --fix legacy-hooks -S " + srv
	for i, h := range l.Hooks {
		out = append(out, Result{Check: "legacy-hook", Level: Warn, Message: h + " " + l.HookCmd[i], Fix: fix, Server: srv})
	}
	for i, k := range l.Keys {
		out = append(out, Result{Check: "legacy-key", Level: Warn, Message: "-T " + k[0] + " " + k[1] + " " + l.KeyCmd[i], Fix: fix, Server: srv})
	}
	for _, o := range l.Options {
		out = append(out, Result{Check: "legacy-option", Level: Warn, Message: o, Fix: fix, Server: srv})
	}
	return out
}

// FixLegacy unsets exactly what FindLegacy found: hook entries by index
// (`set-hook -gu 'name[i]'`), the key bindings, and the options.
func FixLegacy(ctx context.Context, e Env, t tmuxtarget.Target, dry bool) ([]string, error) {
	l, err := FindLegacy(ctx, e, t)
	if err != nil {
		return nil, err
	}
	var cmds [][]string
	var report []string
	for _, h := range l.Hooks {
		cmds = append(cmds, []string{"set-hook", "-gu", h})
		report = append(report, "unset hook "+h)
	}
	for _, k := range l.Keys {
		cmds = append(cmds, []string{"unbind-key", "-T", k[0], k[1]})
		report = append(report, "unbind -T "+k[0]+" "+k[1])
	}
	for _, o := range l.Options {
		cmds = append(cmds, []string{"set-option", "-gu", o})
		report = append(report, "unset option "+o)
	}
	if dry || len(cmds) == 0 {
		for i := range report {
			report[i] = "would " + report[i]
		}
		return report, nil
	}
	// One command per exec: a binding whose key no longer parses must not
	// stop the rest.
	var errs []error
	for i, c := range cmds {
		if _, err := run(ctx, t, c...); err != nil {
			errs = append(errs, err)
			report[i] = "FAILED: " + report[i] + ": " + err.Error()
		}
	}
	return report, errors.Join(errs...)
}

// FixRestoring clears an unparsable or dead @moox-restoring (conditionally:
// only while it still holds the value read).
func FixRestoring(ctx context.Context, t tmuxtarget.Target, dry bool) ([]string, error) {
	out, err := run(ctx, t, "show-options", "-gqv", "@moox-restoring")
	if err != nil {
		return nil, err
	}
	v := strings.TrimRight(out, "\n")
	if v == "" {
		return []string{"@moox-restoring is not set"}, nil
	}
	if _, pid, valid := parseRestoring(v); valid && pidAlive(pid) {
		return []string{"@moox-restoring " + strconv.Quote(v) + " names a live process; left alone"}, nil
	}
	if dry {
		return []string{"would clear @moox-restoring " + strconv.Quote(v)}, nil
	}
	args := engine.ClearRestoringArgs(v)
	if _, _, ok := engine.RestoringOwner(v); !ok {
		// An unparsable value cannot be compared inside a format without
		// letting its `}`/`#(` reach the expander (G1-M1); this explicit
		// --fix clears it outright.
		args = []string{"set-option", "-gu", "@moox-restoring"}
	}
	if _, err := run(ctx, t, args...); err != nil {
		return nil, err
	}
	return []string{"cleared @moox-restoring " + strconv.Quote(v)}, nil
}

// LiveSockets lists the socket files in tmux's own directory for this user
// (LabelPath's directory).
func LiveSockets(env tmuxtarget.Env) []string {
	p, err := tmuxtarget.LabelPath(env, "x")
	if err != nil {
		return nil
	}
	dir := filepath.Dir(p)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range entries {
		if de.Type()&fs.ModeSocket != 0 {
			out = append(out, filepath.Join(dir, de.Name()))
		}
	}
	return out
}
