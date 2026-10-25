//go:build tmuxe2e

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

const v3SID = "0b9d6a3e-1111-4222-8333-444455556666"
const v3FrozenKey = "1761350400-a1b2c3"

// geometry is a layout's shape without ids or checksum: kind, size and
// offset of every cell in DFS order.
func geometry(t *testing.T, s string) string {
	t.Helper()
	root, _, err := layout.Parse(s)
	if err != nil {
		t.Fatalf("layout %q: %v", s, err)
	}
	var b strings.Builder
	var walk func(n *layout.Node)
	walk = func(n *layout.Node) {
		fmt.Fprintf(&b, "%c%dx%d,%d,%d ", n.Kind, n.W, n.H, n.X, n.Y)
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	return b.String()
}

// v3Recordings collects each subtest's recordings after the restore, by
// pane key, so zsh and bash can be compared (§7.1: same verdicts and the
// same recordings).
var v3Recordings sync.Map

// TestE2E_V3_RoundTrip (V3, T3): a workspace with hook-shell panes — three
// sessions (one grouped, one named api.v2, one a:b), splits, cwds with a
// space, `#` and `;`, a title `x;`, a window name `fix; test`, a `#(touch …)`
// title, a recorded launch with an inner `;`, one fake Claude (by-pid
// registry + a process named claude), one frozen tombstone, one zoomed
// window and a tt-observer pane between two panes — is saved, the server
// closed, and a COLD server on the real config restores it through the boot
// path itself (gate, purges, bootstrap poll, rename, drop) with
// MOOX_STUB_COMMANDS=1.
func TestE2E_V3_RoundTrip(t *testing.T) {
	for _, kind := range []string{"zsh", "bash"} {
		t.Run(kind, func(t *testing.T) { v3RoundTrip(t, kind) })
	}
	a, okA := v3Recordings.Load("zsh")
	b, okB := v3Recordings.Load("bash")
	if okA && okB && fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("recordings differ between zsh and bash:\n zsh  %v\n bash %v", a, b)
	}
}

func hookShell(t *testing.T, kind string) tmuxtest.Shell {
	if kind == "bash" {
		return tmuxtest.HookBash(t)
	}
	return tmuxtest.HookZsh(t)
}

