package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFindStale_NeverASavedWorkspace (G2-H9): a per-socket key whose server
// has been down for over 14 days (a separate-mode -L moox after a reboot,
// two weeks away) but holds moox-last, snapshots or pins is not stale, so
// `doctor --fix stale|all` never deletes the only copy of a workspace. A
// key with only debris still is.
func TestFindStale_NeverASavedWorkspace(t *testing.T) {
	root := t.TempDir()
	xs := filepath.Join(root, "xs")
	env := map[string]string{"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": xs}
	mk := func(rel, content string) {
		p := filepath.Join(xs, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("moox/sockets/moox@11111111/snapshots/moox-last", "moox-20261001T100000.json\n")
	mk("moox/sockets/moox@11111111/snapshots/moox-20261001T100000.json", "{}")
	mk("tt/sockets/moox@11111111/observer.log", "x")
	mk("moox/sockets/pinned@22222222/pins", "moox-20261001T100000.json\n")
	mk("moox/sockets/debris@33333333/log/daemon.log", "x")
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = filepath.Walk(xs, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })

	e := Env{Getenv: func(k string) string { return env[k] }}
	got := map[string]bool{}
	for _, s := range FindStale(e) {
		got[filepath.Base(filepath.Dir(s.Parent))+"/"+s.Key] = true
	}
	if got["moox/moox@11111111"] || got["tt/moox@11111111"] {
		t.Fatalf("a key holding moox-last is stale: %v", got)
	}
	if got["moox/pinned@22222222"] {
		t.Fatalf("a key holding pins is stale: %v", got)
	}
	if !got["moox/debris@33333333"] {
		t.Fatalf("a debris-only key is not stale: %v", got)
	}
}

// TestLegacyText_KeysOnlyResurrectContinuum (G2-H13, §8.4): hooks running
// from ~/.tmux/ are legacy (the old config's hooks), but a key binding is
// legacy only when it references tmux-resurrect or continuum — the user's
// own `bind X run ~/.tmux/scripts/x.sh` is left alone.
func TestLegacyText_KeysOnlyResurrectContinuum(t *testing.T) {
	home := "/home/user"
	if !legacyText("run-shell ~/.tmux/plugins/tmux-claude-continuity/scripts/x.sh", home) ||
		!legacyText("run-shell /home/user/.tmux/theme.sh", home) {
		t.Fatal("legacy hooks under ~/.tmux/ not found")
	}
	for _, own := range []string{"run-shell ~/.tmux/scripts/mine.sh", "run-shell /home/user/.tmux/x.sh", "display-popup"} {
		if legacyKeyText(own) {
			t.Errorf("the user's binding %q counted as legacy", own)
		}
	}
	for _, leg := range []string{"run-shell /home/user/.tmux/plugins/tmux-resurrect/scripts/restore.sh", "run-shell ~/.tmux/plugins/tmux-continuum/x"} {
		if !legacyKeyText(leg) {
			t.Errorf("resurrect/continuum binding %q missed", leg)
		}
	}
}

// TestCheckRpaster_SaysWhichOne (V10): doctor names the rpaster prefix V
// relies on — moox's bundled libexec copy first — and a separately installed
// PATH rpaster (madappgang/tap/rpaster) as somebody else's.
func TestCheckRpaster_SaysWhichOne(t *testing.T) {
	d := t.TempDir()
	share := filepath.Join(d, "opt", "moox", "share", "moox")
	lib := filepath.Join(d, "opt", "moox", "libexec", "rpaster")
	other := filepath.Join(d, "bin", "rpaster")
	for _, p := range []string{lib, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	onPath := func(name string) (string, error) {
		if name == "rpaster" {
			return other, nil
		}
		return "", os.ErrNotExist
	}
	none := func(string) (string, error) { return "", os.ErrNotExist }

	r := checkRpaster(Env{Root: share}, onPath)
	if len(r) != 2 || r[0].Level != OK || !strings.Contains(r[0].Message, lib) || !strings.Contains(r[0].Message, "bundled") ||
		r[1].Level != Info || !strings.Contains(r[1].Message, other) {
		t.Fatalf("bundled + PATH: %+v", r)
	}
	r = checkRpaster(Env{Root: share}, none)
	if len(r) != 1 || r[0].Level != OK || !strings.Contains(r[0].Message, lib) {
		t.Fatalf("bundled only: %+v", r)
	}
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	r = checkRpaster(Env{Root: share}, onPath)
	if len(r) != 1 || r[0].Level != OK || !strings.Contains(r[0].Message, other) || !strings.Contains(r[0].Message, "on PATH") {
		t.Fatalf("PATH fallback: %+v", r)
	}
	r = checkRpaster(Env{Root: share}, none)
	if len(r) != 1 || r[0].Level != Info || r[0].Check != "rpaster" {
		t.Fatalf("none: %+v", r)
	}
}
