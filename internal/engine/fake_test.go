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
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// The fake world: a tmux server model good enough to answer exactly the
// commands Save issues, plus a ps table. Formats are rendered the way tmux
// renders them (#{name} → value), so the engine's own field lists are what is
// exercised.

type fPane struct {
	id, session        string
	window             int
	wid                string
	index              int
	title, cwd, cmd    string
	pid                int
	active, dead       bool
	ttObs, start       string
	mcpOwner, mcpPane  string
	ccFrozen, contents string
}

type fWindow struct {
	id, session          string
	index                int
	name                 string
	active, last, zoomed bool
	layout               string
	auto                 bool // effective automatic-rename
	autoLocal            bool // auto is set on the window itself, even when it equals the global
	w, h                 int
}

type fSession struct {
	id, name, group string
	grouped         bool
}

type fClient struct {
	session, last string
	activity      int64
}

type fProc struct {
	pid, ppid int
	start     string
	cmd       string
}

type world struct {
	pid        int
	start      int64
	version    string
	best       string
	opts       map[string]string
	globalAuto string
	sessions   []fSession
	windows    []fWindow
	panes      []fPane
	clients    []fClient
	procs      []fProc
	env        map[int]string
	dead       map[int]bool // pids that are gone (kill -0 fails)
	extraAlive map[int]bool // alive but not in the table (started after ps)

	failCapture map[string]bool
	calls       [][]string
	envCalls    []int
	psCalls     int
	nextPid     int
	nextPane    int
	nextWin     int
	nextSess    int
}

func newWorld() *world {
	return &world{pid: 812, start: 1761262800, version: "3.8", opts: map[string]string{"@moox-active": "1"},
		globalAuto: "on", env: map[int]string{}, dead: map[int]bool{}, extraAlive: map[int]bool{},
		failCapture: map[string]bool{}, nextPid: 1000, nextPane: 1, nextWin: 1, nextSess: 1}
}

func (w *world) session(name string) string {
	id := fmt.Sprintf("$%d", w.nextSess)
	w.nextSess++
	w.sessions = append(w.sessions, fSession{id: id, name: name})
	if w.best == "" {
		w.best = name
	}
	return id
}

// window adds a window of n shell panes laid out side by side (a real v2
// layout), and returns the window id and the pane ids.
func (w *world) window(session string, index, n int) (string, []string) {
	wid := fmt.Sprintf("@%d", w.nextWin)
	w.nextWin++
	width, height := 160, 40
	root := &layout.Node{Kind: layout.KindH, W: width, H: height, Last: -1, Z: -1}
	var ids []string
	x := 0
	for i := 0; i < n; i++ {
		pw := (width - (n - 1)) / n
		if i == n-1 {
			pw = width - x
		}
		id := fmt.Sprintf("%%%d", w.nextPane)
		w.nextPane++
		pid := w.nextPid
		w.nextPid += 10
		leaf := &layout.Node{Kind: layout.KindPane, W: pw, H: height, X: x, Index: i, Last: -1, Z: -1, SavedPane: id, Active: i == 0}
		root.Children = append(root.Children, leaf)
		x += pw + 1
		w.panes = append(w.panes, fPane{id: id, session: session, window: index, wid: wid, index: i + 1,
			cwd: "/home/user/p" + id[1:], cmd: "zsh", pid: pid, active: i == 0})
		w.procs = append(w.procs, fProc{pid: pid, ppid: w.pid, cmd: "-zsh"})
		ids = append(ids, id)
	}
	lay := root.EncodeV2()
	if n == 1 {
		root.Children[0].W = width
		lay = root.Children[0].EncodeV2()
	}
	w.windows = append(w.windows, fWindow{id: wid, session: session, index: index, name: "zsh", active: len(w.windowsOf(session)) == 0,
		layout: lay, auto: true, w: width, h: height})
	return wid, ids
}

func (w *world) windowsOf(session string) []fWindow {
	var out []fWindow
	for _, x := range w.windows {
		if x.session == session {
			out = append(out, x)
		}
	}
	return out
}

func (w *world) pane(id string) *fPane {
	for i := range w.panes {
		if w.panes[i].id == id {
			return &w.panes[i]
		}
	}
	return nil
}

// child adds a process under parent and returns its pid.
func (w *world) child(parent int, cmd string) int {
	pid := w.nextPid
	w.nextPid += 10
	w.procs = append(w.procs, fProc{pid: pid, ppid: parent, cmd: cmd})
	return pid
}

