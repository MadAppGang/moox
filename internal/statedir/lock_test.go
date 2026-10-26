package statedir_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/statedir"
)

// The lock tests run real holders in SUBPROCESSES (this test binary
// re-executed with STATEDIR_LOCK_HELPER set), because flock's guarantees are
// between open file descriptions and processes, and the properties §5.6 relies
// on — release on death, no inheritance by children — only exist across
// processes.

const helperEnv = "STATEDIR_LOCK_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(lockHelper(mode))
	}
	os.Exit(m.Run())
}

// lockHelper modes (args via env):
//
//	hold       lock DIR/NAME, print "locked", block on stdin until it closes
//	hold-child lock, spawn `sleep 30` (pid printed), print "locked", block
//	contend    loop ITER times: try; on success append "+pid" to LOG, hold
//	           briefly, append "-pid", release. Print the success count.
func lockHelper(mode string) int {
	dir, name := os.Getenv("LOCK_DIR"), os.Getenv("LOCK_NAME")
	d, err := statedir.Open(dir, filepath.Join(dir, "..", "no-claude"))
	if err != nil {
		fmt.Println("open:", err)
		return 2
	}
	rec := []byte(strconv.Itoa(os.Getpid()) + "\n")
	switch mode {
	case "hold", "hold-child":
		l, err := d.TryLock(name, rec)
		if err != nil {
			fmt.Println("busy:", err)
			return 3
		}
		if mode == "hold-child" {
			c := exec.Command("sleep", "30")
			if err := c.Start(); err != nil {
				fmt.Println("spawn:", err)
				return 4
			}
			fmt.Println("child", c.Process.Pid)
		}
		fmt.Println("locked")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		l.Release()
		return 0
	case "contend":
		iters, _ := strconv.Atoi(os.Getenv("LOCK_ITER"))
		log, err := os.OpenFile(os.Getenv("LOCK_LOG"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			fmt.Println("log:", err)
			return 5
		}
		won := 0
		for i := 0; i < iters; i++ {
			l, err := d.TryLock(name, rec)
			if err != nil {
				if !errors.Is(err, statedir.ErrBusy) {
					fmt.Println("err:", err)
					return 6
				}
				continue
			}
			won++
			fmt.Fprintf(log, "+%d\n", os.Getpid())
			for j := 0; j < 2000; j++ { // hold for a moment without a timer
				_ = j * j
			}
			fmt.Fprintf(log, "-%d\n", os.Getpid())
			l.Release()
		}
		fmt.Println(won)
		return 0
	case "first-create":
		// LOCK_DIR is a root; for i in 0..ITER-1 wait (spinning, no timer)
		// until ROOT/i exists, then TryLock NAME in it once. Prints one line
		// per result that is neither a hold nor ErrBusy.
		return firstCreate(os.Getenv("LOCK_DIR"), name)
	}
	return 1
}

func firstCreate(root, name string) int {
	iters, _ := strconv.Atoi(os.Getenv("LOCK_ITER"))
	bad := 0
	for i := 0; i < iters; i++ {
		dir := filepath.Join(root, strconv.Itoa(i))
		var d *statedir.Dir
		for start := time.Now(); d == nil; {
			if time.Since(start) > 10*time.Second {
				fmt.Println("timeout waiting for", dir)
				return 7
			}
			d, _ = statedir.OpenExisting(dir, filepath.Join(root, "no-claude"))
		}
		l, err := d.TryLock(name, nil)
		switch {
		case err == nil:
			l.Release()
		case errors.Is(err, statedir.ErrBusy):
		default:
			bad++
			fmt.Println("err:", err)
		}
		_ = d.Close()
	}
	fmt.Println("bad", bad)
	return 0
}

type holder struct {
	cmd   *exec.Cmd
	stdin interface{ Close() error }
	out   *bufio.Reader
	child int
}

func startHolder(t *testing.T, dir, name, mode string) *holder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "LOCK_DIR="+dir, "LOCK_NAME="+name)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &holder{cmd: cmd, stdin: in, out: bufio.NewReader(outPipe)}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if h.child > 0 {
			_ = syscall.Kill(h.child, syscall.SIGKILL)
		}
	})
	for {
		line, err := h.out.ReadString('\n')
		if err != nil {
			t.Fatalf("holder exited before locking: %v", err)
		}
		line = strings.TrimSpace(line)
		if pid, ok := strings.CutPrefix(line, "child "); ok {
			h.child, _ = strconv.Atoi(pid)
			continue
		}
		if line == "locked" {
			return h
		}
		t.Fatalf("holder: %s", line)
	}
}

