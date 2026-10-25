package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// layoutV1 is tmux's v1 layout string for one 80x24 pane with id n.
func layoutV1(n int) string {
	body := fmt.Sprintf("80x24,0,0,%d", n)
	csum := 0
	for _, c := range []byte(body) {
		csum = (csum >> 1) + ((csum & 1) << 15)
		csum += int(c)
		csum &= 0xffff
	}
	return fmt.Sprintf("%04x,%s", csum, body)
}

// fixtureSnapshot is a valid two-window snapshot of session "a".
func fixtureSnapshot(t *testing.T, dir, name string) string {
	t.Helper()
	s := &snapshot.Snapshot{Format: snapshot.FormatName, Version: 1, Source: snapshot.SourceSave,
		Client: &snapshot.Client{Session: "a"}, CreatedAt: "2026-10-01T10:00:00Z"}
	s.Sessions = []snapshot.Session{{Name: "a", ActiveWindow: 0, LastWindow: -1}}
	for w := 0; w < 2; w++ {
		id := "%" + fmt.Sprint(w+1)
		s.Panes = append(s.Panes, snapshot.Pane{Key: snapshot.PaneKey("a", w, 0), Session: "a", Window: w, Index: 0,
			SavedID: id, Cwd: "/tmp/w" + fmt.Sprint(w), Role: snapshot.RoleShell, Active: true})
		s.Windows = append(s.Windows, snapshot.Window{Session: "a", Index: w, Name: "w", Layout: layoutV1(w + 1),
			LayoutFormat: snapshot.LayoutV1, AutomaticRename: snapshot.AutoRenameUnset})
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	b, err := snapshot.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRestore_DryRunIsLocal (§6.4a step 0, R2-H5): --dry-run reads and
// validates the snapshot and prints the plan without contacting or starting
// a server: the socket it names never appears.
func TestRestore_DryRunIsLocal(t *testing.T) {
	h := newFakeHome(t)
	sock := filepath.Join(h.root, "nosuch")
	snap := fixtureSnapshot(t, filepath.Join(h.root, "elsewhere"), "moox snap.json")

	code, out := h.run("restore", "--dry-run", "-S", sock, "--snapshot", snap)
	mustCode(t, code, out, 0)
	for _, want := range []string{"moox restore --dry-run " + snap, "1 sessions, 2 windows, 2 panes", "/tmp/w0", "/tmp/w1"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	// Relative to the requester's cwd.
	rel, _ := filepath.Rel(h.home, snap)
	if code, out = h.run("restore", "--dry-run", "-S", sock, "--snapshot", rel); code != 0 {
		t.Fatalf("relative path: %d\n%s", code, out)
	}
	if _, err := os.Lstat(sock); err == nil {
		t.Fatal("--dry-run started a server")
	}
	// moox-last of the socket's own key, when no --snapshot is given.
	p, err := engineFor(h, sock)
	if err != nil {
		t.Fatal(err)
	}
	last := fixtureSnapshot(t, p.Paths().Snapshots, "moox-20261001T100000.json")
	if err := os.WriteFile(filepath.Join(p.Paths().Snapshots, "moox-last"), []byte(filepath.Base(last)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = h.run("restore", "--dry-run", "-S", sock)
	mustCode(t, code, out, 0)
	if !strings.Contains(out, last) {
		t.Fatalf("dry run did not use moox-last:\n%s", out)
	}
	if _, err := os.Lstat(sock); err == nil {
		t.Fatal("--dry-run started a server")
	}
	// An invalid file is exit 4.
	bad := filepath.Join(h.root, "bad.json")
	if err := os.WriteFile(bad, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out = h.run("restore", "--dry-run", "-S", sock, "--snapshot", bad); code != exitInvalid {
		t.Fatalf("invalid snapshot: exit %d\n%s", code, out)
	}
}

func engineFor(h *fakeHome, sock string) (*engine.Engine, error) {
	t, err := tmuxtarget.Resolve(envMap(h.environ()), "MOOX", sock, "")
	if err != nil {
		return nil, err
	}
	return engine.New(engine.Options{Target: t, Env: envMap(h.environ())})
}

// TestRestoreRequest_RelativePathResolvedByRequester (R2-L8): the requester
// makes --snapshot absolute against ITS cwd and validates it before asking.
func TestRestoreRequest_RelativePathResolvedByRequester(t *testing.T) {
	h := newFakeHome(t)
	snap := fixtureSnapshot(t, filepath.Join(h.home, "snaps"), "s.json")
	e, err := engineFor(h, filepath.Join(h.root, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := saver.ResolveSnapshot(e, "snaps/s.json", h.home)
	if err != nil || got != snap {
		t.Fatalf("ResolveSnapshot = %q, %v; want %q", got, err, snap)
	}
	if _, err := saver.ResolveSnapshot(e, "snaps/missing.json", h.home); err == nil {
		t.Fatal("a missing snapshot resolved")
	}
}

// TestRestart_DefaultNeedsTypedName (§8.5 step 1, PR-H2): a plain "y" does
// not restart the legacy default server; typing its name does.
func TestRestart_DefaultNeedsTypedName(t *testing.T) {
	var out bytes.Buffer
	if confirmRestart(strings.NewReader("y\n"), &out, "/tmp/tmux-501/default", true, 14, 66) {
		t.Fatal(`"y" restarted the default server`)
	}
	if !strings.Contains(out.String(), "ends 14 sessions / 66 panes on server `default`") || !strings.Contains(out.String(), "Type its name") {
		t.Fatalf("prompt = %q", out.String())
	}
	if !confirmRestart(strings.NewReader("default\n"), &out, "/tmp/tmux-501/default", true, 14, 66) {
		t.Fatal("typing the name did not confirm")
	}
	if !confirmRestart(strings.NewReader("y\n"), &out, "/tmp/x/work", false, 1, 2) {
		t.Fatal(`"y" did not confirm a non-default server`)
	}
	if confirmRestart(strings.NewReader("\n"), &out, "/tmp/x/work", false, 1, 2) {
		t.Fatal("an empty answer confirmed")
	}
}

// Without a terminal, restart needs --yes (exit 2) and touches nothing; with
// no server it is exit 3.
func TestRestart_NoTTYNeedsYes(t *testing.T) {
	h := newFakeHome(t)
	if code, out := h.run("restart", "-S", filepath.Join(h.root, "sock")); code != exitNotApplic {
		t.Fatalf("no server: exit %d\n%s", code, out)
	}
	s := tmuxtest.Start(t)
	pid := s.Run(t, "display-message", "-p", "#{pid}")
	code, out := runIn(t, s.Env(), "restart", "-S", s.T.Socket)
	if code != exitUsage || !strings.Contains(out, "--yes is required") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s.Run(t, "display-message", "-p", "#{pid}") != pid || s.Opt(t, "@moox-restoring") != "" {
		t.Fatal("restart without --yes touched the server")
	}
}

func TestVersionAndRoot(t *testing.T) {
	h := newFakeHome(t)
	code, out := h.run("--version")
	if code != 0 || !strings.HasPrefix(out, "moox ") {
		t.Fatalf("--version: %d %q", code, out)
	}
	code, out = h.run("root")
	if code != 0 || strings.TrimSpace(out) != h.share {
		t.Fatalf("root: %d %q want %q", code, out, h.share)
	}
}