// killPane removes a pane, renumbering the later panes of its window the way
// tmux does, and the window's layout leaf.
func (w *world) killPane(t *testing.T, id string) {
	t.Helper()
	p := w.pane(id)
	var out []fPane
	for _, q := range w.panes {
		if q.id == id {
			continue
		}
		if q.wid == p.wid && q.session == p.session && q.index > p.index {
			q.index--
		}
		out = append(out, q)
	}
	w.panes = out
	for i := range w.windows {
		if w.windows[i].id != p.wid {
			continue
		}
		n, f, err := layout.Parse(w.windows[i].layout)
		if err != nil {
			t.Fatal(err)
		}
		if err := n.RemoveLeaf(id); err != nil {
			t.Fatal(err)
		}
		w.windows[i].layout = n.Encode(f)
	}
}

var fieldRe = regexp.MustCompile(`#\{([^}]*)\}`)

func render(format string, get func(string) string) string {
	return fieldRe.ReplaceAllStringFunc(format, func(m string) string { return get(m[2 : len(m)-1]) })
}

func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (w *world) serverField(name string) string {
	switch name {
	case "pid":
		return strconv.Itoa(w.pid)
	case "start_time":
		return strconv.FormatInt(w.start, 10)
	case "version":
		return w.version
	case "session_name":
		return w.best
	}
	return w.opts[name]
}

func (w *world) paneField(p fPane) func(string) string {
	return func(name string) string {
		switch name {
		case "pane_id":
			return p.id
		case "session_name":
			return p.session
		case "window_index":
			return strconv.Itoa(p.window)
		case "window_id":
			return p.wid
		case "pane_index":
			return strconv.Itoa(p.index)
		case "pane_title":
			return p.title
		case "pane_current_path":
			return p.cwd
		case "pane_active":
			return b2s(p.active)
		case "pane_current_command":
			return p.cmd
		case "pane_pid":
			return strconv.Itoa(p.pid)
		case "pane_dead":
			return b2s(p.dead)
		case "history_size", "cursor_y", "alternate_on":
			return "0"
		case "@tt_obs":
			return p.ttObs
		case "pane_start_command":
			return p.start
		case "@mcp_owner":
			return p.mcpOwner
		case "@mcp_pane":
			return p.mcpPane
		case "@cc-frozen":
			return p.ccFrozen
		}
		return ""
	}
}

func (w *world) windowField(x fWindow) func(string) string {
	return func(name string) string {
		switch name {
		case "window_id":
			return x.id
		case "session_name":
			return x.session
		case "window_index":
			return strconv.Itoa(x.index)
		case "window_name":
			return x.name
		case "window_active":
			return b2s(x.active)
		case "window_last_flag":
			return b2s(x.last)
		case "window_zoomed_flag":
			return b2s(x.zoomed)
		case "window_layout":
			return x.layout
		case "automatic-rename":
			return b2s(x.auto)
		case "window_width":
			return strconv.Itoa(x.w)
		case "window_height":
			return strconv.Itoa(x.h)
		}
		return ""
	}
}

var errFake = errors.New("fake tmux: command failed")

// Run implements tmuxRunner.
func (w *world) Run(_ context.Context, args ...string) ([]byte, error) {
	w.calls = append(w.calls, append([]string(nil), args...))
	var out strings.Builder
	var cmds [][]string
	var cur []string
	for _, a := range args {
		if a == ";" {
			cmds = append(cmds, cur)
			cur = nil
			continue
		}
		cur = append(cur, a)
	}
	cmds = append(cmds, cur)
	for _, c := range cmds {
		if err := w.exec(c, &out); err != nil {
			return []byte(out.String()), err
		}
	}
	return []byte(out.String()), nil
}

func flag(c []string, f string) (string, bool) {
	for i := 0; i+1 < len(c); i++ {
		if c[i] == f {
			return c[i+1], true
		}
	}
	return "", false
}

