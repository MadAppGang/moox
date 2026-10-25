// Package verdict is the restore's two judgements (architecture §6.5), kept
// pure so every accounting rule is a table test:
//
//   - BOOT (B41, R26): what the restore CREATED, computed as plan minus
//     Created — never from error paths, so no failure mode can fall out of
//     the count. There is no `absent` (R3-H1): moox creates every session
//     itself, so a planned row without a pane is a loss.
//   - LAUNCH (B42, R27): what came BACK, judged per row by launch kind from
//     probes (one ps, one list-panes, the ack directory) that the engine's
//     bounded probe chain feeds in.
package verdict

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Results.
const (
	Pass       = "PASS"
	Incomplete = "INCOMPLETE"
	Nothing    = "NOTHING"
	Fail       = "FAIL"
)

// Row is one planned row as the BOOT accounting sees it.
type Row struct {
	Key string
	// Considered: the row qualified (B30). Frozen rows are never considered
	// (R2-H8): an all-frozen workspace is NOTHING TO RESUME.
	Considered bool
	// Resume: the row's creating command carries MOOX_RESUME.
	Resume bool
}

// Boot is the BOOT verdict.
type Boot struct {
	Result     string
	Considered int
	Resumable  int
	Written    int
	Misses     int
	Warnings   int
	// MissKeys are the rows (and FROZEN-ORPHAN keys) counted as misses.
	MissKeys []string
	// WarningLines are FROZEN-CLAIM-FAILED reclaims and LAYOUT-* fallbacks.
	WarningLines []string
}

// ComputeBoot is the BOOT verdict. created maps row key → the pane id its
// creating command printed. orphans are FROZEN-ORPHAN reclaim lines (misses),
// warnings are FROZEN-CLAIM-FAILED and LAYOUT-* lines.
func ComputeBoot(rows []Row, created map[string]string, orphans, warnings []string) Boot {
	b := Boot{WarningLines: append([]string(nil), warnings...)}
	for _, r := range rows {
		_, made := created[r.Key]
		if r.Considered {
			b.Considered++
		}
		if r.Resume && made {
			b.Written++
		}
		if !made {
			b.MissKeys = append(b.MissKeys, r.Key)
		}
	}
	b.MissKeys = append(b.MissKeys, orphans...)
	b.Misses = len(b.MissKeys)
	b.Resumable = b.Considered
	b.Warnings = len(b.WarningLines)
	switch {
	case b.Misses > 0 || b.Written < b.Resumable:
		b.Result = Incomplete
	case b.Considered == 0:
		b.Result = Nothing
	default:
		b.Result = Pass
	}
	return b
}

// Line is the daemon.log line.
func (b Boot) Line() string {
	head := b.Result
	switch {
	case b.Result == Nothing:
		head = "NOTHING TO RESUME"
	case b.Result == Pass && b.Warnings > 0:
		head = fmt.Sprintf("PASS (%d warnings)", b.Warnings)
	}
	s := fmt.Sprintf("BOOT VERDICT: %s — written %d/%d resumable, %d considered, %d misses, %d warnings",
		head, b.Written, b.Resumable, b.Considered, b.Misses, b.Warnings)
	if len(b.MissKeys) > 0 {
		s += " [missed: " + strings.Join(b.MissKeys, ", ") + "]"
	}
	return s
}

// ---------------------------------------------------------------------------
// LAUNCH
// ---------------------------------------------------------------------------

// Per-row LAUNCH states.
const (
	StateOK       = "OK"
	StateExited   = "EXITED"
	StateNotFired = "NOT-FIRED"
	StateStalled  = "STALLED"
	StateGone     = "GONE"
)

// LaunchRow is one judged row: a row whose creating command carried
// MOOX_RESUME. layout, skip and frozen-banner rows are never judged.
type LaunchRow struct {
	Key    string
	Pane   string // the pane id the creating command printed
	Claude bool
	// Token is the exec token's basename of the ORIGINAL command (the stub
	// keeps the original for the plan and verdicts, §6.5): a non-Claude row
	// is OK when this token runs under the pane.
	Token string
}

// Proc is one ps row. Job is set when the job-control columns were read for
// it: Pgid, TPgid (its terminal's foreground process group; ≤ 0 without a
// terminal) and Stopped (a job-control stop, ^Z).
type Proc struct {
	Pid, PPid int
	Command   string
	Job       bool
	Pgid      int
	TPgid     int
	Stopped   bool
}

// PaneState is one list-panes row.
type PaneState struct {
	Pid     int
	Dead    bool
	Current string // pane_current_command
}

// Probe is one probe's observation.
type Probe struct {
	Panes map[string]PaneState
	Procs []Proc
	Acks  map[string]bool // row key → ack file present
}

