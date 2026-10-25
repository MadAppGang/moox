package saver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// ---------------------------------------------------------------------------
// fake clock: timers fire only when the test advances time
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	d       time.Duration
	c       chan time.Time
	stopped bool
	fired   bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool {
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), d: d, c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}

// next is the earliest live timer due at or before limit.
func (c *fakeClock) next(limit time.Time) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *fakeTimer
	for _, t := range c.timers {
		if t.fired || t.stopped || t.at.After(limit) {
			continue
		}
		if best == nil || t.at.Before(best.at) {
			best = t
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// fake engine
// ---------------------------------------------------------------------------

type saveCall struct {
	at  time.Duration // since the harness start
	req engine.SaveRequest
}

type fakeEngine struct {
	mu       sync.Mutex
	h        *harness
	saves    []saveCall
	result   func(n int, r engine.SaveRequest) (engine.SaveResult, error)
	attached int
	restores []engine.RestoreRequest
	launch   chan engine.LaunchVerdict
	precond  error
	hbStart  chan struct{}
	hbBlock  chan struct{}
	hbCount  int
	boot     bool // WaitBootstrap's answer
	skipped  []string
}

func (f *fakeEngine) Save(_ context.Context, r engine.SaveRequest) (engine.SaveResult, error) {
	f.mu.Lock()
	f.saves = append(f.saves, saveCall{at: f.h.elapsed(), req: r})
	n := len(f.saves)
	f.mu.Unlock()
	if f.result != nil {
		res, err := f.result(n, r)
		if res.AttachedClients == 0 {
			res.AttachedClients = f.attached
		}
		return res, err
	}
	return engine.SaveResult{Published: true, File: fmt.Sprintf("moox-%d.json", n), SnapshotFP: fmt.Sprint(n),
		AttachedClients: f.attached}, nil
}

func (f *fakeEngine) Restore(_ context.Context, r engine.RestoreRequest) (engine.RestoreResult, error) {
	f.mu.Lock()
	f.restores = append(f.restores, r)
	f.mu.Unlock()
	return engine.RestoreResult{Launch: f.launch, Boot: engine.BootVerdict{Result: "PASS"}}, nil
}

func (f *fakeEngine) ManualPreconditions(context.Context) error { return f.precond }

func (f *fakeEngine) WaitBootstrap(context.Context, time.Duration, time.Duration) bool { return f.boot }

func (f *fakeEngine) SkipBoot(_ context.Context, reason, _ string) {
	f.mu.Lock()
	f.skipped = append(f.skipped, reason)
	f.mu.Unlock()
}

func (f *fakeEngine) Heartbeat(context.Context) error {
	f.mu.Lock()
	f.hbCount++
	start, block := f.hbStart, f.hbBlock
	f.mu.Unlock()
	if start != nil {
		start <- struct{}{}
	}
	if block != nil {
		<-block
	}
	return nil
}

func (f *fakeEngine) saveCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.saves) }

// ---------------------------------------------------------------------------
// fake tmux: global options and wait-for signals
// ---------------------------------------------------------------------------

type fakeTmux struct {
	mu      sync.Mutex
	opts    map[string]string
	signals []string
	calls   [][]string
}

func newFakeTmux() *fakeTmux { return &fakeTmux{opts: map[string]string{}} }

func (f *fakeTmux) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	var out strings.Builder
	for _, c := range splitChain(args) {
		if err := f.one(c, &out); err != nil {
			return []byte(out.String()), err
		}
	}
	return []byte(out.String()), nil
}

// splitChain undoes tmuxtarget.Chain: ";" separates, a trailing `\;` is a
// literal ';'.
func splitChain(args []string) [][]string {
	var cmds [][]string
	var cur []string
	for _, a := range args {
		if a == ";" {
			cmds = append(cmds, cur)
			cur = nil
			continue
		}
		if strings.HasSuffix(a, `\;`) {
			a = strings.TrimSuffix(a, `\;`) + ";"
		}
		cur = append(cur, a)
	}
	return append(cmds, cur)
}

func (f *fakeTmux) one(c []string, out *strings.Builder) error {
	switch {
	case len(c) == 2 && c[0] == "show-options" && c[1] == "-g":
		var names []string
		for k := range f.opts {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(out, "%s %q\n", k, f.opts[k])
		}
	case len(c) == 3 && c[0] == "show-options" && c[1] == "-gqv":
		if v, ok := f.opts[c[2]]; ok {
			out.WriteString(v + "\n")
		}
	case len(c) == 3 && c[0] == "display-message" && c[1] == "-p":
		s := c[2]
		for {
			i := strings.Index(s, "#{")
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], "}")
			s = s[:i] + f.opts[s[i+2:i+j]] + s[i+j+1:]
		}
		out.WriteString(s + "\n")
	case len(c) == 4 && c[0] == "set-option" && c[1] == "-g":
		f.opts[c[2]] = c[3]
	case len(c) == 3 && c[0] == "set-option" && c[1] == "-gu":
		delete(f.opts, c[2])
	case len(c) == 3 && c[0] == "wait-for" && c[1] == "-S":
		f.signals = append(f.signals, c[2])
	case len(c) >= 1 && c[0] == "if-shell":
		// engine.ClearRestoringArgs: #{==:#{@moox-restoring},v}
		v := strings.TrimSuffix(strings.TrimPrefix(c[2], "#{==:#{@moox-restoring},"), "}")
		if f.opts["@moox-restoring"] == v {
			delete(f.opts, "@moox-restoring")
		}
	default:
		return fmt.Errorf("fake tmux: unhandled %q", c)
	}
	return nil
}

