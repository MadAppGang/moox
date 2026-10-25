package verdict

import "testing"

// jp is a ps row with its job-control columns.
func jp(pid, ppid int, cmd string, pgid, tpgid int) Proc {
	return Proc{Pid: pid, PPid: ppid, Command: cmd, Job: true, Pgid: pgid, TPgid: tpgid}
}

// TestLaunchVerdict_ExitedWithBackgroundHelper (iter-2 real-zsh bug): a real
// zsh with powerlevel10k keeps gitstatusd as a child of the idle shell, in a
// process group of its own. When Claude quits, the shell holds its terminal's
// foreground again: that is EXITED (Observe, and the Settle re-judgement of an
// OK row), and the verdict FAILs. A foreground job, or Claude itself, is not.
func TestLaunchVerdict_ExitedWithBackgroundHelper(t *testing.T) {
	const sh = 50
	helper := jp(sh+1, sh, "gitstatusd -s -1", sh+1, sh)
	idle := []Proc{jp(sh, 1, "zsh", sh, sh), helper}
	claude := []Proc{jp(sh, 1, "zsh", sh, sh+2), helper, jp(sh+2, sh, "claude --resume x", sh+2, sh+2)}
	panes := map[string]PaneState{"%5": {Pid: sh, Current: "zsh"}}
	acks := map[string]bool{"c": true, "t": true}
	claudeRow := []LaunchRow{{Key: "c", Pane: "%5", Claude: true}}

	// Observe: two bare probes → EXITED → FAIL
	j := NewJudge(claudeRow)
	j.Observe(probe(acks, panes, idle...))
	if !j.Observe(probe(acks, panes, idle...)) {
		t.Fatalf("not decided: %+v", j.Verdict())
	}
	if l := j.Verdict(); l.States["c"] != StateExited || l.Result != Fail {
		t.Fatalf("Claude quit, the helper left: %+v", l)
	}

	// Settle: OK, then Claude quits during the settle → EXITED → FAIL
	s := NewJudge(claudeRow)
	s.Observe(probe(acks, panes, claude...))
	s.Settle(probe(acks, panes, idle...))
	if l := s.Verdict(); l.States["c"] != StateExited || l.Result != Fail {
		t.Fatalf("settle over a quit Claude with a helper: %+v", l)
	}

	// Claude still running is OK, whatever helpers exist
	o := NewJudge(claudeRow)
	o.Observe(probe(acks, panes, claude...))
	o.Settle(probe(acks, panes, claude...))
	if l := o.Verdict(); l.States["c"] != StateOK || l.Result != Pass {
		t.Fatalf("Claude running: %+v", l)
	}

	// a non-Claude row whose command quit: EXITED, a warning, not STALLED
	n := NewJudge([]LaunchRow{{Key: "t", Pane: "%5", Token: "make"}})
	n.Observe(probe(acks, panes, idle...))
	n.Observe(probe(acks, panes, idle...))
	if l := n.Verdict(); l.States["t"] != StateExited || l.Result != Pass || l.Warnings != 1 {
		t.Fatalf("non-Claude quit, the helper left: %+v", l)
	}

	// another foreground job is never bare: STALLED at the deadline
	f := NewJudge(claudeRow)
	busy := []Proc{jp(sh, 1, "zsh", sh, sh+3), helper, jp(sh+3, sh, "vim notes", sh+3, sh+3)}
	f.Observe(probe(acks, panes, busy...))
	f.Observe(probe(acks, panes, busy...))
	f.Deadline(probe(acks, panes, busy...))
	if l := f.Verdict(); l.States["c"] != StateStalled {
		t.Fatalf("a foreground vim: %+v", l)
	}
}

// TestForegroundBusy_Fallbacks: without job columns it is the old any-child
// rule; NeedsJobs asks for the job read only for a shell pane with a child.
func TestForegroundBusy_Fallbacks(t *testing.T) {
	const sh = 50
	plain := []Proc{{Pid: sh, PPid: 1, Command: "zsh"}, {Pid: sh + 1, PPid: sh, Command: "helper"}}
	if ForegroundBusy(plain, sh) == "" {
		t.Fatal("without job columns a child must stay busy")
	}
	if ForegroundBusy([]Proc{jp(sh, 1, "zsh", sh, sh), {Pid: sh + 1, PPid: sh, Command: "new"}}, sh) == "" {
		t.Fatal("a child with unknown job columns must stay busy")
	}
	if ForegroundBusy(plain[:1], sh) != "" {
		t.Fatal("a childless shell is idle")
	}
	shellPane := []PaneState{{Pid: sh, Current: "zsh"}}
	if NeedsJobs(shellPane, plain[:1]) {
		t.Fatal("a childless shell needs no job read")
	}
	if !NeedsJobs(shellPane, plain) {
		t.Fatal("a shell with a child needs the job read")
	}
	if NeedsJobs([]PaneState{{Pid: sh, Current: "claude"}}, plain) {
		t.Fatal("a non-shell pane needs no job read")
	}
}
