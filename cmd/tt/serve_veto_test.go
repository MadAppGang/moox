package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// TestGate_LegacyVetoLifecycle (C1, R3-C4): on the legacy default key the
// gate writes $HOME/.config/tmux-claude/moox-excluded when it does not match
// and removes it when it does, so a pane with no MOOX_* env (pre-cutover)
// stops writing exactly while the default server is excluded; a removal with
// no veto is silent. Paths come from a scratch HOME (Legacy is forced: the
// real default socket is never touched).
func TestGate_LegacyVetoLifecycle(t *testing.T) {
	home := t.TempDir()
	p, err := sockets.For(filepath.Join(home, "tmux-501", "x"), tmuxtarget.MapEnv{"HOME": home})
	if err != nil {
		t.Fatal(err)
	}
	p.Legacy = true
	var log strings.Builder
	g := &gate{paths: p, claude: filepath.Join(home, ".claude"), log: &log}
	exists := func() bool { _, err := os.Lstat(p.Veto); return err == nil }

	g.removeVeto() // nothing to remove: no error, no line
	if exists() || strings.Contains(log.String(), "FAILED") {
		t.Fatalf("remove with no veto: %s", log.String())
	}
	g.writeVeto()
	if !exists() || !strings.Contains(log.String(), "VETO-WRITTEN") {
		t.Fatalf("veto not written: %s", log.String())
	}
	if fi, _ := os.Stat(p.Veto); fi.Mode().Perm() != 0o600 {
		t.Fatalf("veto mode %v", fi.Mode().Perm())
	}
	g.removeVeto()
	if exists() || !strings.Contains(log.String(), "VETO-REMOVED") {
		t.Fatalf("veto not removed: %s", log.String())
	}
	g.log = io.Discard
}