func lockDir(t *testing.T) (*statedir.Dir, string) {
	t.Helper()
	root := shortTemp(t)
	dir := filepath.Join(root, "state")
	d, err := statedir.Open(dir, filepath.Join(root, "no-claude"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, dir
}

// waitFree retries TryLock until it succeeds or the deadline passes, with a
// one-shot timer per attempt (the audit's timer rule applies to shipped code;
// this is a test, and it still never sleeps unboundedly).
func waitFree(t *testing.T, d *statedir.Dir, name string, within time.Duration) (*statedir.Lock, time.Duration) {
	t.Helper()
	start := time.Now()
	for {
		l, err := d.TryLock(name, nil)
		if err == nil {
			return l, time.Since(start)
		}
		if !errors.Is(err, statedir.ErrBusy) {
			t.Fatal(err)
		}
		if time.Since(start) > within {
			t.Fatalf("lock still busy after %v", within)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLock_ContendedIsBusy(t *testing.T) {
	d, dir := lockDir(t)
	h := startHolder(t, dir, "save.lock", "hold")
	_, err := d.TryLock("save.lock", nil)
	if !errors.Is(err, statedir.ErrBusy) {
		t.Fatalf("TryLock while held elsewhere: %v", err)
	}
	// The diagnostics record is readable and names the holder.
	b, _, err := d.ReadFile("save.lock")
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(h.cmd.Process.Pid) {
		t.Fatalf("record = %q, %v", b, err)
	}
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
	l, _ := waitFree(t, d, "save.lock", 2*time.Second)
	l.Release()
}

func TestLock_BusyNeverWaits(t *testing.T) {
	d, dir := lockDir(t)
	startHolder(t, dir, "save.lock", "hold")
	start := time.Now()
	for i := 0; i < 50; i++ {
		if _, err := d.TryLock("save.lock", nil); !errors.Is(err, statedir.ErrBusy) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("50 busy attempts took %v; TryLock is waiting", el)
	}
}

func TestLock_ReleasedOnHolderDeath(t *testing.T) {
	d, dir := lockDir(t)
	h := startHolder(t, dir, "save.lock", "hold")
	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Wait()
	l, took := waitFree(t, d, "save.lock", 2*time.Second)
	defer l.Release()
	t.Logf("free %v after SIGKILL, with no reclaim step", took)
	// The file was never unlinked: same name, still there.
	if _, err := d.Lstat("save.lock"); err != nil {
		t.Fatal(err)
	}
}

func TestLock_ChildDoesNotInherit(t *testing.T) {
	d, dir := lockDir(t)
	h := startHolder(t, dir, "save.lock", "hold-child")
	if h.child <= 0 {
		t.Fatal("holder spawned no child")
	}
	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Wait()
	if err := syscall.Kill(h.child, 0); err != nil {
		t.Fatalf("precondition: the child should still be alive: %v", err)
	}
	l, _ := waitFree(t, d, "save.lock", 2*time.Second)
	l.Release()
}

func TestLock_SameProcessSecondHolderBusy(t *testing.T) {
	d, dir := lockDir(t)
	l, err := d.TryLock("save.lock", []byte("me\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.TryLock("save.lock", nil); !errors.Is(err, statedir.ErrBusy) {
		t.Fatalf("second holder, same Dir: %v", err)
	}
	// A second Dir handle on the same directory is the same lock.
	d2, err := statedir.Open(dir, filepath.Join(filepath.Dir(dir), "no-claude"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err := d2.TryLock("save.lock", nil); !errors.Is(err, statedir.ErrBusy) {
		t.Fatalf("second holder, second Dir: %v", err)
	}
	l.Release()
	l.Release() // idempotent
	l2, err := d2.TryLock("save.lock", nil)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
}

func TestLock_NeverUnlinkedRenamedOrReplaced(t *testing.T) {
	d, _ := lockDir(t)
	l, err := d.TryLock("save.lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	info, _ := d.Lstat("save.lock")
	checks := map[string]error{
		"Remove":     d.Remove("save.lock", statedir.TypeRegular, nil),
		"WriteFile":  d.WriteFile("save.lock", []byte("x"), 0o600),
		"Rename":     d.Rename("save.lock", "other"),
		"RenameOver": func() error { _ = d.WriteFile("y", nil, 0o600); return d.Rename("y", "save.lock") }(),
		"RemoveTree": d.RemoveTree("save.lock"),
	}
	for op, err := range checks {
		if !errors.Is(err, statedir.ErrLockFile) {
			t.Errorf("%s on a lock file: %v", op, err)
		}
	}
	after, err := d.Lstat("save.lock")
	if err != nil || !after.SameFile(info) {
		t.Fatalf("the lock file changed identity: %v", err)
	}
}

func TestLock_PublishRefusedIfPathReplaced(t *testing.T) {
	d, dir := lockDir(t)
	l, err := d.TryLock("save.lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if err := l.Check(); err != nil {
		t.Fatalf("Check on an intact lock: %v", err)
	}
	// Something OUTSIDE moox replaces the file (an `rm`, a rename over it).
	if err := os.Remove(filepath.Join(dir, "save.lock")); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); !errors.Is(err, statedir.ErrBusy) {
		t.Fatalf("Check after removal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "save.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); !errors.Is(err, statedir.ErrBusy) {
		t.Fatalf("Check after replacement: %v", err)
	}
}

// TestLock_FirstCreateRaceIsBusyOrHeld: processes that take a lock for the
// FIRST time at the same instant — the file does not exist yet — each either
// hold it or get ErrBusy; none gets an error. macOS answers a concurrent
// O_CREAT of a new name with ENOENT (see openLockFile); without the retry this
// failed on every run on Darwin (571 "open …: no such file or directory" in
// 4×1000 races, measured), and it was the cause of TestLock_NoReclaimRace's
// rare flake. Linux never fails it. The parent creates fresh directories one
// at a time; the contenders spin until each appears, so their opens collide.
func TestLock_FirstCreateRaceIsBusyOrHeld(t *testing.T) {
	root := shortTemp(t)
	const procs, iters = 4, 1000
	cmds := make([]*exec.Cmd, procs)
	outs := make([]*strings.Builder, procs)
	for i := range cmds {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), helperEnv+"=first-create", "LOCK_DIR="+root,
			"LOCK_NAME=save.lock", "LOCK_ITER="+strconv.Itoa(iters))
		outs[i] = &strings.Builder{}
		c.Stdout = outs[i]
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = c
	}
	for i := 0; i < iters; i++ {
		if err := os.Mkdir(filepath.Join(root, strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
		// Long enough for every contender to reach the directory; a busy
		// wait, not a timer.
		for start := time.Now(); time.Since(start) < 200*time.Microsecond; {
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("contender %d: %v (%s)", i, err, outs[i])
		}
		out := outs[i].String()
		if !strings.HasSuffix(out, "bad 0\n") {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			t.Errorf("contender %d: a first-time lock returned an error that is neither a hold nor ErrBusy: %s (first: %s)",
				i, lines[len(lines)-1], lines[0])
		}
	}
}

// TestLock_NoReclaimRace is Q11 (a)'s question asked of flock: three processes
// hammer one lock, and the log of acquire/release markers must never show two
// holders at once. Under rev 2's check-then-unlink reclaim this interleaving
// ended with two holders; with flock there is no reclaim to interleave.
func TestLock_NoReclaimRace(t *testing.T) {
	_, dir := lockDir(t)
	logPath := filepath.Join(filepath.Dir(dir), "holders.log")
	const procs, iters = 3, 300
	cmds := make([]*exec.Cmd, procs)
	outs := make([]*strings.Builder, procs)
	for i := range cmds {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), helperEnv+"=contend", "LOCK_DIR="+dir, "LOCK_NAME=save.lock",
			"LOCK_ITER="+strconv.Itoa(iters), "LOCK_LOG="+logPath)
		outs[i] = &strings.Builder{}
		c.Stdout = outs[i]
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = c
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("contender %d: %v (%s)", i, err, outs[i])
		}
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	holding := ""
	acquired := 0
	for _, line := range strings.Fields(string(b)) {
		switch line[0] {
		case '+':
			if holding != "" {
				t.Fatalf("two holders: %s acquired while %s held", line[1:], holding)
			}
			holding = line[1:]
			acquired++
		case '-':
			if holding != line[1:] {
				t.Fatalf("release by %s while %q held", line[1:], holding)
			}
			holding = ""
		}
	}
	if acquired == 0 {
		t.Fatal("no contender ever acquired the lock")
	}
	t.Logf("%d acquisitions across %d processes, never two holders", acquired, procs)
}
