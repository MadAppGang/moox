//go:build tmuxe2e

package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recTB records Errorf instead of failing, so the leak check itself can be
// watched failing.
type recTB struct {
	testing.TB
	errs []string
}

func (r *recTB) Errorf(format string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(format, a...)) }

// TestClose_ReportsAndReapsAProcessThatOutlivesItsServer (F1): Close fails
// the test for — and SIGKILLs — a process whose argv names the closed
// server's socket and that is still running stragglerWait after the server
// ended. A process that exits with its server is not reported.
func TestClose_ReportsAndReapsAProcessThatOutlivesItsServer(t *testing.T) {
	s := Start(t)
	leak := exec.Command("/bin/sh", "-c", "sleep 60; true", "leak", s.T.Socket)
	if err := leak.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- leak.Wait() }()
	rec := &recTB{TB: t}
	s.tb = rec
	s.Close()
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], fmt.Sprint(leak.Process.Pid)) {
		t.Fatalf("Close reported %q, want one straggler naming pid %d", rec.errs, leak.Process.Pid)
	}
	err := <-done
	if ee, ok := err.(*exec.ExitError); !ok || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the straggler was not killed: %v", err)
	}

	// A clean server: nothing reported.
	s2 := Start(t)
	rec2 := &recTB{TB: t}
	s2.tb = rec2
	s2.Close()
	if len(rec2.errs) != 0 {
		t.Fatalf("a clean close reported %q", rec2.errs)
	}
}

// jobScript writes a script that records its pid in <dir>/pid and then
// becomes `sleep d` (exec: one process, one pid), for a config-time
// `run-shell -b`. Behind the `|| true` the job's `sh -c` cannot exec it, so
// it is a child of the job — the shape of every moox hook script.
func jobScript(t *testing.T, dir, d string) (script, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(dir, "pid")
	script = filepath.Join(dir, "job.sh")
	body := "#!/bin/sh\necho $$ > " + shQuote(pidFile) + "\nexec sleep " + d + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, pidFile
}

func readPid(t *testing.T, file string) int {
	t.Helper()
	var pid int
	if !Poll(10*time.Second, 20*time.Millisecond, func() bool {
		b, _ := os.ReadFile(file)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return pid > 0
	}) {
		t.Fatalf("the job never wrote %s", file)
	}
	return pid
}

// alive: pid runs and is not a zombie.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// TestClose_WaitsForTheServersOrphanedJobs (v0.1.3): a run-shell job's
// script names no socket in its argv, and when the server exits tmux SIGTERMs
// only the job's `sh -c` wrapper, so the script is orphaned and runs on — a
// theme.sh runner did, re-creating <socket>.theme.dirty while the temp root
// was being removed. It stays in the server's session, which is how Close
// knows it: Close returns only once it has exited (no report when that is
// within stragglerWait), and one that outlives stragglerWait is reported and
// killed.
func TestClose_WaitsForTheServersOrphanedJobs(t *testing.T) {
	for _, tc := range []struct {
		name, sleep string
		stuck       bool
	}{{"exits in time", "1.5", false}, {"stuck", "60", true}} {
		t.Run(tc.name, func(t *testing.T) {
			script, pidFile := jobScript(t, t.TempDir(), tc.sleep)
			s := StartCold(t, WithHarnessLines(`run-shell -b "`+shQuote(script)+` >/dev/null 2>&1 || true"`))
			job := readPid(t, pidFile)
			rec := &recTB{TB: t}
			s.tb = rec
			s.Close()
			if tc.stuck {
				// Close SIGKILLed it; the kill lands, and init reaps, a moment later.
				Poll(2*time.Second, 10*time.Millisecond, func() bool { return !alive(job) })
			}
			if alive(job) {
				t.Fatalf("Close returned while the server's job %d still runs", job)
			}
			switch {
			case !tc.stuck && len(rec.errs) != 0:
				t.Fatalf("a job that exited in time was reported: %q", rec.errs)
			case tc.stuck && (len(rec.errs) == 0 || !strings.Contains(strings.Join(rec.errs, "\n"), fmt.Sprint(job))):
				t.Fatalf("Close reported %q, want a straggler naming the job's pid %d", rec.errs, job)
			}
		})
	}
}

// TestClose_WaitsForAProcessWithTheServersTMUX: where /proc shows other
// processes' environments (Linux), a process that holds this server's $TMUX
// — a pane's child, a job that left the session — holds Close too.
func TestClose_WaitsForAProcessWithTheServersTMUX(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("no /proc: environments are not readable here")
	}
	s := Start(t)
	c := exec.Command("/bin/sh", "-c", "sleep 1")
	c.Env = append(c.Environ(), "TMUX="+s.T.Socket+",1,0")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Wait() }()
	rec := &recTB{TB: t}
	s.tb = rec
	s.Close()
	if alive(c.Process.Pid) { // exited but unreaped (a zombie) counts as gone
		t.Fatal("Close returned while a process with this server's $TMUX still runs")
	}
	if len(rec.errs) != 0 {
		t.Fatalf("a $TMUX process that exited in time was reported: %q", rec.errs)
	}
}
