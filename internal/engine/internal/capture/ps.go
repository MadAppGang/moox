package capture

import (
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/MadAppGang/moox/internal/engine/internal/sidmap"
)

// PSArgs is the ONE process-table read of a save (B25): every process's pid,
// ppid, start time and command, unlimited width. Run with LC_ALL=C so lstart
// has one spelling (the by-pid start tokens are written the same way, §7).
func PSArgs() []string {
	if runtime.GOOS == "linux" {
		return []string{"-ww", "-eo", "pid=,ppid=,lstart=,command="}
	}
	return []string{"-ww", "-axo", "pid=,ppid=,lstart=,command="}
}

// PSEnvArgs is the single-pid environment read for a claudish model (B12).
// Never a multi-pid `ps -p a,b`: on this machine two pids take 7–12 s where
// one takes 20 ms (cc_proc.sh:461-472).
func PSEnvArgs(pid int) []string {
	return []string{"eww", "-o", "command=", "-p", strconv.Itoa(pid)}
}

// JobsArgs is the job-control read of the idle-shell check (B47): every
// process's pid, parent, process group, the foreground process group of its
// controlling terminal, and its state. It is not the save's one table (B25):
// only the manual-restore precondition reads it.
func JobsArgs() []string {
	if runtime.GOOS == "linux" {
		return []string{"-eo", "pid=,ppid=,pgid=,tpgid=,stat="}
	}
	return []string{"-axo", "pid=,ppid=,pgid=,tpgid=,stat="}
}

// Job is one JobsArgs row. TPgid is the foreground process group of the
// process's controlling terminal: 0 (macOS) or -1 (Linux) without one.
// Stopped is a job-control stop (state T): a suspended job, ^Z.
type Job struct {
	Pid, PPid, Pgid, TPgid int
	Stopped                bool
}

// ParseJobs parses `ps -o pid=,ppid=,pgid=,tpgid=,stat=`; malformed rows are
// skipped.
func ParseJobs(out []byte) []Job {
	var jobs []Job
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		var n [4]int
		ok := true
		for i, s := range f[:4] {
			v, err := strconv.Atoi(s)
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if ok && n[0] > 0 {
			jobs = append(jobs, Job{Pid: n[0], PPid: n[1], Pgid: n[2], TPgid: n[3], Stopped: strings.HasPrefix(f[4], "T")})
		}
	}
	return jobs
}

// Proc is one process.
type Proc struct {
	Pid, PPid int
	Start     string // the start token: lstart with runs of blanks as "_"
	Command   string
}

// Table is the parsed ps output, in ps order.
type Table struct {
	Procs []Proc
	byPid map[int]int
}

// lstartFields is the number of blank-separated words in `lstart` under
// LC_ALL=C ("Sat Oct 25 03:00:00 2026").
const lstartFields = 5

// ParsePS parses `ps -o pid=,ppid=,lstart=,command=`. The command keeps its
// own inner spacing.
func ParsePS(out []byte) Table {
	t := Table{byPid: map[int]int{}}
	for _, line := range strings.Split(string(out), "\n") {
		words, rest := splitWords(line, 2+lstartFields)
		if len(words) < 2+lstartFields {
			continue
		}
		pid, err1 := strconv.Atoi(words[0])
		ppid, err2 := strconv.Atoi(words[1])
		if err1 != nil || err2 != nil || pid <= 0 {
			continue
		}
		t.byPid[pid] = len(t.Procs)
		t.Procs = append(t.Procs, Proc{Pid: pid, PPid: ppid, Start: strings.Join(words[2:2+lstartFields], "_"),
			Command: strings.TrimSpace(rest)})
	}
	return t
}

// splitWords takes the first n blank-separated words and returns the rest of
// the line unchanged.
func splitWords(line string, n int) ([]string, string) {
	var words []string
	i := 0
	for len(words) < n {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			break
		}
		j := i
		for j < len(line) && line[j] != ' ' && line[j] != '\t' {
			j++
		}
		words = append(words, line[i:j])
		i = j
	}
	return words, line[i:]
}

// Get returns the process with pid.
func (t Table) Get(pid int) (Proc, bool) {
	i, ok := t.byPid[pid]
	if !ok {
		return Proc{}, false
	}
	return t.Procs[i], true
}

// Sidmap converts the table for the climb.
func (t Table) Sidmap() []sidmap.Proc {
	out := make([]sidmap.Proc, len(t.Procs))
	for i, p := range t.Procs {
		out[i] = sidmap.Proc{Pid: p.Pid, PPid: p.PPid, Command: p.Command}
	}
	return out
}

// StartToken normalises a start time the way the shell's _cc_start_token
// does: blanks trimmed, every run of blanks turned into one "_".
func StartToken(s string) string { return strings.Join(strings.Fields(s), "_") }

// FullCommand is the `full_command` rule (PR-M12, cc_proc_pane_cmd): the
// pane process's depth-1 child — the FIRST child of pane_pid in ps order — if
// it has one, else pane_pid's own command (Claude exec'd as the pane process,
// R3). "" when the pane process is not in the table.
func (t Table) FullCommand(panePid int) string {
	for _, p := range t.Procs {
		if p.PPid == panePid && p.Pid != panePid {
			return p.Command
		}
	}
	if p, ok := t.Get(panePid); ok {
		return p.Command
	}
	return ""
}

var resumeRe = regexp.MustCompile(`--resume( +[0-9a-fA-F-]{36})?`)

// ClaudishReplay is _cc_claudish_replay (B12): the launcher argv after its
// `…/claudish` token, with any --resume [uuid] removed and blanks squeezed;
// when it names no model or profile, `--model <model>` is appended (the
// interactively picked model survives only in the Claude child's
// environment). Tabs and newlines are removed.
func ClaudishReplay(launcherArgv, model string) string {
	flags := launcherArgv
	if i := strings.LastIndex(flags, "/claudish"); i >= 0 {
		flags = flags[i+len("/claudish"):]
	}
	flags = resumeRe.ReplaceAllString(flags, "")
	flags = strings.Join(strings.Fields(flags), " ")
	if !HasExplicitModel(flags) && model != "" {
		if flags != "" {
			flags += " "
		}
		flags += "--model " + model
	}
	return strings.NewReplacer("\t", "", "\n", "").Replace(flags)
}

// HasExplicitModel reports whether claudish flags name a model or profile
// (--model, -m, --model-<role>, --profile), so no ps eww is needed.
func HasExplicitModel(flags string) bool {
	for _, tok := range strings.Fields(flags) {
		switch {
		case tok == "--model", tok == "-m", tok == "--profile",
			strings.HasPrefix(tok, "--model="), strings.HasPrefix(tok, "--model-"), strings.HasPrefix(tok, "--profile="):
			return true
		}
	}
	return false
}

// ModelFromEnv reads CLAUDISH_ACTIVE_MODEL_NAME out of `ps eww` output.
func ModelFromEnv(out []byte) string {
	for _, tok := range strings.Fields(string(out)) {
		if v, ok := strings.CutPrefix(tok, "CLAUDISH_ACTIVE_MODEL_NAME="); ok {
			return v
		}
	}
	return ""
}
