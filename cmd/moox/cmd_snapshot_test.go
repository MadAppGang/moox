package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// TestSnapshot_ShowAndUnpin: show lists moox-last and every pin with its
// recovery command; unpin releases one (or --all) under the lock and
// re-sets @moox-warning on the server.
func TestSnapshot_ShowAndUnpin(t *testing.T) {
	s := tmuxtest.Start(t)
	p := s.Paths(t)
	a := fixtureSnapshot(t, p.Snapshots, "moox-20261001T100000.json")
	b := fixtureSnapshot(t, p.Snapshots, "moox-20261002T100000.json")
	if err := os.WriteFile(filepath.Join(p.Snapshots, "moox-last"), []byte(filepath.Base(b)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.State, "pins"), []byte(filepath.Base(a)+"\n"+filepath.Base(b)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Run(t, "set-option", "-g", "@moox-warning", "restore INCOMPLETE: 2 pinned — moox snapshot show")
	env := s.Env()
	code, out := runIn(t, env, "snapshot", "show", "-S", s.T.Socket)
	// G2-H4: every recovery command names the inspected server, quoted.
	sock := shArg(s.T.Socket)
	if code != 0 || !strings.Contains(out, "moox-last: "+filepath.Base(b)) ||
		!strings.Contains(out, "moox restart -S "+sock+" --snapshot "+shArg(a)) ||
		!strings.Contains(out, "moox snapshot unpin -S "+sock+" "+filepath.Base(b)) {
		t.Fatalf("show: %d\n%s", code, out)
	}
	if shArg("/a b/it's") != `'/a b/it'\''s'` || shArg("/tmp/x.json") != "/tmp/x.json" {
		t.Fatalf("shArg quoting")
	}
	if code, out = runIn(t, env, "snapshot", "unpin", "-S", s.T.Socket, filepath.Base(a)); code != 0 || strings.TrimSpace(out) != "1 pinned" {
		t.Fatalf("unpin: %d %q", code, out)
	}
	if w := s.Opt(t, "@moox-warning"); w != "restore INCOMPLETE: 1 pinned — moox snapshot show" {
		t.Fatalf("@moox-warning = %q", w)
	}
	if code, out = runIn(t, env, "snapshot", "unpin", "-S", s.T.Socket, filepath.Base(a)); code != exitNotApplic {
		t.Fatalf("unpin of an unpinned file: %d %q", code, out)
	}
	if code, out = runIn(t, env, "snapshot", "unpin", "-S", s.T.Socket, "--all"); code != 0 || strings.TrimSpace(out) != "0 pinned" {
		t.Fatalf("unpin --all: %d %q", code, out)
	}
	if w := s.Opt(t, "@moox-warning"); w != "" {
		t.Fatalf("@moox-warning after unpin --all = %q", w)
	}
	code, out = runIn(t, env, "snapshot", "show", "-S", s.T.Socket, "--json")
	if code != 0 || !strings.Contains(out, `"pins": []`) {
		t.Fatalf("show --json: %d\n%s", code, out)
	}
}
