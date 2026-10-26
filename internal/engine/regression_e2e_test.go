//go:build tmuxe2e

package engine

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

var stubLine = regexp.MustCompile(`MOOX-STUB ([A-Za-z0-9+/=]+)`)

// stubPayload is the original command a stub pane printed.
func stubPayload(t *testing.T, s *tmuxtest.Server, pane string) string {
	t.Helper()
	var m []string
	tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool {
		m = stubLine.FindStringSubmatch(s.Run(t, "capture-pane", "-p", "-J", "-S", "-", "-t", pane))
		return m != nil
	})
	if m == nil {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("pane %s: stub payload %q: %v", pane, m[1], err)
	}
	return string(b)
}

// sleepAs is a pane command that runs `sleep <args>` with argv[0] name — a
// fake Claude ("claude") or helper. `exec -a` is a bash builtin: macOS's
// /bin/sh is bash and has it, Linux's dash does not ("exec: -a: not found"),
// so it is spelled through bash on every platform. Both execs keep the
// process tree the bare `exec -a` form had.
func sleepAs(name, args string) string {
	return "exec bash -c 'exec -a " + name + " sleep " + args + "'"
}

// claudeUnder finds the pid of the process with argv[0] "claude" at or under
// pid (the registry is keyed by the Claude process's own pid).
func claudeUnder(t *testing.T, e *Engine, pid int) int {
	t.Helper()
	var found int
	tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		out, err := e.procs.Table(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		procs := capture.ParsePS(out).Procs
		var vp []verdict.Proc
		for _, p := range procs {
			vp = append(vp, verdict.Proc{Pid: p.Pid, PPid: p.PPid, Command: p.Command})
		}
		under := map[int]bool{pid: true}
		for changed := true; changed; {
			changed = false
			for _, p := range vp {
				if under[p.PPid] && !under[p.Pid] {
					under[p.Pid], changed = true, true
				}
			}
		}
		for _, p := range vp {
			if under[p.Pid] && strings.HasPrefix(p.Command, "claude ") {
				found = p.Pid
				return true
			}
		}
		return false
	})
	if found == 0 {
		t.Fatalf("no claude process under pid %d", pid)
	}
	return found
}

