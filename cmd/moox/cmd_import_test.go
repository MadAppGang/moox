package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const fixture = "pane\tapi\t1\t1\t:*\t1\tt\t:/home/user\t1\tzsh\t:\n" +
	"window\tapi\t1\t:w\t1\t:*\t" + `{"V":2,"L":{"t":"p","w":167,"h":51,"x":0,"y":0,"a":true,"i":1,"I":"%3"}}` + "\toff\n" +
	"state\tapi\n"

type world struct {
	root, src string
	env       tmuxtarget.MapEnv
	out, errb bytes.Buffer
	stdin     string
	tty       bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{root: root, env: tmuxtarget.MapEnv{"HOME": filepath.Join(root, "home"),
		"XDG_STATE_HOME": filepath.Join(root, "xs"), "XDG_DATA_HOME": filepath.Join(root, "xd"),
		"XDG_CONFIG_HOME": filepath.Join(root, "xc")}}
	w.src = filepath.Join(root, "resurrect")
	if err := os.MkdirAll(w.src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.src, "tmux_resurrect_20261024T004046.txt"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tmux_resurrect_20261024T004046.txt", filepath.Join(w.src, "last")); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) run(args ...string) int {
	w.out.Reset()
	w.errb.Reset()
	m := &meta{ctx: context.Background(), env: w.env, stdin: strings.NewReader(w.stdin), stdout: &w.out, stderr: &w.errb,
		stdinTTY: w.tty}
	return run(m, append([]string{"import"}, args...))
}

// legacyData is where a default-key publish lands: the scratch XDG_DATA_HOME.
func (w *world) legacyData() string { return filepath.Join(w.root, "xd", "tmux", "resurrect") }

func (w *world) published(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(w.legacyData())
	if err != nil {
		return nil
	}
	var n []string
	for _, e := range ents {
		n = append(n, e.Name())
	}
	return n
}

func TestCmdImport_OutFile(t *testing.T) {
	w := newWorld(t)
	out := filepath.Join(w.root, "x", "imported.json")
	rep := filepath.Join(w.root, "x", "report.txt")
	if code := w.run("--from", w.src, "--out", out, "--report", rep); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s, err := snapshot.Decode(b)
	if err != nil || snapshot.Validate(s) != nil || s.Source != snapshot.SourceImport {
		t.Fatalf("%v %+v", err, s)
	}
	r, _ := os.ReadFile(rep)
	if !strings.Contains(string(r), "panes 1\n") || !strings.Contains(string(r), "windows 1\n") {
		t.Fatalf("report:\n%s", r)
	}
}

func TestCmdImport_Usage(t *testing.T) {
	w := newWorld(t)
	for name, args := range map[string][]string{
		"no-from":         {"--out", "x.json"},
		"no-action":       {"--from", w.src},
		"both":            {"--from", w.src, "--out", "x.json", "--publish", "-L", "x"},
		"publish-no-sock": {"--from", w.src, "--publish"},
		"L-and-S":         {"--from", w.src, "--publish", "-L", "a", "-S", "/tmp/b"},
		"yes-without-pub": {"--from", w.src, "--out", "x.json", "--yes"},
	} {
		if code := w.run(args...); code != exitUsage {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if n := w.published(t); len(n) != 0 {
		t.Fatalf("usage errors wrote %v", n)
	}
}

func TestCmdImport_RefusesMooxJSON(t *testing.T) {
	w := newWorld(t)
	f := filepath.Join(w.root, "moox-20261024T004046.json")
	_ = os.WriteFile(f, []byte(`{"format":"moox-snapshot","version":1}`), 0o600)
	if code := w.run("--from", f, "--out", filepath.Join(w.root, "o.json")); code != exitInvalid {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
}

func TestCmdImport_RefusesResurrectOut(t *testing.T) {
	w := newWorld(t)
	if code := w.run("--from", w.src, "--out", filepath.Join(w.src, "last")); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.Readlink(filepath.Join(w.src, "last")); b != "tmux_resurrect_20261024T004046.txt" {
		t.Fatal("resurrect's last was modified")
	}
}

func TestCmdImport_PublishDefaultNeedsReplaceLive(t *testing.T) {
	w := newWorld(t)
	// -L default with no TMUX_TMPDIR is THE login default socket: the
	// legacy key. Every path still comes from the scratch environment.
	if code := w.run("--from", w.src, "--publish", "-L", "default"); code != exitUsage {
		t.Fatalf("without --replace-live: exit %d", code)
	}
	if !strings.Contains(w.errb.String(), "--replace-live") {
		t.Errorf("stderr: %s", w.errb.String())
	}
	// No terminal to confirm on, no --yes.
	if code := w.run("--from", w.src, "--publish", "-L", "default", "--replace-live"); code != exitUsage {
		t.Fatalf("unconfirmed: exit %d", code)
	}
	// A terminal that says no.
	w.tty, w.stdin = true, "n\n"
	if code := w.run("--from", w.src, "--publish", "-L", "default", "--replace-live"); code != exitAborted {
		t.Fatalf("declined: exit %d", code)
	}
	if n := w.published(t); len(n) != 0 {
		t.Fatalf("a refused publish wrote %v", n)
	}
	w.stdin = "y\n"
	if code := w.run("--from", w.src, "--publish", "-L", "default", "--replace-live"); code != 0 {
		t.Fatalf("confirmed: exit %d: %s", code, w.errb.String())
	}
	if !strings.Contains(w.errb.String(), "superseding no current moox snapshot") {
		t.Errorf("the confirmation must name what it supersedes: %s", w.errb.String())
	}
	w.tty, w.stdin = false, ""
	if code := w.run("--from", w.src, "--publish", "-L", "default", "--replace-live", "--yes"); code != 0 {
		t.Fatalf("--yes: exit %d: %s", code, w.errb.String())
	}
	if !strings.Contains(w.out.String(), "superseded moox-") {
		t.Errorf("stdout: %s", w.out.String())
	}
	n := w.published(t)
	if len(n) < 3 { // two snapshots + moox-last
		t.Fatalf("published %v", n)
	}
	for _, name := range []string{"last", "tmux_resurrect_20261024T004046.txt"} {
		for _, got := range n {
			if got == name {
				t.Fatalf("publish wrote resurrect's %s", name)
			}
		}
	}
}

func TestCmdImport_PublishScratchSocket(t *testing.T) {
	w := newWorld(t)
	w.env["TMUX_TMPDIR"] = filepath.Join(w.root, "tmp")
	_ = os.MkdirAll(w.env["TMUX_TMPDIR"], 0o700)
	if code := w.run("--from", w.src, "--publish", "-L", "moox-test-import"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errb.String())
	}
	if !strings.Contains(w.out.String(), filepath.Join(w.root, "xs", "moox", "sockets")) {
		t.Fatalf("published outside the scratch state: %s", w.out.String())
	}
}
