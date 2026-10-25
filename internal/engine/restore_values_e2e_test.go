//go:build tmuxe2e

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// envDump makes every new pane write its environment to dir/env.<N> and then
// run /bin/sh: the oracle for what C2 delivered to which pane (§4.4), with no
// hook shell involved.
func envDump(t *testing.T, s *tmuxtest.Server) string {
	t.Helper()
	dir := t.TempDir()
	s.Run(t, "set-option", "-g", "default-command", "env > "+shq(dir)+"/env.${TMUX_PANE#%}; exec /bin/sh")
	return dir
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// paneEnv reads the dump of pane id (waiting for it to appear).
func paneEnv(t *testing.T, dir, id string) map[string]string {
	t.Helper()
	p := filepath.Join(dir, "env."+strings.TrimPrefix(id, "%"))
	var b []byte
	if !tmuxtest.Poll(5*time.Second, 20*time.Millisecond, func() bool {
		var err error
		b, err = os.ReadFile(p)
		return err == nil && len(b) > 0 && b[len(b)-1] == '\n'
	}) {
		t.Fatalf("pane %s never dumped its environment", id)
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// record writes launch/<N> for a live pane (what the hook would have
// recorded, B1).
func record(t *testing.T, e *Engine, paneID, line string) {
	t.Helper()
	if err := os.MkdirAll(e.Paths().Launch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Paths().Launch, strings.TrimPrefix(paneID, "%")), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func firstPane(t *testing.T, s *tmuxtest.Server, target string) string {
	t.Helper()
	return strings.Fields(s.Run(t, "list-panes", "-t", target, "-F", "#{pane_id}"))[0]
}

// TestE2E_Restore_ResumeNotInheritedBySiblings (C2, E1, Q3, R2-H1): the
// session env that new-session -e writes is cleared by $sid right after, for
// session names `main`, `a.b`, `a:b`, `a\b`; the first pane keeps its value,
// sibling windows and splits get nothing, and so does a window the user
// creates later.
func TestE2E_Restore_ResumeNotInheritedBySiblings(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	e.timing.deadline = 1500 * time.Millisecond // /bin/sh panes never consume: NOT-FIRED is expected
	names := []string{"main", "a.b", "a:b", `a\b`}
	for i, n := range names {
		sid := newSession(t, s, n, s.Home)
		w2 := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_id}"))
		s.Run(t, "split-window", "-d", "-t", w2)
		record(t, e, firstPane(t, s, sid), "sleep "+strconv.Itoa(1000+i))
	}
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	dump := envDump(t, s)
	resetTo(t, s, "moox-bootstrap")
	res, lv := restoreNow(t, e, false)
	if res.Boot.Misses != 0 {
		t.Fatalf("BOOT %+v", res.Boot)
	}
	if len(saved.Sessions) != len(names) {
		t.Fatalf("saved sessions %+v", saved.Sessions)
	}
	resumed := 0
	for _, se := range saved.Sessions {
		var sid string
		for _, r := range res.Plan.Rows {
			if r.Pane.Session == se.Name {
				sid = strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[r.Key], "#{session_id}"))
				break
			}
		}
		env := s.Run(t, "show-environment", "-t", sid)
		for _, line := range strings.Split(env, "\n") {
			if strings.HasPrefix(line, "MOOX_") {
				t.Errorf("session %q (%s) env holds %q", se.Name, sid, line)
			}
		}
		if !strings.Contains(env, "-MOOX_RESUME\n") {
			t.Errorf("session %q (%s) env has no -MOOX_RESUME marker:\n%s", se.Name, sid, env)
		}
		for _, r := range res.Plan.Rows {
			if r.Pane.Session != se.Name {
				continue
			}
			got := paneEnv(t, dump, res.Created[r.Key])["MOOX_RESUME"]
			if r.Resumes() {
				resumed++
				if got != r.Cmd || !strings.HasPrefix(got, "sleep 10") {
					t.Errorf("%s: MOOX_RESUME %q, want %q", r.Key, got, r.Cmd)
				}
			} else if got != "" {
				t.Errorf("sibling %s inherited MOOX_RESUME %q", r.Key, got)
			}
		}
		later := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{pane_id}"))
		if v := paneEnv(t, dump, later)["MOOX_RESUME"]; v != "" {
			t.Errorf("a user window in %q inherited MOOX_RESUME %q", se.Name, v)
		}
	}
	if resumed != len(names) {
		t.Fatalf("%d resumed rows, want %d", resumed, len(names))
	}
	if lv.Result != verdict.Fail || lv.States[res.Plan.Rows[0].Key] != verdict.StateNotFired {
		t.Fatalf("LAUNCH with /bin/sh panes must be NOT-FIRED: %+v", lv)
	}
}

// TestE2E_Restore_TitleWithFormatIsLiteral (B39, E2): titles, window names
// and a session name holding #(…), #S, #P, ## and #[style] come back byte
// for byte, and nothing runs.
func TestE2E_Restore_TitleWithFormatIsLiteral(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	touched := filepath.Join(t.TempDir(), "touched")
	// `\` included (I-4): tmux reports it doubled, and a restore that set the
	// reported form back would double it again.
	titles := []string{"#(touch " + touched + ")", "#S #P ## #{pane_id}", "#[fg=red]red ##[x]", "", "✳ Claude Code",
		`literal\backslash \\ #{x}\`}
	sid := newSession(t, s, "fmt #S ##", s.Home)
	wid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", sid, "#{window_id}"))
	s.Run(t, tmuxtarget.RenameWindow(wid, tmuxtarget.Literal("#(touch "+touched+".w) #W"))...)
	ids := []string{firstPane(t, s, sid)}
	for range titles[1:] {
		ids = append(ids, strings.TrimSpace(s.Run(t, "split-window", "-d", "-t", wid, "-P", "-F", "#{pane_id}")))
		s.Run(t, "select-layout", "-t", wid, "tiled")
	}
	for i, id := range ids {
		s.Run(t, tmuxtarget.SelectPaneTitle(id, tmuxtarget.Literal(titles[i]))...)
	}
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	for _, p := range saved.Panes {
		got := strings.TrimSuffix(s.Run(t, "display", "-p", "-t", res.Created[p.Key], "#{pane_title}"), "\n")
		if got != p.Title {
			t.Errorf("%s title %q, want %q", p.Key, got, p.Title)
		}
	}
	w := strings.TrimSuffix(s.Run(t, "display", "-p", "-t", res.Created[saved.Panes[0].Key], "#{window_name}"), "\n")
	if w != saved.Windows[0].Name {
		t.Errorf("window name %q, want %q", w, saved.Windows[0].Name)
	}
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "fmt #S ##" {
		t.Errorf("sessions %q", got)
	}
	for _, f := range []string{touched, touched + ".w"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s exists: a #(…) ran", f)
		}
	}
}

// TestE2E_Restore_SemicolonInsideValues (E5, Q5, PV14): `;` inside and at the
// end of titles, window names, cwds and resumed commands survives the chains.
func TestE2E_Restore_SemicolonInsideValues(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	e.timing.deadline = 1500 * time.Millisecond
	semi := mkdir(t, s, "dir; with ;")
	end := mkdir(t, s, "ends;")
	sid := newSession(t, s, "semi;", semi)
	wid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", sid, "#{window_id}"))
	// (a single command needs Chain too: a trailing `;` is a separator)
	ch := tmuxtarget.Chain
	s.Run(t, ch(tmuxtarget.RenameWindow(wid, tmuxtarget.Literal("fix; test;")))...)
	p0 := firstPane(t, s, sid)
	p1 := strings.TrimSpace(s.Run(t, ch(tmuxtarget.SplitWindow{Target: wid, Cwd: tmuxtarget.Literal(end),
		PrintFormat: "#{pane_id}"}.Args())...))
	s.Run(t, ch(tmuxtarget.SelectPaneTitle(p0, tmuxtarget.Literal("x;")))...)
	s.Run(t, ch(tmuxtarget.SelectPaneTitle(p1, tmuxtarget.Literal("a ; b;")))...)
	w2 := strings.TrimSpace(s.Run(t, ch(tmuxtarget.NewWindow{Target: sid + ":", Name: tmuxtarget.Literal(";"),
		Cwd: tmuxtarget.Literal(semi), PrintFormat: "#{window_id}"}.Args())...))
	s.Run(t, "set-option", "-w", "-t", w2, "automatic-rename", "off")
	record(t, e, p0, "sleep 1000; true")
	record(t, e, p1, "echo one;")
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	dump := envDump(t, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, false)
	for _, p := range listPanes(t, s) {
		var want *livePane
		for _, sp := range saved.Panes {
			if sp.Key == p.key {
				want = &livePane{sp.Key, "", sp.Cwd, sp.Title}
			}
		}
		if want == nil || p.title != want.title || p.cwd != want.cwd {
			t.Errorf("pane %+v, want %+v", p, want)
		}
	}
	names := s.Run(t, "list-windows", "-a", "-F", "#{window_name}")
	if names != "fix; test;\n;\n" {
		t.Errorf("window names %q", names)
	}
	for k, want := range map[string]string{saved.Panes[0].Key: "sleep 1000; true", saved.Panes[1].Key: "echo one;"} {
		if got := paneEnv(t, dump, res.Created[k])["MOOX_RESUME"]; got != want {
			t.Errorf("%s MOOX_RESUME %q, want %q", k, got, want)
		}
	}
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "semi;" {
		t.Errorf("sessions %q", got)
	}
}

// failingTmux fails the commands fail selects, for the failure-path tests.
type failingTmux struct {
	r    tmuxRunner
	fail func(args []string) bool
}

func (f failingTmux) Run(ctx context.Context, args ...string) ([]byte, error) {
	if f.fail(args) {
		return nil, errors.New("injected failure")
	}
	return f.r.Run(ctx, args...)
}

// TestE2E_Restore_EnvClearFailureKillsSession (R2-H1, fail closed): when
// exec A′ fails twice the session is killed, every row of it is a miss
// (BOOT INCOMPLETE) and the restore continues with the next session; the
// source is pinned (TestE2E_Restore_SessionCreateFailurePins).
func TestE2E_Restore_EnvClearFailureKillsSession(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	one := newSession(t, s, "one", s.Home)
	s.Run(t, "split-window", "-d", "-t", one)
	newSession(t, s, "two", s.Home)
	s.Run(t, "kill-session", "-t", "boot")
	saveNow(t, e, s)
	last, _ := e.Last()
	resetTo(t, s, "bootstrap")
	// every env clear after `new-session -s one` fails, until the next
	// new-session
	inOne := false
	e.tmux = failingTmux{r: s.T, fail: func(args []string) bool {
		if args[0] == "new-session" {
			inOne = strings.Contains(" "+strings.Join(args, " ")+" ", " -s one ")
			return false
		}
		return inOne && args[0] == "set-environment"
	}}
	res, lv := restoreNow(t, e, true)
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "two" {
		t.Fatalf("sessions %q: want only `two` (one killed fail-closed, bootstrap dropped)", got)
	}
	if res.Boot.Result != verdict.Incomplete || res.Boot.Misses != 2 {
		t.Fatalf("BOOT %+v (want INCOMPLETE with the 2 rows of `one` missed)", res.Boot)
	}
	if lv.Result != verdict.Pass {
		t.Fatalf("LAUNCH %+v", lv)
	}
	st, err := store.Open(e.Paths(), filepath.Join(s.Home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pins, _ := st.Pins()
	if len(pins) != 1 || pins[0] != last.Name {
		t.Fatalf("pins %v, want [%s]", pins, last.Name)
	}
	if w := s.Opt(t, "@moox-warning"); !strings.Contains(w, "1 pinned") {
		t.Fatalf("@moox-warning %q", w)
	}
	if w := s.Opt(t, "@claude-continuity-boot-warning"); w == "" {
		t.Fatal("INCOMPLETE did not set @claude-continuity-boot-warning")
	}
}

// TestE2E_Restore_WindowSizesFromLayout (R3-C1, Q10): detached, every window
// gets its saved layout root's size from select-layout — 1-pane windows
// included — and window-size is never manual; then a client... (attach is
// covered by the nested-client test in P5's harness).
func TestE2E_Restore_WindowSizesFromLayout(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(220, 60))
	e := e2eEngine(t, s)
	sid := newSession(t, s, "sizes", s.Home)
	s.Run(t, "resize-window", "-t", sid+":0", "-x", "160", "-y", "40")
	s.Run(t, "split-window", "-d", "-h", "-t", sid+":0")
	w1 := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_id}"))
	s.Run(t, "resize-window", "-t", w1, "-x", "120", "-y", "30")
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	for _, w := range saved.Windows {
		var pane string
		for _, p := range saved.Panes {
			if p.Session == w.Session && p.Window == w.Index {
				pane = res.Created[p.Key]
			}
		}
		got := strings.TrimSpace(s.Run(t, "display", "-p", "-t", pane, "#{window_width}x#{window_height}"))
		root, _, err := layout.Parse(w.Layout)
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.Itoa(root.W) + "x" + strconv.Itoa(root.H); got != want {
			t.Errorf("window %d: %s, want the layout root's %s", w.Index, got, want)
		}
		wid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", pane, "#{window_id}"))
		if ws := strings.TrimSpace(s.Run(t, "show-options", "-wqv", "-t", wid, "window-size")); ws == "manual" {
			t.Errorf("window %d: window-size is manual (resize-window was used)", w.Index)
		}
	}
	// T11/PV4: default-size unset on the session, so a later window gets the
	// global size (TestE2E_Restore_DefaultSizeUnset).
	rsid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[saved.Panes[0].Key], "#{session_id}"))
	if v := strings.TrimSpace(s.Run(t, "show-options", "-qv", "-t", rsid, "default-size")); v != "" {
		t.Errorf("session default-size still %q", v)
	}
}

// TestE2E_Restore_ZoomAfterLayout (T10): the active pane is selected first and
// the window zoomed after the layout; the active pane of every window is the
// saved one (TestE2E_Restore_ActivePane).
func TestE2E_Restore_ZoomAfterLayout(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	sid := newSession(t, s, "zoom", s.Home)
	p1 := strings.TrimSpace(s.Run(t, "split-window", "-d", "-t", sid+":0", "-P", "-F", "#{pane_id}"))
	s.Run(t, "split-window", "-d", "-t", sid+":0")
	s.Run(t, "select-pane", "-t", p1)
	s.Run(t, "resize-pane", "-Z", "-t", p1)
	w1 := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_id}"))
	p3 := strings.TrimSpace(s.Run(t, "split-window", "-d", "-t", w1, "-P", "-F", "#{pane_id}"))
	s.Run(t, "select-pane", "-t", p3)
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	for _, p := range saved.Panes {
		got := strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[p.Key], "#{pane_active} #{window_zoomed_flag}"))
		var zoomed bool
		for _, w := range saved.Windows {
			if w.Index == p.Window {
				zoomed = w.Zoomed
			}
		}
		want := map[bool]string{true: "1", false: "0"}[p.Active] + " " + map[bool]string{true: "1", false: "0"}[zoomed]
		if got != want {
			t.Errorf("%s active/zoomed %q, want %q", p.Key, got, want)
		}
	}
}

// TestE2E_Restore_AutomaticRenameTriState (T6): on, off and unset come back as
// the window-local option they were.
func TestE2E_Restore_AutomaticRenameTriState(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	sid := newSession(t, s, "auto", s.Home)
	w1 := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_id}"))
	w2 := strings.TrimSpace(s.Run(t, "new-window", "-d", "-t", sid+":", "-P", "-F", "#{window_id}"))
	s.Run(t, "set-option", "-g", "automatic-rename", "on")
	s.Run(t, "set-option", "-w", "-t", sid+":0", "automatic-rename", "on")
	s.Run(t, tmuxtarget.RenameWindow(w1, tmuxtarget.Literal("named"))...) // sets it off locally
	s.Run(t, "set-option", "-wu", "-t", w2, "automatic-rename")
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	// I-5: window 0 sets `on` itself, equal to the global, and must save so.
	for _, w := range saved.Windows {
		if want := map[int]string{0: "on", 1: "off", 2: "unset"}[w.Index]; w.AutomaticRename != want {
			t.Errorf("window %d saved automatic_rename %q, want %q", w.Index, w.AutomaticRename, want)
		}
	}
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	for _, w := range saved.Windows {
		var pane string
		for _, p := range saved.Panes {
			if p.Window == w.Index {
				pane = res.Created[p.Key]
			}
		}
		wid := strings.TrimSpace(s.Run(t, "display", "-p", "-t", pane, "#{window_id}"))
		local := strings.TrimSpace(s.Run(t, "show-options", "-wqv", "-t", wid, "automatic-rename"))
		want := map[string]string{"on": "on", "off": "off", "unset": ""}[w.AutomaticRename]
		if local != want {
			t.Errorf("window %d (%s): local automatic-rename %q, want %q", w.Index, w.AutomaticRename, local, want)
		}
	}
}

// TestE2E_Restore_NoClientSetsTarget (T11, RR16): with no client attached the
// restore sets @moox-restore-target to the client session's $id.
func TestE2E_Restore_NoClientSetsTarget(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	newSession(t, s, "first", s.Home)
	newSession(t, s, "second", s.Home)
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, true)
	want := ""
	for _, p := range saved.Panes {
		if p.Session == saved.Client.Session {
			want = strings.TrimSpace(s.Run(t, "display", "-p", "-t", res.Created[p.Key], "#{session_id}"))
		}
	}
	if got := s.Opt(t, "@moox-restore-target"); got == "" || got != want {
		t.Fatalf("@moox-restore-target %q, want %q (client session %q)", got, want, saved.Client.Session)
	}
}

// TestE2E_Restore_DuplicateTitlesDistinctPanes (B35, R5): two panes with the
// same title and cwd each get their own command — the direct id map has no
// resolver to confuse.
func TestE2E_Restore_DuplicateTitlesDistinctPanes(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	e := e2eEngine(t, s)
	e.timing.deadline = 1500 * time.Millisecond
	sid := newSession(t, s, "dups", s.Home)
	a := firstPane(t, s, sid)
	b := strings.TrimSpace(s.Run(t, "split-window", "-d", "-t", sid+":0", "-P", "-F", "#{pane_id}"))
	for _, id := range []string{a, b} {
		s.Run(t, tmuxtarget.SelectPaneTitle(id, tmuxtarget.Literal("✳ Claude Code"))...)
	}
	record(t, e, a, "sleep 1111")
	record(t, e, b, "sleep 2222")
	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	dump := envDump(t, s)
	resetTo(t, s, "bootstrap")
	res, _ := restoreNow(t, e, false)
	for _, p := range saved.Panes {
		want := map[string]string{a: "sleep 1111", b: "sleep 2222"}[p.SavedID]
		if got := paneEnv(t, dump, res.Created[p.Key])["MOOX_RESUME"]; got != want {
			t.Errorf("%s (saved %s): MOOX_RESUME %q, want %q", p.Key, p.SavedID, got, want)
		}
	}
	_ = relaunch.KindRecording
}
