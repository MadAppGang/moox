package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
)

// TestParseJobs reads the pid/ppid/pgid/tpgid/stat rows of both ps
// spellings (macOS: 0 without a terminal; Linux: -1) and skips junk.
func TestParseJobs(t *testing.T) {
	out := []byte("  100     1   100   100 Ss  \n  101   100   101   100 T\n  102     1   102    -1 S\njunk line\n 7 1 7 x S\n\n")
	got := capture.ParseJobs(out)
	want := []capture.Job{
		{Pid: 100, PPid: 1, Pgid: 100, TPgid: 100},
		{Pid: 101, PPid: 100, Pgid: 101, TPgid: 100, Stopped: true},
		{Pid: 102, PPid: 1, Pgid: 102, TPgid: -1},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseJobs = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestShellForegroundBusy (B47, iter-2 real-zsh bug): a bootstrap pane is
// idle when its shell owns the terminal's foreground. An idle zsh keeps
// helpers as children (powerlevel10k's gitstatusd, async prompt workers) in
// process groups of their own; those must not make the pane populated.
// Anything run at the prompt still refuses.
func TestShellForegroundBusy(t *testing.T) {
	const sh = 500
	for _, c := range []struct {
		name string
		jobs string
		busy bool
	}{
		{"a fresh shell", jobLine(sh, 1, sh, sh, "Ss"), false},
		{"gitstatusd in its own group", jobLine(sh, 1, sh, sh, "Ss") + jobLine(sh+1, sh, sh+1, sh, "S"), false},
		{"a helper's grandchild in the helper's group", jobLine(sh, 1, sh, sh, "Ss") +
			jobLine(sh+1, sh, sh+1, sh, "S") + jobLine(sh+2, sh+1, sh+1, sh, "S"), false},
		{"an async worker in its own session (no terminal)", jobLine(sh, 1, sh, sh, "Ss") + jobLine(sh+1, sh, sh+1, 0, "Ss"), false},
		{"a foreground job (claude, vim, sleep)", jobLine(sh, 1, sh, sh+1, "Ss") + jobLine(sh+1, sh, sh+1, sh+1, "S+"), true},
		{"a foreground job's process is gone, its group still owns the tty", jobLine(sh, 1, sh, sh+9, "Ss"), true},
		{"no job control: the command shares the shell's group", jobLine(sh, 1, sh, sh, "Ss") + jobLine(sh+1, sh, sh, sh, "S+"), true},
		{"no job control: a grandchild in the shell's group", jobLine(sh, 1, sh, sh, "Ss") +
			jobLine(sh+1, sh, sh+1, sh, "S") + jobLine(sh+2, sh+1, sh, sh, "S"), true},
		{"a job suspended with ^Z", jobLine(sh, 1, sh, sh, "Ss") + jobLine(sh+1, sh, sh+1, sh, "T"), true},
		{"no terminal: the old rule, any child refuses", jobLine(sh, 1, sh, -1, "Ss") + jobLine(sh+1, sh, sh+1, -1, "S"), true},
		{"no terminal, no child", jobLine(sh, 1, sh, 0, "Ss"), false},
		{"the shell's row is missing, a child is listed", jobLine(sh+1, sh, sh+1, sh, "S"), true},
		{"the shell's row is missing, nothing else", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := shellForegroundBusy(capture.ParseJobs([]byte(c.jobs)), sh)
			if (got != "") != c.busy {
				t.Fatalf("shellForegroundBusy = %q, want busy=%v", got, c.busy)
			}
		})
	}
}

// TestManualPreconditions_BackgroundHelperAllowed: the precondition reads
// the job-control table, so a real-zsh bootstrap with gitstatusd is
// restorable while a foreground program still refuses (ErrPopulated).
func TestManualPreconditions_BackgroundHelperAllowed(t *testing.T) {
	g := newRFake()
	g.addSession("0")
	e, _, _ := newRestoreEngine(t, g)
	g.opts["@moox-active"] = "1"
	pid := g.panes[0].pid
	ctx := context.Background()

	e.procs = rprocs{alive: map[int]bool{}, table: psLine(pid, 1, "zsh") + psLine(pid+1, pid, "gitstatusd"),
		jobs: jobLine(pid, 1, pid, pid, "Ss") + jobLine(pid+1, pid, pid+1, pid, "S")}
	if err := e.ManualPreconditions(ctx); err != nil {
		t.Fatalf("zsh with gitstatusd in its own group: %v", err)
	}
	e.procs = rprocs{alive: map[int]bool{}, table: psLine(pid, 1, "zsh") + psLine(pid+2, pid, "sleep 600"),
		jobs: jobLine(pid, 1, pid, pid+2, "Ss") + jobLine(pid+2, pid, pid+2, pid+2, "S+")}
	if err := e.ManualPreconditions(ctx); !errors.Is(err, ErrPopulated) || !errors.Is(err, ErrPreconditions) {
		t.Fatalf("zsh with a foreground sleep: %v, want ErrPopulated", err)
	}
}
