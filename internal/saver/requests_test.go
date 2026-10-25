package saver

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServerEnv_KeepsStubFlag (G2-H7): the env a server is started with
// (moox start, ordinary moox restart) keeps MOOX_STUB_COMMANDS=1 — the boot
// daemon reads it from there — while every other MOOX_* and TMUX go.
func TestServerEnv_KeepsStubFlag(t *testing.T) {
	got := strings.Join(ServerEnv([]string{"PATH=/bin", "MOOX_STUB_COMMANDS=1", "MOOX_ROOT=/x", "MOOX_BIN=/b",
		"TMUX=/s,1,0", "TMUX_PANE=%1", "MOOX_NO_AUTO_RESTORE=1"}), " ")
	if got != "PATH=/bin MOOX_STUB_COMMANDS=1 MOOX_BIN=/b" {
		t.Fatalf("ServerEnv = %q", got)
	}
	if got := strings.Join(ServerEnv([]string{"MOOX_STUB_COMMANDS=0"}), " "); got != "" {
		t.Fatalf("a non-1 stub value is dropped like any MOOX_*: %q", got)
	}
}

// TestValidSnapshot_NeverBlocksOrSlurps (G1-M3): the requester's path is
// read in the daemon's single event loop. A FIFO must fail at once (an open
// or read of it would block every save), and an oversized file is refused
// without being read.
func TestValidSnapshot_NeverBlocksOrSlurps(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- validSnapshot(fifo) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("validSnapshot blocked on a FIFO")
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, maxRequestedSnapshot+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validSnapshot(big); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("oversized: %v", err)
	}
	if err := validSnapshot(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}
