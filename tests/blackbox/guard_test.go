package blackbox_test

import (
	"os"
	"path/filepath"
	"testing"
)

// Harness self-test (contract/test-environment.md): the tmux guard must deny
// every foreign socket, kill-server and tool-client run-shell, while letting a
// subcommand flag such as "resize-pane -L 5" through. Exit 97 = guard denial.
func TestHarness_GuardDeniesForeignSocketsButNotSubcommandFlags(t *testing.T) {
	e := newSandbox(t)
	owned := "moox-bb-" + e.nonce + "-guard"
	denials := filepath.Join(e.base, "control", "guard-denials")
	cases := []struct {
		name   string
		env    map[string]string
		args   []string
		denied bool
	}{
		{"bare command, no TMUX", nil, []string{"list-sessions"}, true},
		{"default name", nil, []string{"-L", "default", "list-sessions"}, true},
		{"attached foreign name", nil, []string{"-Ldefault", "list-sessions"}, true},
		{"foreign path", nil, []string{"-S", "/private/tmp/tmux-501/default", "list-sessions"}, true},
		{"lexical alias outside root", nil, []string{"-S", "/tmp/elsewhere/sock", "list-sessions"}, true},
		{"hidden in a cluster", nil, []string{"-uL", "default", "list-sessions"}, true},
		{"foreign TMUX only", map[string]string{"TMUX": "/private/tmp/tmux-501/default,1,0"}, []string{"list-sessions"}, true},
		{"kill-server on owned socket", nil, []string{"-L", owned, "kill-server"}, true},
		{"run-shell from the test process", nil, []string{"-L", owned, "run-shell", "true"}, true},
		{"subcommand -L flag on owned socket", nil, []string{"-L", owned, "-f", "/dev/null", "resize-pane", "-L", "5"}, false},
		{"owned canonical path", nil, []string{"-S", filepath.Join(e.base, "tmux", "x.sock"), "list-sessions"}, false},
	}
	for _, c := range cases {
		r := e.run(e.tmux, c.env, "", c.args...)
		if got := r.code == 97; got != c.denied {
			t.Errorf("%s: exit=%d denied=%v, want denied=%v (stderr %q)", c.name, r.code, got, c.denied, r.err)
		}
	}
	// The denials above are intended; clear them so cleanup's no-denial
	// assertion keeps guarding every other test.
	if err := os.Remove(denials); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
