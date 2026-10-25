package verdict

import (
	"strings"
	"testing"
)

func rows(spec ...string) []Row {
	// "key:c" considered, ":r" resumes, ":cr" both
	var out []Row
	for _, s := range spec {
		k, f, _ := strings.Cut(s, ":")
		out = append(out, Row{Key: k, Considered: strings.Contains(f, "c"), Resume: strings.Contains(f, "r")})
	}
	return out
}

func all(rs []Row) map[string]string {
	m := map[string]string{}
	for i, r := range rs {
		m[r.Key] = "%" + string(rune('0'+i))
	}
	return m
}

// TestBootVerdict_Accounting (B41, R26): considered=0, the denominator, a
// FROZEN-ORPHAN miss, a CLAIM-FAILED warning, all-frozen = NOTHING, a missing
// banner changes nothing, a fail-closed session's rows are misses, and there
// is no `absent`.
func TestBootVerdict_Accounting(t *testing.T) {
	cases := []struct {
		name     string
		rows     []Row
		created  func([]Row) map[string]string
		orphans  []string
		warnings []string
		want     string
		misses   int
		written  int
	}{
		{"nothing considered", rows("a:", "b:"), all, nil, nil, Nothing, 0, 0},
		{"all resumed", rows("a:cr", "b:cr", "c:"), all, nil, nil, Pass, 0, 2},
		{"considered but not written (3f)", rows("a:cr", "b:c"), all, nil, nil, Incomplete, 0, 1},
		{"frozen orphan is a miss", rows("a:cr", "t:"), all, []string{"t FROZEN-ORPHAN"}, nil, Incomplete, 1, 1},
		{"claim-failed is a warning", rows("a:cr", "t:"), all, nil, []string{"FROZEN-CLAIM-FAILED t"}, Pass, 0, 1},
		{"all frozen, banner or not", rows("t1:", "t2:"), all, nil, nil, Nothing, 0, 0},
		{"a fail-closed session: its rows have no pane", rows("a:cr", "b:", "c:"),
			func([]Row) map[string]string { return map[string]string{"a": "%1"} }, nil, nil, Incomplete, 2, 1},
		{"no absent: a planned shell row without a pane is a loss", rows("a:", "b:"),
			func([]Row) map[string]string { return map[string]string{"a": "%1"} }, nil, nil, Incomplete, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := ComputeBoot(c.rows, c.created(c.rows), c.orphans, c.warnings)
			if b.Result != c.want || b.Misses != c.misses || b.Written != c.written || b.Resumable != b.Considered {
				t.Fatalf("%+v, want %s misses=%d written=%d", b, c.want, c.misses, c.written)
			}
			if c.warnings != nil && (b.Warnings != len(c.warnings) || !strings.Contains(b.Line(), "PASS (1 warnings)")) {
				t.Fatalf("warnings: %+v %q", b, b.Line())
			}
			if strings.Contains(b.Line(), "absent") {
				t.Fatalf("a BOOT line never says absent: %q", b.Line())
			}
		})
	}
}

// TestComputeBoot_PlanMinusCreated: misses are plan minus Created, whatever
// the reason a row has no pane (the executor-level tests are the engine's
// TestBootVerdict_SessionCreateFailureIsMiss and _UnattemptedSessionsAreMisses).
func TestComputeBoot_PlanMinusCreated(t *testing.T) {
	rs := []Row{{Key: "a:1.1", Considered: true, Resume: true}, {Key: "b:1.1"}, {Key: "b:1.2", Considered: true, Resume: true}, {Key: "c:1.1"}}
	b := ComputeBoot(rs, map[string]string{"a:1.1": "%1"}, nil, nil)
	if b.Result != Incomplete || b.Misses != 3 || b.Written != 1 || b.Resumable != 2 {
		t.Fatalf("%+v", b)
	}
}

func probe(acks map[string]bool, panes map[string]PaneState, procs ...Proc) Probe {
	return Probe{Panes: panes, Procs: procs, Acks: acks}
}

