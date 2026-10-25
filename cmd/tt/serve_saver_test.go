package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
	"github.com/MadAppGang/moox/internal/sockets"
)

type mapEnv map[string]string

func (m mapEnv) Getenv(k string) string { return m[k] }

// saverFixture is the production layout (sockets.For): observer.pid in the
// per-socket tt dir, saver.state in the per-socket moox state dir — two
// different trees, which a hand-built Paths with both under one root hid
// (G2-2-M3).
func saverFixture(t *testing.T) sockets.Paths {
	t.Helper()
	home := t.TempDir()
	p, err := sockets.For(filepath.Join(home, "tmux-501", "work"), mapEnv{"HOME": home})
	if err != nil {
		t.Fatal(err)
	}
	if p.TTState == p.State {
		t.Fatalf("fixture: TTState and State are the same dir %s", p.State)
	}
	for _, d := range []string{p.TTState, p.State} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func writePid(t *testing.T, p sockets.Paths, rec wire.PidFile) {
	t.Helper()
	b, err := wire.EncodePidFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.TTState, "observer.pid"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSaverState(t *testing.T, p sockets.Paths, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.State, "saver.state"), []byte(strconv.Itoa(pid)+" idle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHasSaver_PlainObserverIsReplaced (G2-H5, G2-2-H1): `tt serve --ensure`
// keeps a running observer only when it is the moox daemon. The pid file now
// says which role wrote it, so a plain `tt observe` is replaced however young
// it is (a 15 s age grace used to keep a fresh one as "the daemon" and leave
// an active server with no saver), and a daemon is kept even before, or
// without, a saver.state write (G2-2-M4).
func TestHasSaver_PlainObserverIsReplaced(t *testing.T) {
	now := time.Now()
	pid := os.Getpid()
	at := func(ago time.Duration) string { return observer.FormatTime(now.Add(-ago)) }

	cases := []struct {
		name      string
		role      string
		started   time.Duration
		saverPID  int // 0: no saver.state
		wantSaver bool
	}{
		{"fresh plain observer (2 s), no saver.state", wire.RoleObserve, 2 * time.Second, 0, false},
		{"old plain observer, no saver.state", wire.RoleObserve, time.Hour, 0, false},
		{"fresh plain observer, saver.state names another pid", wire.RoleObserve, 2 * time.Second, pid + 1, false},
		{"fresh daemon (2 s) before its first saver.state", wire.RoleServe, 2 * time.Second, 0, true},
		{"old daemon whose saver.state write failed", wire.RoleServe, time.Hour, 0, true},
		{"daemon with its saver.state", wire.RoleServe, time.Hour, pid, true},
		{"role-less (older binary), saver.state names it", "", time.Hour, pid, true},
		{"role-less, fresh, no saver.state", "", 2 * time.Second, 0, false},
		{"role-less, saver.state names another pid", "", 2 * time.Second, pid + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := saverFixture(t)
			rec := wire.PidFile{PID: pid, StartedAt: at(c.started), TT: "x", Role: c.role}
			writePid(t, p, rec)
			if c.saverPID != 0 {
				writeSaverState(t, p, c.saverPID)
			}
			if got := hasSaver(p, rec); got != c.wantSaver {
				t.Fatalf("hasSaver = %v, want %v", got, c.wantSaver)
			}
		})
	}
}

// TestSaverStateWriter_LogsFailureOnce (G2-2-M4): a saver.state write that
// fails is logged once per failure streak, and the recovery is logged too.
func TestSaverStateWriter_LogsFailureOnce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	if err := os.WriteFile(dir, []byte("a file where the dir should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	w := saverStateWriter(dir, filepath.Join(root, "claude"), logf)
	w("idle")
	w("dirty")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "SAVER-STATE-WRITE-FAILED state=idle") {
		t.Fatalf("log after two failing writes = %q, want one SAVER-STATE-WRITE-FAILED", lines)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w("saving")
	if len(lines) != 2 || lines[1] != "SAVER-STATE-WRITE-OK state=saving" {
		t.Fatalf("log after recovery = %q", lines)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "saver.state")); err != nil || !strings.Contains(string(b), " saving ") {
		t.Fatalf("saver.state = %q, %v", b, err)
	}
}

// TestPidFileRole: the role reaches the pid file and survives a round trip;
// a pid file from a binary without roles decodes with an empty role.
func TestPidFileRole(t *testing.T) {
	b, err := wire.EncodePidFile(wire.PidFile{PID: 7, Role: wire.RoleServe})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := wire.DecodePidFile(b)
	if err != nil || rec.Role != wire.RoleServe {
		t.Fatalf("round trip: %+v %v", rec, err)
	}
	rec, err = wire.DecodePidFile([]byte(`{"pid":7,"startedAt":"x","socket":"s","tt":"v"}`))
	if err != nil || rec.Role != "" {
		t.Fatalf("role-less pid file: %+v %v", rec, err)
	}
}
