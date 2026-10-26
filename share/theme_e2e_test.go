//go:build tmuxe2e

package share_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// TestE2E_Theme_RunnerEndsWithItsServer (v0.1.3 CI): a theme.sh runner whose
// server exits in the middle of a pass ends at once and leaves nothing beside
// the socket. On the way out tmux SIGTERMs its run-shell jobs (the `sh -c`
// wrapper, so the runner itself is orphaned), and every tmux call of the pass
// fails. The runner used to take that for a failed pass: it re-marked the
// request, slept, retried three times and left <socket>.theme.dirty behind —
// writing beside a socket nobody serves for ~300 ms after its server ended,
// which is what made the harness's temp-root removal fail ("directory not
// empty"). The pass is held in flight by THEME_SYNC_TESTHOOK, armed only
// after the config's own theme pass has finished.
func TestE2E_Theme_RunnerEndsWithItsServer(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	q := tmuxtest.ConfQuote
	script := "#!/bin/sh\n" +
		"[ \"$1\" = pass ] && [ -e " + q(filepath.Join(dir, "arm")) + " ] || exit 0\n" +
		"echo \"$PPID\" >> " + q(filepath.Join(dir, "runner")) + "\n" +
		"i=0\n" +
		"while [ ! -e " + q(filepath.Join(dir, "release")) + " ] && [ \"$i\" -lt 200 ]; do sleep 0.05; i=$((i + 1)); done\n" +
		"exit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s := tmuxtest.StartCold(t, tmuxtest.WithEnv("THEME_SYNC_TESTHOOK="+hook))
	beside := func() []string {
		m, _ := filepath.Glob(s.T.Socket + ".theme.*")
		return m
	}
	// The config's own `theme.sh auto` runner must be done (its lock gone)
	// before arming, so the held pass is the one the new window's hook
	// starts. It may leave a request owed (.theme.dirty: a live server's
	// failed pass keeps it for the next request); the held pass takes it.
	lock := s.T.Socket + ".theme.lock"
	if !tmuxtest.Poll(10*time.Second, 50*time.Millisecond, func() bool { _, err := os.Lstat(lock); return err != nil }) {
		t.Fatalf("the config's theme runner did not finish: %v", beside())
	}
	if err := os.WriteFile(filepath.Join(dir, "arm"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Run(t, "new-window", "-d") // after-new-window[100]: run-shell -b theme.sh sync
	var runner int
	if !tmuxtest.Poll(10*time.Second, 20*time.Millisecond, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "runner"))
		f := strings.Fields(string(b))
		if len(f) == 0 {
			return false
		}
		runner, _ = strconv.Atoi(f[0])
		return runner > 0
	}) {
		t.Fatal("no theme pass started for the new window")
	}

	// The server ends under the held pass (exit-empty, no kill-server).
	s.Run(t, "kill-session", "-t", s.Session)
	if !tmuxtest.Poll(5*time.Second, 20*time.Millisecond, func() bool { return !s.Alive() }) {
		t.Fatal("the server did not exit")
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alive := func() bool { return processAlive(runner) }
	if !tmuxtest.Poll(2*time.Second, 10*time.Millisecond, func() bool { return !alive() }) {
		t.Fatalf("runner %d still runs 2 s after its server ended (it retries against nobody)", runner)
	}
	// Nothing beside the socket, now or a moment later (no relaunch either).
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if left := beside(); len(left) != 0 {
			t.Fatalf("after its server ended the runner left %v beside the socket", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processAlive: pid runs and is not a zombie (an orphan that exited answers
// kill 0 until its new parent reaps it).
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true // no /proc (macOS): kill 0 is the answer
	}
	// "<pid> (<comm>) <state> …": the state follows the last ')'.
	st := string(b)
	if i := strings.LastIndexByte(st, ')'); i >= 0 && i+2 < len(st) {
		return st[i+2] != 'Z'
	}
	return true
}