// TestLaunchVerdict_States (B42, R27): OK / EXITED / NOT-FIRED / STALLED /
// GONE per kind, decided from successive probes.
func TestLaunchVerdict_States(t *testing.T) {
	rs := []LaunchRow{
		{Key: "ok-claude", Pane: "%1", Claude: true},
		{Key: "exited-claude", Pane: "%2", Claude: true},
		{Key: "not-fired", Pane: "%3", Claude: true},
		{Key: "stalled", Pane: "%4", Claude: true},
		{Key: "gone", Pane: "%5", Claude: true},
		{Key: "exited-shell-cmd", Pane: "%6", Token: "make"},
	}
	j := NewJudge(rs)
	acks := map[string]bool{"ok-claude": true, "exited-claude": true, "stalled": true, "gone": true, "exited-shell-cmd": true}
	panes := map[string]PaneState{
		"%1": {Pid: 10, Current: "claude"}, "%2": {Pid: 20, Current: "zsh"}, "%3": {Pid: 30, Current: "zsh"},
		"%4": {Pid: 40, Current: "node"}, "%6": {Pid: 60, Current: "bash"},
	}
	procs := []Proc{{Pid: 10, PPid: 1, Command: "zsh"}, {Pid: 11, PPid: 10, Command: "claude --resume x"}, {Pid: 20, PPid: 1, Command: "-zsh"}, {Pid: 30, PPid: 1, Command: "zsh"}, {Pid: 40, PPid: 1, Command: "zsh"},
		{Pid: 41, PPid: 40, Command: "node wrapper.js"}, {Pid: 60, PPid: 1, Command: "bash"}}
	p := probe(acks, panes, procs...)
	if j.Observe(p) {
		t.Fatal("decided after one probe: EXITED needs two consecutive bare probes")
	}
	if j.Observe(p) {
		t.Fatal("not-fired and stalled cannot decide before the deadline")
	}
	j.Deadline(p)
	l := j.Verdict()
	want := map[string]string{"ok-claude": StateOK, "exited-claude": StateExited, "not-fired": StateNotFired,
		"stalled": StateStalled, "gone": StateGone, "exited-shell-cmd": StateExited}
	for k, w := range want {
		if l.States[k] != w {
			t.Errorf("%s: %s, want %s", k, l.States[k], w)
		}
	}
	if l.Result != Fail || l.Warnings != 1 || l.Failures != 4 {
		t.Fatalf("%+v", l)
	}
	if !strings.Contains(l.Line(), "LAUNCH VERDICT: FAIL") {
		t.Fatal(l.Line())
	}
}

// TestLaunchVerdict_NonClaudeOKEarly: a healthy sleep, tail or nvim is decided
// on the first probe that sees it, so it never holds suppression to the
// deadline; zero judged rows is PASS.
func TestLaunchVerdict_NonClaudeOKEarly(t *testing.T) {
	rs := []LaunchRow{{Key: "tail", Pane: "%1", Token: "tail"}, {Key: "nvim", Pane: "%2", Token: "nvim"}}
	j := NewJudge(rs)
	p := probe(map[string]bool{"tail": true, "nvim": true}, map[string]PaneState{
		"%1": {Pid: 10, Current: "tail"}, "%2": {Pid: 20, Current: "zsh"},
	}, Proc{Pid: 10, PPid: 1, Command: "tail -f log"}, Proc{Pid: 20, PPid: 1, Command: "zsh"}, Proc{Pid: 21, PPid: 20, Command: "/opt/homebrew/bin/nvim -S"})
	if !j.Observe(p) {
		t.Fatalf("not decided on the first probe: %+v", j.Verdict())
	}
	if l := j.Verdict(); l.Result != Pass {
		t.Fatalf("%+v", l)
	}
	if l := NewJudge(nil).Verdict(); l.Result != Pass {
		t.Fatalf("zero judged rows: %+v", l)
	}
	// a Claude row that is OK and then exits during the settle is EXITED
	c := NewJudge([]LaunchRow{{Key: "c", Pane: "%9", Claude: true}})
	c.Observe(probe(map[string]bool{"c": true}, map[string]PaneState{"%9": {Pid: 90, Current: "claude"}}, Proc{Pid: 90, PPid: 1, Command: "claude 600"}))
	c.Settle(probe(map[string]bool{"c": true}, map[string]PaneState{"%9": {Pid: 90, Current: "zsh"}}, Proc{Pid: 90, PPid: 1, Command: "zsh"}))
	if l := c.Verdict(); l.States["c"] != StateExited || l.Result != Fail {
		t.Fatalf("%+v", l)
	}
}

func TestParsePanesAndPS(t *testing.T) {
	m := ParsePanes("%1 100 0 zsh\n%2 200 1 \nbad\n")
	if m["%1"].Pid != 100 || m["%1"].Current != "zsh" || !m["%2"].Dead {
		t.Fatalf("%+v", m)
	}
	ps := ParsePS("  10   1 /bin/zsh -l\n 11 10 claude --resume x\n")
	if len(ps) != 2 || ps[1].Command != "claude --resume x" {
		t.Fatalf("%+v", ps)
	}
	if !IsShell("-zsh") || !IsShell("/bin/bash") || IsShell("claude") {
		t.Fatal("IsShell")
	}
}
