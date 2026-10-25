package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// The restore fake: a tmux model good enough for the commands Restore,
// BootGate and ManualPreconditions issue. A chain stops at the first failing
// command (E5), and each command's -P -F output is printed in order.

type rPane struct {
	id, win, cmd string
	pid          int
	env          []string
}

type rWin struct {
	id, sess string
	idx      int
}

type rSess struct {
	id, name string
}

type rfake struct {
	mu       sync.Mutex
	sessions []*rSess
	wins     []*rWin
	panes    []*rPane
	n        int
	opts     map[string]string
	globEnv  []string
	version  string
	start    int64
	fail     func(cmd []string) bool
	onCmd    func(cmd []string)
	log      [][]string
}

func newRFake() *rfake {
	return &rfake{opts: map[string]string{"base-index": "0"}, version: "3.8", start: 1000}
}

func (f *rfake) id(prefix string) string { f.n++; return prefix + strconv.Itoa(f.n) }

// addSession creates a pre-existing session with one window and pane.
func (f *rfake) addSession(name string) *rSess {
	s := &rSess{id: f.id("$"), name: name}
	f.sessions = append(f.sessions, s)
	w := &rWin{id: f.id("@"), sess: s.id}
	f.wins = append(f.wins, w)
	f.panes = append(f.panes, &rPane{id: f.id("%"), win: w.id, cmd: "zsh", pid: 9000 + f.n})
	return s
}

var fmtRef = regexp.MustCompile(`#\{([^}]*)\}`)

func (f *rfake) expand(format string, vals map[string]string) string {
	return fmtRef.ReplaceAllStringFunc(format, func(m string) string {
		k := m[2 : len(m)-1]
		if v, ok := vals[k]; ok {
			return v
		}
		return f.opts[k]
	})
}

func rflag(cmd []string, name string) string {
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == name {
			return cmd[i+1]
		}
	}
	return ""
}

func (f *rfake) sess(id string) *rSess {
	for _, s := range f.sessions {
		if s.id == id || s.name == id {
			return s
		}
	}
	return nil
}

func (f *rfake) winOf(sess string) []*rWin {
	var out []*rWin
	for _, w := range f.wins {
		if w.sess == sess {
			out = append(out, w)
		}
	}
	return out
}

func (f *rfake) panesOf(win string) []*rPane {
	var out []*rPane
	for _, p := range f.panes {
		if p.win == win {
			out = append(out, p)
		}
	}
	return out
}

func (f *rfake) paneVals(p *rPane) map[string]string {
	var w *rWin
	for _, x := range f.wins {
		if x.id == p.win {
			w = x
		}
	}
	return map[string]string{"pane_id": p.id, "pane_pid": strconv.Itoa(p.pid), "pane_dead": "0",
		"pane_current_command": p.cmd, "window_id": w.id, "session_id": w.sess}
}

func (f *rfake) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var cmds [][]string
	cur := []string{}
	for _, a := range args {
		if a == ";" {
			cmds = append(cmds, cur)
			cur = []string{}
			continue
		}
		cur = append(cur, strings.TrimSuffix(a, `\;`)+map[bool]string{true: ";", false: ""}[strings.HasSuffix(a, `\;`)])
	}
	cmds = append(cmds, cur)
	var out strings.Builder
	for _, c := range cmds {
		if ctx.Err() != nil {
			return []byte(out.String()), ctx.Err()
		}
		f.log = append(f.log, c)
		if f.onCmd != nil {
			f.onCmd(c)
		}
		if f.fail != nil && f.fail(c) {
			return []byte(out.String()), errors.New("fake: injected failure: " + strings.Join(c, " "))
		}
		s, err := f.one(c)
		out.WriteString(s)
		if err != nil {
			return []byte(out.String()), err
		}
	}
	return []byte(out.String()), nil
}

