//go:build tmuxe2e

package share_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// stubGate puts a stand-in `tt` into the server's MOOX_BIN that does what the
// real gate's config-visible half does: @moox-active (1 or 0), @moox-tmux-38
// (1 only on tmux >= 3.8, from the server's parsed version, as cmd/tt's gate
// sets it), and MOOX_ROOT in the server environment. It keeps these tests
// about the config, independent of tt serve.
func stubGate(t *testing.T, s *tmuxtest.Server, active string) {
	t.Helper()
	is38 := "0"
	if s.Version(t).AtLeast(3, 8) {
		is38 = "1"
	}
	script := `#!/bin/sh
sock=""
while [ $# -gt 0 ]; do case "$1" in --tmux-socket) sock=$2; shift 2 ;; *) shift ;; esac; done
[ -n "$sock" ] || exit 0
root=$(tmux -u -S "$sock" show -gv @moox-root)
exec tmux -u -S "$sock" set -g @moox-active ` + active + ` \; set -g @moox-tmux-38 ` + is38 + ` \; set-environment -g MOOX_ROOT "$root"
`
	if err := os.WriteFile(filepath.Join(s.Bin, "tt"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// tmuxtest.Start scrubs PATH from the server's environment; the plugin entry
	// scripts and the gate call `tmux` by name, as they do on a real server.
	s.Run(t, "set-environment", "-g", "PATH", os.Getenv("PATH"))
}

type hookEntry struct {
	name string
	slot int
	cmd  string
}

var hookLine = regexp.MustCompile(`^([a-z-]+)\[([0-9]+)\] (.*)$`)

// loadedHooks is every hook entry the server holds: session (-g), window
// (-gw) and pane (-gp) hooks.
func loadedHooks(t *testing.T, s *tmuxtest.Server) []hookEntry {
	t.Helper()
	var out []hookEntry
	for _, scope := range []string{"-g", "-gw", "-gp"} {
		for _, l := range strings.Split(s.Run(t, "show-hooks", scope), "\n") {
			m := hookLine.FindStringSubmatch(strings.TrimSpace(l))
			if m == nil {
				continue
			}
			n, _ := strconv.Atoi(m[2])
			out = append(out, hookEntry{m[1], n, m[3]})
		}
	}
	return out
}

func prefixKeys(t *testing.T, s *tmuxtest.Server) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for _, l := range strings.Split(s.Run(t, "list-keys", "-T", "prefix"), "\n") {
		f := strings.Fields(l)
		if len(f) >= 5 && f[0] == "bind-key" {
			keys[f[3]] = strings.Join(f[4:], " ")
		}
	}
	return keys
}

var silentTail = regexp.MustCompile(`>\s*/dev/null\s+2>&1\s*\|\|\s*true\s*\\?['"]`)

// TestE2E_Conf_LoadedHooksSilentAndIndexed loads the real moox.conf into an
// isolated server and reads back what tmux actually holds (R2-C1): every hook
// entry sits in a slot of the §5.1 table — so a vendored plugin that registers
// an unindexed hook at runtime is caught too — and every run-shell in hook text
// is backgrounded, silent and `|| true`.
func TestE2E_Conf_LoadedHooksSilentAndIndexed(t *testing.T) {
	s := tmuxtest.Start(t)
	stubGate(t, s, "1")
	s.SourceMoox(t)
	if got := s.Opt(t, "@moox-active"); got != "1" {
		t.Fatalf("@moox-active = %q", got)
	}
	hooks := loadedHooks(t, s)
	slots := map[int]int{}
	for _, h := range hooks {
		slots[h.slot]++
		if h.slot < 100 || h.slot > 105 {
			t.Errorf("hook %s[%d] is outside the moox slot table: %s", h.name, h.slot, h.cmd)
		}
		if strings.Contains(h.cmd, "run-shell") {
			if !strings.Contains(h.cmd, "run-shell -b") || !silentTail.MatchString(h.cmd) {
				t.Errorf("hook %s[%d] runs a run-shell that is not -b, silent and || true: %s", h.name, h.slot, h.cmd)
			}
		}
	}
	for slot := 100; slot <= 105; slot++ {
		if slots[slot] == 0 {
			t.Errorf("no hook registered in slot [%d]", slot)
		}
	}
	// The 3.8-only saver hooks are inside `if -F #{@moox-tmux-38}`: present
	// on 3.8, absent (and never sourced — tmux 3.6 rejects the names with
	// "invalid option") below it.
	found := false
	for _, h := range hooks {
		if h.name == "window-created" && h.slot == 101 {
			found = true
		}
	}
	if is38 := s.Opt(t, "@moox-tmux-38") == "1"; found != is38 {
		t.Errorf("window-created[101] registered=%v on tmux %s (@moox-tmux-38 %q)", found, s.Version(t), s.Opt(t, "@moox-tmux-38"))
	}

	// A reload replaces every indexed entry instead of appending (PV10).
	before := len(hooks)
	s.SourceMoox(t)
	if after := len(loadedHooks(t, s)); after != before {
		t.Errorf("reload changed the number of hook entries %d -> %d", before, after)
	}
}

// The bindings of §3.1 on a loaded server: tmux-sensible bound `prefix a` from
// the prefix options.conf set before it; keys.conf's `h` beats pain-control;
// tmux-jump's `j` beats pain-control; Z and C-s exist on a matching server.
func TestConf_PrefixABoundBySensible(t *testing.T) {
	s := tmuxtest.Start(t)
	stubGate(t, s, "1")
	s.SourceMoox(t)
	keys := prefixKeys(t, s)
	if s.Opt(t, "prefix") != "C-a" {
		t.Fatalf("prefix = %q", s.Opt(t, "prefix"))
	}
	if keys["a"] == "" {
		t.Error("prefix a is not bound: tmux-sensible did not see the C-a prefix at load")
	}
	if !strings.HasPrefix(keys["h"], "split-window -v") {
		t.Errorf("prefix h = %q, want keys.conf's split-window -v", keys["h"])
	}
	if !strings.Contains(keys["j"], "tmux-jump") {
		t.Errorf("prefix j = %q, want tmux-jump", keys["j"])
	}
	if !strings.Contains(keys["Z"], "cc_popup.sh") || !strings.HasPrefix(keys["Z"], "display-popup") {
		t.Errorf("prefix Z = %q, want the freeze manager popup", keys["Z"])
	}
	if !strings.Contains(keys["C-s"], "wait-for -S moox-save") {
		t.Errorf("prefix C-s = %q", keys["C-s"])
	}
	if !strings.Contains(keys["T"], "/scripts/theme.sh") {
		t.Errorf("prefix T = %q, want the theme menu", keys["T"])
	}
	if f := keys["F"]; f != "" && !strings.Contains(f, "fingers") {
		t.Errorf("prefix F = %q, want tmux-fingers (or unbound when it is not installed)", f)
	}
	if hl := s.Opt(t, "history-limit"); hl != "50000" {
		t.Errorf("history-limit = %q", hl)
	}
}

// A server the gate does not match gets keys, theme and plugins, and nothing of
// active.conf: no slots 101–104, no Z or C-s (D7).
func TestE2E_Conf_NotMatchedGetsNoContinuity(t *testing.T) {
	s := tmuxtest.Start(t)
	stubGate(t, s, "0")
	s.SourceMoox(t)
	for _, h := range loadedHooks(t, s) {
		if h.slot >= 101 && h.slot <= 104 {
			t.Errorf("non-matching server holds %s[%d]", h.name, h.slot)
		}
	}
	keys := prefixKeys(t, s)
	if keys["Z"] != "" || keys["C-s"] != "" {
		t.Errorf("non-matching server binds Z=%q C-s=%q", keys["Z"], keys["C-s"])
	}
	if !strings.Contains(keys["T"], "theme.sh") {
		t.Error("non-matching server lost the theme")
	}
}
