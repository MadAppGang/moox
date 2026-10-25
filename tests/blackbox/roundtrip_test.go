package blackbox_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TEST-26, REQ-18/REQ-19/REQ-37: stop via session teardown, then start COLD
// with the shipped configuration. Never call explicit restore on the new server;
// doing so would fail to test the product's boot path.
func TestRestore_ColdShippedConfigRestoresSessionsWindowsPanesCwdsAndLiteralTitles(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("roundtrip", "SELF", false)
	s.makeRestorable()
	marker := filepath.Join(e.base, "must-not-be-created")
	paths := []string{
		filepath.Join(e.home, "cwd with spaces"),
		filepath.Join(e.home, "cwd#hash"),
		filepath.Join(e.home, "cwd;semi"),
		filepath.Join(e.home, "cwd\\backslash"),
	}
	for _, path := range paths {
		mustMkdir(t, path)
	}
	// Replace the two initial panes' cwd via safe /bin/sh input, using shell
	// quoting; no user's startup files and no real launch recordings are used.
	initial := strings.Fields(s.mustTmux("list-panes", "-a", "-F", "#{pane_id}"))
	for i, id := range initial {
		s.mustTmux("send-keys", "-t", id, "-l", "cd "+shQuote(paths[i%len(paths)]))
		s.mustTmux("send-keys", "-t", id, "Enter")
	}
	for i, name := range []string{"mnemex", "claudish"} {
		s.mustTmux("new-session", "-d", "-s", name, "-x", "180", "-y", "50", "-c", paths[i+1], "/bin/sh")
		s.mustTmux("new-window", "-d", "-t", name+":", "-n", "fix; test # literal", "-c", paths[i+2], "/bin/sh")
	}
	pane := s.mustTmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "magus:",
		"-c", paths[2], "/bin/sh")
	s.mustTmux("select-pane", "-t", pane, "-T", "split; # literal \\")
	windows := strings.Fields(s.mustTmux("list-windows", "-a", "-F", "#{window_id}"))
	s.mustTmux("rename-window", "-t", windows[0], "#(touch "+marker+"); fix # literal")
	ids := strings.Fields(s.mustTmux("list-panes", "-a", "-F", "#{pane_id}"))
	if len(ids) != 7 || len(windows) != 6 {
		t.Fatalf("fixture shape=%d panes/%d windows, want exactly 7/6", len(ids), len(windows))
	}
	for i, id := range ids {
		title := []string{"x;", "#(touch " + marker + ")", "hash#middle;end", "literal\\backslash"}[i%4]
		s.mustTmux("select-pane", "-t", id, "-T", title)
	}
	// Wait on the actual cwd, not a fixed delay after typing cd into a shell.
	for i, id := range initial {
		want := paths[i%len(paths)]
		eventually(t, 5*time.Second, "fixture pane cwd", func() (bool, string) {
			got := s.mustTmux("display-message", "-p", "-t", id, "#{pane_current_path}")
			return got == want, got
		})
	}
	e.rememberPanePIDs(s)
	want := s.view()
	if len(want) != 7 {
		t.Fatalf("empty or incomplete pre-save oracle: %q", want)
	}
	pointer, _ := s.save()
	saved := string(readFile(t, pointer))
	s.stop()
	// Same isolated socket and XDG roots; no NO_AUTO_RESTORE on this start.
	cold := e.newServer("roundtrip", "SELF", true)
	cold.requireActive("1")
	// @moox-boot-result carries the boot (Claude-resume) verdict: with only
	// /bin/sh panes there is nothing to resume, so NOTHING is a legitimate
	// verdict of a completed restore. SKIPPED/HANDOVER/INCOMPLETE are not; the
	// byte-identical view below is the restore oracle either way.
	eventually(t, 35*time.Second, "automatic cold restore to complete", func() (bool, string) {
		r := cold.tmuxRun("show-options", "-gqv", "@moox-boot-result")
		guard := cold.tmuxRun("show-options", "-gqv", "@moox-restoring")
		v := strings.TrimSpace(r.out)
		return (v == "PASS" || v == "NOTHING") && strings.TrimSpace(guard.out) == "", "boot-result=" + r.out + r.err
	})
	eventually(t, 10*time.Second, "byte-identical layout/cwd/title round trip", func() (bool, string) {
		got := cold.view()
		return reflect.DeepEqual(got, want), "got=" + strings.Join(got, "\n") + "\nwant=" + strings.Join(want, "\n")
	})
	if got := cold.mustTmux("list-sessions", "-F", "#{session_name}"); strings.Contains(got, "bootstrap") {
		t.Errorf("cold restore left bootstrap session behind: %q", got)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Errorf("literal #()/semicolon metadata executed a command: marker stat=%v", err)
	}
	if got := string(readFile(t, pointer)); got != saved {
		t.Errorf("cold start clobbered the restorable pointer: %q != %q", got, saved)
	}
	e.rememberPanePIDs(cold)
}