func (f *rfake) one(c []string) (string, error) {
	print := func(vals map[string]string) string {
		if fm := rflag(c, "-F"); fm != "" && contains(c, "-P") {
			return f.expand(fm, vals) + "\n"
		}
		return ""
	}
	switch c[0] {
	case "display-message":
		vals := map[string]string{"version": f.version, "pid": "4242", "start_time": strconv.FormatInt(f.start, 10),
			"server_sessions": strconv.Itoa(len(f.sessions))}
		if t := rflag(c, "-t"); t != "" {
			for _, p := range f.panes {
				if p.id == t {
					vals = f.paneVals(p)
				}
			}
		}
		return f.expand(c[len(c)-1], vals) + "\n", nil
	case "show-environment":
		return strings.Join(f.globEnv, "\n") + "\n", nil
	case "list-sessions":
		var b strings.Builder
		for _, s := range f.sessions {
			b.WriteString(f.expand(rflag(c, "-F"), map[string]string{"session_id": s.id, "session_name": s.name,
				"session_windows": strconv.Itoa(len(f.winOf(s.id)))}) + "\n")
		}
		return b.String(), nil
	case "list-panes":
		var b strings.Builder
		t := rflag(c, "-t")
		for _, p := range f.panes {
			v := f.paneVals(p)
			if t != "" && v["session_id"] != t {
				continue
			}
			b.WriteString(f.expand(rflag(c, "-F"), v) + "\n")
		}
		return b.String(), nil
	case "list-clients", "capture-pane", "wait-for", "select-layout", "select-pane", "rename-window", "resize-pane",
		"select-window", "switch-client", "set-environment":
		return "", nil
	case "set-option":
		if contains(c, "-g") && !contains(c, "-gu") {
			f.opts[c[len(c)-2]] = c[len(c)-1]
		}
		if contains(c, "-gu") {
			delete(f.opts, c[len(c)-1])
		}
		return "", nil
	case "if-shell":
		if strings.Contains(c[len(c)-1], "set-option -gu @moox-restoring") {
			delete(f.opts, "@moox-restoring")
		}
		return "", nil
	case "rename-session":
		if s := f.sess(rflag(c, "-t")); s != nil {
			s.name = c[len(c)-1]
		}
		return "", nil
	case "kill-session":
		s := f.sess(rflag(c, "-t"))
		if s == nil {
			return "", errors.New("fake: no session")
		}
		var keep []*rSess
		for _, x := range f.sessions {
			if x != s {
				keep = append(keep, x)
			}
		}
		f.sessions = keep
		return "", nil
	case "new-session":
		s := &rSess{id: f.id("$"), name: rflag(c, "-s")}
		if f.sess(s.name) != nil {
			return "", errors.New("fake: duplicate session: " + s.name)
		}
		f.sessions = append(f.sessions, s)
		if g := rflag(c, "-t"); g != "" {
			return print(map[string]string{"session_id": s.id}), nil
		}
		w := &rWin{id: f.id("@"), sess: s.id, idx: atoi(f.opts["base-index"])}
		f.wins = append(f.wins, w)
		p := &rPane{id: f.id("%"), win: w.id, cmd: "zsh", pid: 9000 + f.n, env: envOf(c)}
		f.panes = append(f.panes, p)
		return print(map[string]string{"session_id": s.id, "window_id": w.id, "pane_id": p.id}), nil
	case "move-window":
		return "", nil
	case "new-window":
		sid, idx, _ := strings.Cut(rflag(c, "-t"), ":")
		if f.sess(sid) == nil {
			return "", errors.New("fake: no session " + sid)
		}
		w := &rWin{id: f.id("@"), sess: sid, idx: atoi(idx)}
		f.wins = append(f.wins, w)
		p := &rPane{id: f.id("%"), win: w.id, cmd: "zsh", pid: 9000 + f.n, env: envOf(c)}
		f.panes = append(f.panes, p)
		return print(map[string]string{"window_id": w.id, "pane_id": p.id}), nil
	case "split-window":
		p := &rPane{id: f.id("%"), win: rflag(c, "-t"), cmd: "zsh", pid: 9000 + f.n, env: envOf(c)}
		f.panes = append(f.panes, p)
		return print(map[string]string{"pane_id": p.id}), nil
	}
	return "", fmt.Errorf("fake: unknown command %q", c[0])
}

func contains(c []string, s string) bool {
	for _, x := range c {
		if x == s {
			return true
		}
	}
	return false
}

