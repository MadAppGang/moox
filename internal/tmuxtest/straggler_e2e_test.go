//go:build tmuxe2e

package tmuxtest

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
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