func (w *world) exec(c []string, out *strings.Builder) error {
	last := c[len(c)-1]
	switch c[0] {
	case "display-message":
		if t, ok := flag(c, "-t"); ok {
			if w.failCapture[t] {
				return errFake // the pane is gone: its own display fails first
			}
			if p := w.pane(t); p != nil {
				out.WriteString(render(last, w.paneField(*p)) + "\n")
				return nil
			}
			for _, x := range w.windows {
				if x.id == t {
					out.WriteString(render(last, w.windowField(x)) + "\n")
					return nil
				}
			}
			return errFake
		}
		out.WriteString(render(last, w.serverField) + "\n")
	case "list-clients":
		for _, cl := range w.clients {
			out.WriteString(render(last, func(n string) string {
				switch n {
				case "client_session":
					return cl.session
				case "client_last_session":
					return cl.last
				case "client_activity":
					return strconv.FormatInt(cl.activity, 10)
				}
				return ""
			}) + "\n")
		}
	case "list-sessions":
		for _, s := range w.sessions {
			out.WriteString(render(last, func(n string) string {
				switch n {
				case "session_id":
					return s.id
				case "session_name":
					return s.name
				case "session_grouped":
					return b2s(s.grouped)
				case "session_group":
					return s.group
				}
				return ""
			}) + "\n")
		}
	case "list-windows":
		for _, x := range w.windows {
			out.WriteString(render(last, w.windowField(x)) + "\n")
		}
	case "list-panes":
		for _, p := range w.panes {
			out.WriteString(render(last, w.paneField(p)) + "\n")
		}
	case "show-options":
		t, ok := flag(c, "-t")
		if !ok {
			out.WriteString(w.globalAuto + "\n")
			return nil
		}
		for _, x := range w.windows {
			if x.id != t {
				continue
			}
			// -wqv without -A: the window's own value, or nothing
			if x.autoLocal || x.auto != (w.globalAuto != "off") {
				out.WriteString(map[bool]string{true: "on", false: "off"}[x.auto] + "\n")
			}
			return nil
		}
		return errFake
	case "capture-pane":
		t, _ := flag(c, "-t")
		p := w.pane(t)
		if p == nil || w.failCapture[t] {
			return errFake
		}
		out.WriteString(p.contents)
	case "set-option":
		if c[1] == "-gu" {
			delete(w.opts, c[2])
		} else {
			w.opts[c[2]] = c[3]
		}
	case "if-shell":
		m := regexp.MustCompile(`^#\{==:#\{(@[^}]+)\},(.*)\}$`).FindStringSubmatch(c[2])
		if m != nil && w.opts[m[1]] == m[2] {
			delete(w.opts, m[1])
		}
	default:
		return fmt.Errorf("fake tmux: unexpected command %q", c)
	}
	return nil
}

// procLister
func (w *world) Table(context.Context) ([]byte, error) {
	w.psCalls++
	var b strings.Builder
	for _, p := range w.procs {
		st := p.start
		if st == "" {
			st = "Sat Oct 25 03:00:00 2026"
		}
		fmt.Fprintf(&b, "%6d %6d %s %s\n", p.pid, p.ppid, st, p.cmd)
	}
	return []byte(b.String()), nil
}

func (w *world) Jobs(ctx context.Context) ([]byte, error) {
	n := w.psCalls
	t, _ := w.Table(ctx)
	w.psCalls = n // the save's one-ps count (B25) is not the jobs read
	return jobsFromTable(t), nil
}

func (w *world) Env(_ context.Context, pid int) ([]byte, error) {
	w.envCalls = append(w.envCalls, pid)
	return []byte(w.env[pid]), nil
}

func (w *world) Alive(pid int) bool {
	if w.dead[pid] {
		return false
	}
	if w.extraAlive[pid] {
		return true
	}
	for _, p := range w.procs {
		if p.pid == pid {
			return true
		}
	}
	return false
}

// harness is an Engine over a world with a scratch HOME/XDG.
type harness struct {
	e     *Engine
	w     *world
	home  string
	clock time.Time
	log   strings.Builder
}

func newHarness(t *testing.T, w *world) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{w: w, home: filepath.Join(root, "home"), clock: time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)}
	env := tmuxtarget.MapEnv{"HOME": h.home, "XDG_STATE_HOME": filepath.Join(root, "xs"),
		"XDG_DATA_HOME": filepath.Join(root, "xd"), "XDG_CONFIG_HOME": filepath.Join(root, "xc")}
	e, err := New(Options{Target: tmuxtarget.Target{Bin: "/nonexistent/tmux", Socket: filepath.Join(root, "moox-test-fake")},
		Env: env, Producer: "moox 0.0.0-test", Log: &h.log, Now: func() time.Time { return h.clock }})
	if err != nil {
		t.Fatal(err)
	}
	e.tmux, e.procs = w, w
	h.e = e
	return h
}

func (h *harness) save(t *testing.T, r SaveRequest) SaveResult {
	t.Helper()
	h.clock = h.clock.Add(time.Minute)
	res, err := h.e.Save(context.Background(), r)
	if err != nil {
		t.Fatalf("Save: %v\nlog:\n%s", err, h.log.String())
	}
	return res
}

func (h *harness) mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) byPid() string { return h.e.paths.ByPid("") }

func (h *harness) register(t *testing.T, pid int, sid string) {
	t.Helper()
	h.mustWrite(t, filepath.Join(h.byPid(), strconv.Itoa(pid)+".session-id"), sid+"\nsome title\n")
}

func (h *harness) record(t *testing.T, paneID, line string) {
	t.Helper()
	h.mustWrite(t, filepath.Join(h.e.paths.Launch, strings.TrimPrefix(paneID, "%")), line)
}

// paths for assertions
func (h *harness) paths() sockets.Paths { return h.e.paths }