// ParsePanes parses `list-panes -a -F '#{pane_id} #{pane_pid} #{pane_dead} #{pane_current_command}'`.
func ParsePanes(out string) map[string]PaneState {
	m := map[string]PaneState{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), " ", 4)
		if len(f) < 3 || !strings.HasPrefix(f[0], "%") {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		st := PaneState{Pid: pid, Dead: f[2] == "1"}
		if len(f) == 4 {
			st.Current = f[3]
		}
		m[f[0]] = st
	}
	return m
}

// ParsePS parses `ps -axo pid=,ppid=,command=`.
func ParsePS(out string) []Proc {
	var ps []Proc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		ps = append(ps, Proc{Pid: pid, PPid: ppid, Command: strings.Join(f[2:], " ")})
	}
	return ps
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"tcsh": true, "csh": true, "mksh": true, "ash": true}

// IsShell reports whether a pane_current_command (or exec basename) is a
// login or interactive shell.
func IsShell(cmd string) bool {
	return shells[strings.TrimPrefix(path.Base(strings.TrimSpace(cmd)), "-")]
}

// execBase is the first token's basename (the process's own argv[0]).
func execBase(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return strings.TrimPrefix(path.Base(f[0]), "-")
}

// subtree is pid and every descendant of it.
func subtree(procs []Proc, root int) []Proc {
	kids := map[int][]Proc{}
	var self *Proc
	for i := range procs {
		kids[procs[i].PPid] = append(kids[procs[i].PPid], procs[i])
		if procs[i].Pid == root {
			self = &procs[i]
		}
	}
	var out []Proc
	if self != nil {
		out = append(out, *self)
	}
	queue := []int{root}
	for len(queue) > 0 && len(out) < 4096 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range kids[p] {
			out = append(out, c)
			queue = append(queue, c.Pid)
		}
	}
	return out
}

func hasChildren(procs []Proc, pid int) bool {
	for _, p := range procs {
		if p.PPid == pid {
			return true
		}
	}
	return false
}

// ForegroundBusy says why the shell with pid shell is not idle at its
// prompt, or "" when it is. Idle is "the shell holds its terminal's
// foreground process group", not "the shell has no child": an idle zsh keeps
// helpers as children (powerlevel10k's gitstatusd, async prompt workers) in
// process groups of their own. Busy is:
//   - the terminal's foreground group is another group (a job-control shell
//     runs every foreground job in a group of its own);
//   - a descendant shares the shell's own group (a shell without job control
//     runs its foreground commands in its group);
//   - a descendant is stopped (a ^Z job: the shell is back at its prompt, but
//     the job is still the user's);
//   - a descendant whose job columns are unknown (it appeared between reads).
//
// Without the shell's job columns, or without a terminal, it is the old
// stricter rule: any child is busy.
func ForegroundBusy(procs []Proc, shell int) string {
	kids := map[int][]Proc{}
	var self *Proc
	for i, p := range procs {
		if p.Pid == shell {
			self = &procs[i]
		} else {
			kids[p.PPid] = append(kids[p.PPid], p)
		}
	}
	if self == nil || !self.Job || self.TPgid <= 0 {
		if c := kids[shell]; len(c) > 0 {
			return fmt.Sprintf("child %d, foreground unknown", c[0].Pid)
		}
		return ""
	}
	if self.TPgid != self.Pgid {
		return fmt.Sprintf("foreground process group %d, the shell's is %d", self.TPgid, self.Pgid)
	}
	seen := map[int]bool{shell: true}
	queue := []int{shell}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range kids[p] {
			if seen[c.Pid] {
				continue
			}
			seen[c.Pid] = true
			switch {
			case !c.Job:
				return fmt.Sprintf("child %d, foreground unknown", c.Pid)
			case c.Pgid == self.Pgid:
				return fmt.Sprintf("child %d in the shell's foreground group %d", c.Pid, self.Pgid)
			case c.Stopped:
				return fmt.Sprintf("stopped job %d", c.Pid)
			}
			queue = append(queue, c.Pid)
		}
	}
	return ""
}

// bare: the pane is dead, or back at an idle shell prompt.
func bare(ps PaneState, procs []Proc) bool {
	return ps.Dead || (IsShell(ps.Current) && ForegroundBusy(procs, ps.Pid) == "")
}

// NeedsJobs reports whether ForegroundBusy could tell more than the old
// any-child rule for one of panes: a shell pane whose shell has a child. The
// engine reads the job-control table only then.
func NeedsJobs(panes []PaneState, procs []Proc) bool {
	for _, ps := range panes {
		if !ps.Dead && IsShell(ps.Current) && hasChildren(procs, ps.Pid) {
			return true
		}
	}
	return false
}

