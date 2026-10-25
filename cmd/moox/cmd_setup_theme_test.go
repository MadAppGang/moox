package main

// I-1 (black-box Phase 6): `moox setup` runs the shipped `theme.sh detect`,
// which used to call a bare `tmux list-clients` with $TMUX empty — the user's
// DEFAULT server. These tests replace both tmux binaries with loggers: the one
// on PATH must never run, a --dry-run contacts no server at all, and a real
// setup reaches only the resolved socket through MOOX_TMUX_BIN.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loggingTmux writes a fake tmux at rel that appends "<argv0>|<args>" to log
// and answers -V like tmux 3.8. It never talks to a server.
func loggingTmux(h *fakeHome, rel, log string) string {
	return h.write(rel, "#!/bin/sh\nprintf '%s|%s\\n' \"$0\" \"$*\" >> '"+log+"'\n"+
		"case \"$1\" in -V) echo 'tmux 3.8' ;; esac\nexit 0\n", 0o755)
}

func tmuxCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestSetup_ThemeDetectReachesOnlyTheResolvedServer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		tmuxV string // $TMUX of the caller: a server moox did not select
	}{
		{"dry-run", []string{"setup", "--dry-run", "--yes"}, ""},
		{"yes", []string{"setup", "--yes"}, ""},
		{"yes-foreign-TMUX", []string{"setup", "--yes"}, "/elsewhere/default,1,0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHome(t)
			h.write("home/.zshrc", "# z\n", 0o644)
			log := filepath.Join(h.root, "tmux-calls.log")
			pathTmux := loggingTmux(h, "bin/tmux", log)
			moox := loggingTmux(h, "selected/tmux", log)
			sock := filepath.Join(h.root, "tmux-tmp", "selected.sock")
			h.extra = []string{"MOOX_TMUX_BIN=" + moox, "MOOX_TMUX_SOCKET=" + sock}
			if tc.tmuxV != "" {
				h.extra = append(h.extra, "TMUX="+tc.tmuxV)
			}
			code, out := h.run(tc.args...)
			mustCode(t, code, out, 0)

			calls := tmuxCalls(t, log)
			sawClients := false
			for _, c := range calls {
				bin, args, _ := strings.Cut(c, "|")
				if bin == pathTmux {
					t.Errorf("PATH's tmux ran (no socket of moox's choosing): %q", c)
					continue
				}
				if args == "-V" {
					continue // the version probe contacts no server
				}
				if tc.name == "dry-run" {
					t.Errorf("--dry-run contacted a tmux server: %q", c)
					continue
				}
				if !strings.HasPrefix(args, "-S "+sock+" ") {
					t.Errorf("tmux call does not target the resolved socket %s: %q", sock, c)
				}
				if strings.Contains(args, "list-clients") {
					sawClients = true
				}
			}
			if tc.name != "dry-run" && !sawClients {
				t.Errorf("theme detection never asked the selected server for its clients: %q", calls)
			}
		})
	}
}