func envOf(c []string) []string {
	var out []string
	for i := 0; i+1 < len(c); i++ {
		if c[i] == "-e" {
			out = append(out, c[i+1])
		}
	}
	return out
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// rprocs is the process side: one ps table and liveness. jobs is the
// job-control table; "" derives it from table (jobsFromTable).
type rprocs struct {
	table string
	jobs  string
	alive map[int]bool
}

func (p rprocs) Table(context.Context) ([]byte, error) { return []byte(p.table), nil }
func (p rprocs) Jobs(context.Context) ([]byte, error) {
	if p.jobs != "" {
		return []byte(p.jobs), nil
	}
	return jobsFromTable([]byte(p.table)), nil
}

// jobsFromTable models a job-control shell on a fake ps table: every process
// leads its own group, and a process with a child gives its terminal's
// foreground to its first child (a foreground job), so "a child of the
// pane's shell" stays a busy pane, as the fake tests mean it.
func jobsFromTable(table []byte) []byte {
	t := capture.ParsePS(table)
	var b strings.Builder
	for _, p := range t.Procs {
		fg := p.Pid
		for _, c := range t.Procs {
			if c.PPid == p.Pid && c.Pid != p.Pid {
				fg = c.Pid
				break
			}
		}
		fmt.Fprintf(&b, "%d %d %d %d S\n", p.Pid, p.PPid, p.Pid, fg)
	}
	return []byte(b.String())
}

// jobLine is one job-control row: pid ppid pgid tpgid stat.
func jobLine(pid, ppid, pgid, tpgid int, stat string) string {
	return fmt.Sprintf("%d %d %d %d %s\n", pid, ppid, pgid, tpgid, stat)
}
func (p rprocs) Env(context.Context, int) ([]byte, error) { return nil, nil }
func (p rprocs) Alive(pid int) bool                       { return p.alive[pid] }

// rscripts records every script call and answers reclaim from a table.
type rscripts struct {
	mu    sync.Mutex
	calls [][]string
	out   map[string]string // first arg → output
}

func (s *rscripts) Run(_ context.Context, _ []string, path string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string{filepath.Base(path)}, args...))
	if len(args) > 0 {
		return []byte(s.out[args[0]]), nil
	}
	return nil, nil
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newRestoreEngine is an engine on the fake, with scratch roots and an
// instant LAUNCH probe.
func newRestoreEngine(t *testing.T, f *rfake) (*Engine, *rscripts, *logBuf) {
	t.Helper()
	home := t.TempDir()
	log := &logBuf{}
	e, err := New(Options{Target: tmuxtarget.Target{Bin: "tmux", Socket: filepath.Join(home, "moox-test-fake")},
		Env: tmuxtarget.MapEnv{"HOME": home}, Producer: "test", Log: log})
	if err != nil {
		t.Fatal(err)
	}
	sc := &rscripts{out: map[string]string{}}
	e.tmux, e.procs, e.scripts = f, rprocs{alive: map[int]bool{}}, sc
	e.timing = &launchTiming{first: time.Millisecond, every: time.Millisecond, settle: time.Millisecond, deadline: 200 * time.Millisecond}
	f.opts["@moox-root"] = filepath.Join(home, "share")
	return e, sc, log
}

// vLayout is a valid v1 layout of n stacked 80-wide panes with ids ids.
func vLayout(ids ...string) string {
	if len(ids) == 1 {
		return (&layout.Node{Kind: layout.KindPane, W: 80, H: 24, Last: -1, Z: -1, SavedPane: ids[0]}).EncodeV1()
	}
	root := &layout.Node{Kind: layout.KindV, W: 80, H: 25*len(ids) - 1}
	for i, id := range ids {
		root.Children = append(root.Children, &layout.Node{Kind: layout.KindPane, W: 80, H: 24, Y: 25 * i, Last: -1, Z: -1,
			SavedPane: id, Index: i})
	}
	return root.EncodeV1()
}

// snapOf builds a valid snapshot: spec is "session:window:paneCount" items.
func snapOf(spec ...string) *snapshot.Snapshot {
	s := &snapshot.Snapshot{Format: snapshot.FormatName, Version: 1, Source: snapshot.SourceSave, Client: &snapshot.Client{}}
	seen := map[string]bool{}
	id := 0
	for _, it := range spec {
		f := strings.Split(it, ":")
		name, win, n := f[0], atoi(f[1]), atoi(f[2])
		if !seen[name] {
			seen[name] = true
			s.Sessions = append(s.Sessions, snapshot.Session{Name: name, ActiveWindow: win, LastWindow: -1})
		}
		var ids []string
		for i := 0; i < n; i++ {
			id++
			pid := "%" + strconv.Itoa(id)
			ids = append(ids, pid)
			s.Panes = append(s.Panes, snapshot.Pane{Key: snapshot.PaneKey(name, win, i), Session: name, Window: win, Index: i,
				SavedID: pid, Cwd: "/", Role: snapshot.RoleShell, Active: i == 0})
		}
		s.Windows = append(s.Windows, snapshot.Window{Session: name, Index: win, Name: "w", Layout: vLayout(ids...),
			LayoutFormat: snapshot.LayoutV1, AutomaticRename: snapshot.AutoRenameUnset})
	}
	if len(s.Sessions) > 0 {
		s.Client.Session = s.Sessions[0].Name
	}
	return s
}

// publish writes snap as this engine's moox-last.
func publish(t *testing.T, e *Engine, snap *snapshot.Snapshot) string {
	t.Helper()
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	l, err := st.TryLock(e.record())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	name, _ := st.NewNames(time.Now())
	if err := st.Publish(l, snap, name, "", nil); err != nil {
		t.Fatal(err)
	}
	return name
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
