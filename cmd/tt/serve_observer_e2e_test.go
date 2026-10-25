//go:build tmuxe2e

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer/wire"
)

// TestE2E_Serve_EnsureReplacesFreshPlainObserver (G2-2-H1): a plain
// `tt observe` that bound this server's socket seconds ago is not the daemon.
// `tt serve --ensure` replaces it at once (it used to keep any observer under
// 15 s old, log DAEMON-KEPT and leave an active server with no saver), and
// the daemon that replaces it says so in its pid file (role "serve").
// Isolation: the world/startServer harness of serve_e2e_test.go (scratch
// HOME/XDG, -f /dev/null -L moox-test-*, /bin/sh panes).
func TestE2E_Serve_EnsureReplacesFreshPlainObserver(t *testing.T) {
	dev, _ := ttBinaries(t)
	w := newWorld(t)
	s := startServer(t, "")
	dir := w.paths(s).TTState
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	obs := exec.Command(dev, "observe", "--socket-dir", dir, "--tmux-socket", s.T.Socket, "--tmux-bin", s.T.Bin)
	obs.Env = w.env()
	obs.Dir = w.home
	if err := obs.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = obs.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = obs.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = obs.Process.Kill()
		}
	})
	plain := w.daemon(s, 0)
	if plain.PID != obs.Process.Pid || plain.Role != wire.RoleObserve {
		t.Fatalf("the plain observer's pid file = %+v, want pid %d role %q", plain, obs.Process.Pid, wire.RoleObserve)
	}

	w.ensure(dev, s) // well inside the old 15 s grace
	daemon := w.daemon(s, plain.PID)
	if daemon.Role != wire.RoleServe {
		t.Fatalf("the daemon's pid file = %+v, want role %q", daemon, wire.RoleServe)
	}
	log := w.log(s)
	if !strings.Contains(log, "is an observer without a saver") || strings.Contains(log, fmt.Sprintf("DAEMON-KEPT pid=%d", plain.PID)) {
		t.Fatalf("the fresh plain observer was not replaced as a non-saver:\n%s", log)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the replaced plain observer pid %d still runs", plain.PID)
	}
	// A second ensure keeps the daemon: its role, not its age, decides.
	w.ensure(dev, s)
	if rec, ok := daemonRunning(dir); !ok || rec.PID != daemon.PID {
		t.Fatalf("a second ensure replaced the daemon: %+v", rec)
	}
}