func tokenUnder(procs []Proc, root int, tokens ...string) bool {
	for _, p := range subtree(procs, root) {
		b := execBase(p.Command)
		for _, t := range tokens {
			if t != "" && b == t {
				return true
			}
		}
		// an interpreter-run script (`sh /x/claude`): look at the 2nd token
		if f := strings.Fields(p.Command); len(f) > 1 && IsShell(execBase(p.Command)) {
			for _, t := range tokens {
				if t != "" && path.Base(f[1]) == t {
					return true
				}
			}
		}
	}
	return false
}

type rowState struct {
	state     string // "" = undecided
	bareCount int    // consecutive probes that saw a bare shell
}

// Judge accumulates probes for the rows of one restore.
type Judge struct {
	rows  []LaunchRow
	state map[string]*rowState
}

// NewJudge starts a judgement over rows.
func NewJudge(rows []LaunchRow) *Judge {
	j := &Judge{rows: rows, state: map[string]*rowState{}}
	for _, r := range rows {
		j.state[r.Key] = &rowState{}
	}
	return j
}

// classify is one probe's reading of one row: a final state or "" (not yet).
func classify(r LaunchRow, st *rowState, p Probe) string {
	ps, ok := p.Panes[r.Pane]
	if !ok {
		return StateGone
	}
	if !p.Acks[r.Key] {
		st.bareCount = 0
		return ""
	}
	if r.Claude {
		if tokenUnder(p.Procs, ps.Pid, "claude", "claudish") {
			st.bareCount = 0
			return StateOK
		}
	} else if (!ps.Dead && !IsShell(ps.Current)) || tokenUnder(p.Procs, ps.Pid, r.Token) {
		st.bareCount = 0
		return StateOK
	}
	if bare(ps, p.Procs) {
		st.bareCount++
		if st.bareCount >= 2 {
			return StateExited
		}
		return ""
	}
	st.bareCount = 0
	return ""
}

// Observe applies one probe and reports whether every row is decided.
// A decided row is not re-judged by later probes (Settle does that once).
func (j *Judge) Observe(p Probe) bool {
	all := true
	for _, r := range j.rows {
		st := j.state[r.Key]
		if st.state != "" {
			continue
		}
		if s := classify(r, st, p); s != "" {
			st.state = s
		} else {
			all = false
		}
	}
	return all
}

// Settle is the one probe after the 15 s settle: a row judged OK whose pane
// has gone or whose shell is bare again is re-judged.
func (j *Judge) Settle(p Probe) {
	for _, r := range j.rows {
		st := j.state[r.Key]
		if st.state != StateOK {
			continue
		}
		ps, ok := p.Panes[r.Pane]
		switch {
		case !ok:
			st.state = StateGone
		case r.Claude && !tokenUnder(p.Procs, ps.Pid, "claude", "claudish") &&
			bare(ps, p.Procs):
			st.state = StateExited
		}
	}
}

// Deadline decides every row still undecided at the deadline, with the last
// probe: the pane is gone → GONE; no ack → NOT-FIRED; an ack and a shell seen
// bare → EXITED; an ack with children that never met the OK rule → STALLED.
func (j *Judge) Deadline(p Probe) {
	for _, r := range j.rows {
		st := j.state[r.Key]
		if st.state != "" {
			continue
		}
		_, present := p.Panes[r.Pane]
		switch {
		case !present:
			st.state = StateGone
		case !p.Acks[r.Key]:
			st.state = StateNotFired
		case st.bareCount > 0:
			st.state = StateExited
		default:
			st.state = StateStalled
		}
	}
}

// Launch is the LAUNCH verdict.
type Launch struct {
	Result   string
	States   map[string]string // row key → state
	Warnings int               // non-Claude EXITED rows
	Failures int
	// Exited are the rows whose last lines the engine logs.
	Exited []string
}

// Verdict is the judgement: PASS if every judged row is OK (a non-Claude
// EXITED is a warning), FAIL otherwise; zero judged rows is PASS.
func (j *Judge) Verdict() Launch {
	l := Launch{Result: Pass, States: map[string]string{}}
	for _, r := range j.rows {
		s := j.state[r.Key].state
		if s == "" {
			s = StateStalled
		}
		l.States[r.Key] = s
		switch {
		case s == StateOK:
		case s == StateExited && !r.Claude:
			l.Warnings++
			l.Exited = append(l.Exited, r.Key)
		default:
			if s == StateExited {
				l.Exited = append(l.Exited, r.Key)
			}
			l.Failures++
			l.Result = Fail
		}
	}
	return l
}

// Line is the daemon.log line.
func (l Launch) Line() string {
	keys := make([]string, 0, len(l.States))
	for k := range l.States {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+l.States[k])
	}
	return fmt.Sprintf("LAUNCH VERDICT: %s — %d judged, %d failed, %d warnings [%s]",
		l.Result, len(l.States), l.Failures, l.Warnings, strings.Join(parts, " "))
}