func v3RoundTrip(t *testing.T, kind string) {
	// ---- A: build and save the workspace ------------------------------
	a := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithShell(hookShell(t, kind)), tmuxtest.WithSize(200, 50))
	if a.Opt(t, "@moox-active") != "1" {
		t.Fatalf("A's gate did not activate")
	}
	e := e2eEngine(t, a)
	sp, hash, semi := mkdir(t, a, "with space"), mkdir(t, a, "has#hash"), mkdir(t, a, "semi;colon")
	touched := filepath.Join(t.TempDir(), "touched")
	ch := tmuxtarget.Chain
	run := func(args ...string) string { return strings.TrimSpace(a.Run(t, ch(args)...)) }

	// main:0 — p0 (cwd with a space, title "x;"), the observer between,
	// p2 printing the contents line (cwd with #).
	main := strings.TrimSpace(a.Run(t, ch(tmuxtarget.NewSession{Name: tmuxtarget.Literal("main"), Cwd: tmuxtarget.Literal(sp),
		Width: 200, Height: 50, PrintFormat: "#{session_id}"}.Args())...))
	w0 := run("display", "-p", "-t", main, "#{window_id}")
	p0 := run("display", "-p", "-t", main, "#{pane_id}")
	obs := run(tmuxtarget.SplitWindow{Target: p0, Horizontal: true, Cwd: tmuxtarget.Literal(a.Home), PrintFormat: "#{pane_id}",
		Command: []string{"/bin/sh"}}.Args()...)
	run("set-option", "-p", "-t", obs, "@tt_obs", "1")
	p2 := run(tmuxtarget.SplitWindow{Target: obs, Horizontal: true, Cwd: tmuxtarget.Literal(hash), PrintFormat: "#{pane_id}",
		Command: []string{"printf 'hello-v3-contents\\n'; exec /bin/sh"}}.Args()...)
	run(tmuxtarget.SelectPaneTitle(p0, tmuxtarget.Literal("x;"))...)
	// main:1 "fix; test" — two panes, the second zoomed, a #(touch) title.
	w1 := run(tmuxtarget.NewWindow{Target: main + ":", Name: tmuxtarget.Literal("fix; test"), Cwd: tmuxtarget.Literal(semi),
		PrintFormat: "#{window_id}"}.Args()...)
	p4 := run(tmuxtarget.SplitWindow{Target: w1, Cwd: tmuxtarget.Literal(semi), PrintFormat: "#{pane_id}"}.Args()...)
	run(tmuxtarget.SelectPaneTitle(p4, tmuxtarget.Literal("#(touch "+touched+")"))...)
	run("select-pane", "-t", p4)
	run("resize-pane", "-Z", "-t", p4)
	// main-view: grouped with main.
	run(tmuxtarget.NewSession{Name: tmuxtarget.Literal("main-view"), Group: main}.Args()...)
	// api.v2 — the fake Claude, and a window whose pane has a recording.
	api := strings.TrimSpace(a.Run(t, ch(tmuxtarget.NewSession{Name: tmuxtarget.Literal("api.v2"), Cwd: tmuxtarget.Literal(a.Home),
		PrintFormat: "#{session_id}", Command: []string{"exec -a claude sleep 1000"}}.Args())...))
	claudePane := run("display", "-p", "-t", api, "#{pane_id}")
	claudePid := run("display", "-p", "-t", claudePane, "#{pane_pid}")
	// (a plain /bin/sh: a hook shell would clear the planted recording at
	// its next prompt, B2)
	recPane := run(tmuxtarget.NewWindow{Target: api + ":", Cwd: tmuxtarget.Literal(sp), PrintFormat: "#{pane_id}",
		Command: []string{"/bin/sh"}}.Args()...)
	record(t, e, recPane, "sleep 1000; true")
	byPid := filepath.Join(a.Home, ".config", "tmux-claude", "panes", "by-pid")
	if err := os.MkdirAll(byPid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(byPid, claudePid+".session-id"), []byte(v3SID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a:b — the tombstone.
	ab := strings.TrimSpace(a.Run(t, ch(tmuxtarget.NewSession{Name: tmuxtarget.Literal("a:b"), Cwd: tmuxtarget.Literal(a.Home),
		PrintFormat: "#{session_id}"}.Args())...))
	tomb := run("display", "-p", "-t", ab, "#{pane_id}")
	run(tmuxtarget.SelectPaneTitle(tomb, tmuxtarget.Literal("❄ FROZEN "+v3FrozenKey+" 1p/0s 2026-10-25"))...)
	run("set-option", "-p", "-t", tomb, "@cc-frozen", v3FrozenKey)
	run("set-option", "-p", "-t", tomb, "allow-rename", "off")
	run("new-window", "-d", "-t", ab+":")
	_ = w0

	if !tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		return strings.Contains(a.Run(t, "capture-pane", "-p", "-t", p2), "hello-v3-contents")
	}) {
		t.Fatal("the contents pane never printed")
	}
	saved := saveNow(t, e, a)
	roles := map[string]int{}
	for _, p := range saved.Panes {
		roles[p.Role]++
	}
	if roles[snapshot.RoleTTObserver] != 1 || roles[snapshot.RoleFrozen] != 1 || roles[snapshot.RoleClaude] != 1 {
		t.Fatalf("saved roles %v", roles)
	}
	plan, err := buildPlan(saved, planInputs{relaunch: relaunch.Options{NvimSession: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapDir := e.Paths().Snapshots
	a.Close()

	// ---- B: cold start on the saved snapshot dir, stubbed ---------------
	b := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithShell(hookShell(t, kind)), tmuxtest.WithSize(200, 50),
		tmuxtest.WithSnapshot(snapDir), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"),
		tmuxtest.WithPrepare(func(s *tmuxtest.Server) {
			fd := s.Paths(t).FreezeDir
			if err := os.MkdirAll(fd, 0o700); err != nil {
				t.Fatal(err)
			}
			state := "v\t1\nunit\tpane\nserver_pid\t1\nwindow_id\t@1\npane_id\t%1\npane_count\t0\nsid_count\t0\nend\t1\n"
			if err := os.WriteFile(filepath.Join(fd, v3FrozenKey+".state"), []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fd, v3FrozenKey+".banner"), []byte("V3-FROZEN-BANNER\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}))
	if !tmuxtest.Poll(60*time.Second, 200*time.Millisecond, func() bool { return b.Opt(t, "@moox-boot-result") != "" }) {
		t.Fatalf("no boot result; daemon.log:\n%s", daemonLog(t, b))
	}
	if !tmuxtest.Poll(90*time.Second, 250*time.Millisecond, func() bool { return b.Opt(t, "@moox-restoring") == "" }) {
		t.Fatalf("@moox-restoring never cleared; daemon.log:\n%s", daemonLog(t, b))
	}
	log := daemonLog(t, b)
	if got := b.Opt(t, "@moox-boot-result"); got != "PASS" {
		t.Fatalf("@moox-boot-result %q; daemon.log:\n%s", got, log)
	}
	// two resumed rows (the Claude and the recording), each acked and OK; the
	// tombstone is reclaimed, neither considered nor judged (R2-H8)
	if !strings.Contains(log, "written 2/2 resumable, 2 considered, 0 misses") ||
		!strings.Contains(log, "LAUNCH VERDICT: PASS — 2 judged, 0 failed") || !strings.Contains(log, "FROZEN-CLAIMED") {
		t.Fatalf("daemon.log verdicts:\n%s", log)
	}
	if !strings.Contains(log, "LAUNCH VERDICT: PASS") || !strings.Contains(log, "BOOTSTRAP") {
		t.Fatalf("daemon.log has no LAUNCH PASS / bootstrap line:\n%s", log)
	}

	// panes per window, in pane order: cwd and title as saved (observer gone)
	live := map[string][]livePane{}
	for _, p := range listPanes(t, b) {
		w := p.key[:strings.LastIndexByte(p.key, '.')]
		live[w] = append(live[w], p)
	}
	byKey := map[string]livePane{}
	for _, w := range plan.Windows {
		wk := fmt.Sprintf("%s:%d", w.Session, w.Index)
		got := live[wk]
		if len(got) != len(w.Rows) {
			t.Errorf("window %s: %d panes, want %d (%+v)", wk, len(got), len(w.Rows), got)
			continue
		}
		for i, ri := range w.Rows {
			want := plan.Rows[ri].Pane
			if got[i].cwd != want.Cwd || got[i].title != want.Title {
				t.Errorf("%s: cwd %q title %q, want %q %q", got[i].key, got[i].cwd, got[i].title, want.Cwd, want.Title)
			}
			byKey[want.Key] = got[i]
		}
		layoutNow := strings.TrimSpace(b.Run(t, "display", "-p", "-t", got[0].id, "#{window_layout}"))
		if g, wg := geometry(t, layoutNow), geometry(t, w.Layout); g != wg {
			t.Errorf("window %s layout %s, want %s", wk, g, wg)
		}
	}
	for _, r := range plan.Rows {
		p, ok := byKey[r.Key]
		if !ok {
			continue
		}
		text := b.Run(t, "capture-pane", "-p", "-J", "-S", "-", "-t", p.id)
		if r.Resumes() && !strings.Contains(text, "MOOX-STUB") {
			t.Errorf("%s (%s): no stub output:\n%s", r.Key, p.id, text)
		}
		if r.Pane.SavedID == p2 {
			if n := strings.Count(text, "hello-v3-contents"); n != 1 {
				t.Errorf("contents printed %d times in %s:\n%s", n, p.id, text)
			}
		}
		if r.Pane.Role == snapshot.RoleFrozen {
			if !strings.Contains(text, "V3-FROZEN-BANNER") {
				t.Errorf("tombstone %s: no banner:\n%s", p.id, text)
			}
			if got := strings.TrimSpace(b.Run(t, "show-options", "-pqv", "-t", p.id, "@cc-frozen")); got != v3FrozenKey {
				t.Errorf("tombstone %s: @cc-frozen %q (reclaim)", p.id, got)
			}
			if got := strings.TrimSpace(b.Run(t, "show-options", "-pqv", "-t", p.id, "allow-set-title")); got != "off" {
				t.Errorf("tombstone %s: allow-set-title %q", p.id, got)
			}
		}
	}
	for _, sid := range strings.Fields(b.Run(t, "list-sessions", "-F", "#{session_id}")) {
		for _, line := range strings.Split(b.Run(t, "show-environment", "-t", sid), "\n") {
			if strings.HasPrefix(line, "MOOX_") {
				t.Errorf("session %s env holds %q", sid, line)
			}
		}
	}
	names := sessionNames(t, b)
	sort.Strings(names)
	if want := []string{"a:b", "api.v2", "boot", "main", "main-view"}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("sessions %q, want %q", names, want)
	}
	if _, err := os.Stat(touched); err == nil {
		t.Errorf("%s exists: a #(…) title ran", touched)
	}
	zoomed := strings.TrimSpace(b.Run(t, "display", "-p", "-t", byKey[paneByID(saved, p4).Key].id, "#{window_zoomed_flag} #{pane_active}"))
	if zoomed != "1 1" {
		t.Errorf("main:1 zoomed/active %q", zoomed)
	}
	// the recordings the resumed rows left (B4: the hook records the resumed
	// line first), by saved key
	recs := map[string]string{}
	bp := b.Paths(t)
	for k, p := range byKey {
		if data, err := os.ReadFile(filepath.Join(bp.Launch, strings.TrimPrefix(p.id, "%"))); err == nil && len(data) > 0 {
			recs[k] = strings.TrimSpace(string(data))
		}
	}
	if len(recs) == 0 {
		t.Errorf("no recordings in %s after the restore", bp.Launch)
	}
	v3Recordings.Store(kind, recs)
	for _, line := range strings.Split(log, "\n") {
		for _, w := range []string{"BOOT", "LAUNCH", "RESTORE", "FROZEN", "LAYOUT", "PIN", "PURGE", "CREATE", "WARN"} {
			if strings.Contains(line, w) {
				t.Log(line)
				break
			}
		}
	}
}

func daemonLog(t *testing.T, s *tmuxtest.Server) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Paths(t).State, "daemon.log"))
	if err != nil {
		return err.Error()
	}
	return string(b)
}