func (f *fakeTmux) opt(name string) string { f.mu.Lock(); defer f.mu.Unlock(); return f.opts[name] }
func (f *fakeTmux) set(name, v string)     { f.mu.Lock(); defer f.mu.Unlock(); f.opts[name] = v }
func (f *fakeTmux) signalled(ch string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.signals {
		if s == ch {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// harness: drives one machine on one goroutine, deterministically
// ---------------------------------------------------------------------------

type harness struct {
	t     *testing.T
	clk   *fakeClock
	start time.Time
	eng   *fakeEngine
	tmux  *fakeTmux
	m     *machine
	log   strings.Builder
	logMu sync.Mutex
	alive map[int]bool
}

func (h *harness) Write(p []byte) (int, error) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.log.Write(p)
}

func (h *harness) logText() string { h.logMu.Lock(); defer h.logMu.Unlock(); return h.log.String() }

func newHarness(t *testing.T, mod ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, clk: newFakeClock(), tmux: newFakeTmux(), alive: map[int]bool{}}
	h.start = h.clk.Now()
	h.eng = &fakeEngine{h: h, launch: make(chan engine.LaunchVerdict, 1)}
	c := Config{Engine: h.eng, Tmux: h.tmux, Clock: h.clk, Debounce: 3 * time.Second, MaxWait: 30 * time.Second,
		Backup: 15 * time.Minute, Log: h, Alive: func(pid int) bool { return h.alive[pid] }}
	for _, f := range mod {
		f(&c)
	}
	h.m = newMachine(context.Background(), c)
	return h
}

func (h *harness) elapsed() time.Duration { return h.clk.Now().Sub(h.start) }

// advance moves time forward by d, firing every timer due on the way, in
// order, through the machine's own handler for it.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	limit := h.clk.Now().Add(d)
	for {
		t := h.clk.next(limit)
		if t == nil {
			break
		}
		h.clk.mu.Lock()
		h.clk.now = t.at
		t.fired = true
		h.clk.mu.Unlock()
		switch {
		case h.m.debounce != nil && h.m.debounce.(*fakeTimer) == t:
			h.m.onDebounce()
		case h.m.backup != nil && h.m.backup.(*fakeTimer) == t:
			h.m.onBackup()
		default:
			h.t.Fatalf("a timer fired that the machine no longer holds (armed for %s)", t.d)
		}
	}
	h.clk.mu.Lock()
	h.clk.now = limit
	h.clk.mu.Unlock()
}

// endRestore delivers the restore goroutine's RESTORE-END to the loop.
func (h *harness) endRestore() {
	h.t.Helper()
	select {
	case end := <-h.m.restoreDone:
		h.m.onRestoreEnd(end)
	case <-time.After(5 * time.Second):
		h.t.Fatal("the restore goroutine never ended")
	}
}

func (h *harness) saveTimes() []time.Duration {
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	var out []time.Duration
	for _, s := range h.eng.saves {
		out = append(out, s.at)
	}
	return out
}

func (h *harness) idle() {
	h.t.Helper()
	if n := h.m.armedTimers(); n != 0 {
		h.t.Fatalf("%d timer(s) armed on an idle server (debounce=%v backup=%v)", n, h.m.debounce != nil, h.m.backup != nil)
	}
	if n := h.clk.live(); n != 0 {
		h.t.Fatalf("%d live timer(s) on the clock on an idle server", n)
	}
}

func (c *fakeClock) live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.fired && !t.stopped {
			n++
		}
	}
	return n
}

var errBoom = errors.New("boom")

// layoutV1 is tmux's v1 layout string for one 80x24 pane with id n.
func layoutV1(n int) string {
	body := fmt.Sprintf("80x24,0,0,%d", n)
	csum := 0
	for _, c := range []byte(body) {
		csum = (csum >> 1) + ((csum & 1) << 15)
		csum += int(c)
		csum &= 0xffff
	}
	return fmt.Sprintf("%04x,%s", csum, body)
}

// writeValidSnapshot writes a two-window snapshot that Validate accepts, at
// a path with a space in it.
func writeValidSnapshot(t *testing.T) string {
	t.Helper()
	s := &snapshot.Snapshot{Format: snapshot.FormatName, Version: 1, Source: snapshot.SourceSave,
		Client: &snapshot.Client{Session: "a"}}
	s.Sessions = []snapshot.Session{{Name: "a", ActiveWindow: 0, LastWindow: -1}}
	for w := 0; w < 2; w++ {
		id := "%" + fmt.Sprint(w+1)
		s.Panes = append(s.Panes, snapshot.Pane{Key: snapshot.PaneKey("a", w, 0), Session: "a", Window: w, Index: 0,
			SavedID: id, Cwd: "/", Role: snapshot.RoleShell, Active: true})
		s.Windows = append(s.Windows, snapshot.Window{Session: "a", Index: w, Name: "w", Layout: layoutV1(w + 1),
			LayoutFormat: snapshot.LayoutV1, AutomaticRename: snapshot.AutoRenameUnset})
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatalf("the fixture snapshot does not validate: %v", err)
	}
	b, err := snapshot.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "moox snap.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