// TestRegression_HistoricalRestoreBugs runs each of the memory note's
// historical restore bugs (tmux-claude-continuity-resume-mechanism.md)
// through Save → Restore on ONE isolated server with HookZsh panes and the
// real gate, stubbed, and checks the command each restored pane received:
//
//	R6 window-renumber drift   R3 depth-0 capture       R30 hook not ready
//	R4 the MCP-child filter    R5 the dedup subshell    R7 the stale sidecar
//	R2 the wrapper depth       R1 the `wt` launcher grammar
//
// (R8 is a sidmap invariant, not one of them: TestSidmap_OneSessionOnePane_StableAcrossSaves.)
func TestRegression_HistoricalRestoreBugs(t *testing.T) {
	// StartCold: the real config and gate (MOOX_* for the hooks, C1); no
	// snapshot yet, so the boot gate skips and the daemon only observes.
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithShell(tmuxtest.HookZsh(t)), tmuxtest.WithSize(200, 50))
	if s.Opt(t, "@moox-active") != "1" || s.Opt(t, "@moox-boot-result") != "SKIPPED:no-snapshot" {
		t.Fatalf("gate: active %q boot-result %q", s.Opt(t, "@moox-active"), s.Opt(t, "@moox-boot-result"))
	}
	e := e2eEngine(t, s)
	ch := tmuxtarget.Chain
	run := func(args ...string) string { return strings.TrimSpace(s.Run(t, ch(args)...)) }
	byPid := filepath.Join(s.Home, ".config", "tmux-claude", "panes", "by-pid")
	if err := os.MkdirAll(byPid, 0o700); err != nil {
		t.Fatal(err)
	}
	register := func(pid int, sid string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(byPid, strconv.Itoa(pid)+".session-id"), []byte(sid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	panePid := func(pane string) int {
		n, _ := strconv.Atoi(run("display", "-p", "-t", pane, "#{pane_pid}"))
		return n
	}
	fakeClaude := sleepAs("claude", "1000")
	want := map[string]string{} // saved pane id → the command its restore must get
	newS := func(name, cmd string) (sid, pane string) {
		sid = strings.TrimSpace(s.Run(t, ch(tmuxtarget.NewSession{Name: tmuxtarget.Literal(name), Cwd: tmuxtarget.Literal(s.Home),
			PrintFormat: "#{session_id}", Command: []string{cmd}}.Args())...))
		return sid, run("display", "-p", "-t", sid, "#{pane_id}")
	}

	// R6: windows at non-contiguous indices, one Claude each.
	r6, r6p := newS("r6", fakeClaude)
	register(claudeUnder(t, e, panePid(r6p)), "sid-r6-first")
	want[r6p] = "claude 1000 --resume sid-r6-first"
	for _, idx := range []int{4, 9} {
		p := run(tmuxtarget.NewWindow{Target: r6 + ":" + strconv.Itoa(idx), PrintFormat: "#{pane_id}", Command: []string{fakeClaude}}.Args()...)
		sid := "sid-r6-" + strconv.Itoa(idx)
		register(claudeUnder(t, e, panePid(p)), sid)
		want[p] = "claude 1000 --resume " + sid
	}
	// R3: Claude exec'd as the pane process (depth 0).
	_, r3p := newS("r3", fakeClaude)
	register(panePid(r3p), "sid-r3")
	want[r3p] = "claude 1000 --resume sid-r3"
	// R30: a recording consumed by a shell whose hook comes up after the
	// restore created the pane (C2: env, no pending file, no nudge).
	_, r30p := newS("r30", tmuxtest.PaneShell)
	record(t, e, r30p, "sleep 3030")
	want[r30p] = "sleep 3030"
	// R4: the pane's depth-1 child is an MCP helper; the sid stays
	// authoritative (full_command alone never disqualifies, B31).
	_, r4p := newS("r4", "bash -c 'exec -a mcp-server sleep 1000' & "+sleepAs("claude", "1000"))
	register(claudeUnder(t, e, panePid(r4p)), "sid-r4")
	want[r4p] = "claude --resume sid-r4"
	// R5: two Claudes with the same title in one window, distinct sids.
	r5, r5a := newS("r5", fakeClaude)
	r5b := run(tmuxtarget.SplitWindow{Target: r5 + ":", PrintFormat: "#{pane_id}", Command: []string{fakeClaude}}.Args()...)
	for i, p := range []string{r5a, r5b} {
		run(tmuxtarget.SelectPaneTitle(p, tmuxtarget.Literal("✳ Claude Code"))...)
		sid := "sid-r5-" + strconv.Itoa(i)
		register(claudeUnder(t, e, panePid(p)), sid)
		want[p] = "claude 1000 --resume " + sid
	}
	// R7: a stale position sidecar names another sid; the registry wins and
	// the sidecar is never read.
	_, r7p := newS("r7", fakeClaude)
	register(claudeUnder(t, e, panePid(r7p)), "sid-r7-registry")
	if err := os.MkdirAll(e.Paths().Panes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Paths().Panes, "r7-1-1.session-id"), []byte("sid-r7-stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want[r7p] = "claude 1000 --resume sid-r7-registry"
	// R2: Claude two wrapper levels below the pane process.
	_, r2p := newS("r2", `/bin/sh -c '/bin/sh -c "exec bash -c \"exec -a claude sleep 1000\""; :'; :`)
	register(claudeUnder(t, e, panePid(r2p)), "sid-r2")
	want[r2p] = "claude --resume sid-r2"
	// R1: the `wt` launcher: a typed recording plus a sid gets --resume
	// appended to the recording, never a plain claude.
	_, r1p := newS("r1", fakeClaude)
	register(claudeUnder(t, e, panePid(r1p)), "sid-r1")
	record(t, e, r1p, "wt foo red")
	want[r1p] = "wt foo red --resume sid-r1"

	s.Run(t, "kill-session", "-t", "boot")
	saved := saveNow(t, e, s)
	if got := len(saved.Sids()); got != 10 {
		t.Fatalf("saved %d sids, want 10", got)
	}
	resetTo(t, s, "bootstrap")
	res, lv := restoreNow(t, e, true)
	if res.Boot.Result != verdict.Pass || lv.Result != verdict.Pass {
		t.Errorf("BOOT %s (%d/%d, misses %v) LAUNCH %s %v", res.Boot.Result, res.Boot.Written, res.Boot.Resumable,
			res.Boot.MissKeys, lv.Result, lv.States)
	}
	for _, p := range saved.Panes {
		w, ok := want[p.SavedID]
		if !ok {
			continue
		}
		pane := res.Created[p.Key]
		if pane == "" {
			t.Errorf("%s (saved %s): not created", p.Key, p.SavedID)
			continue
		}
		where := strings.TrimSpace(s.Run(t, "display", "-p", "-t", pane, "#S:#I"))
		if where != p.Session+":"+strconv.Itoa(p.Window) {
			t.Errorf("%s landed in %s (window-renumber drift, R6)", p.Key, where)
		}
		if got := stubPayload(t, s, pane); got != w {
			t.Errorf("%s (saved %s): restored with %q, want %q", p.Key, p.SavedID, got, w)
		}
	}
}
