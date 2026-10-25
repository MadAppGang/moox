package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/setup"
	"github.com/MadAppGang/moox/internal/setup/manifest"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// separateWorld is a scratch HOME/XDG with a separate-mode setup.json, no
// $TMUX, no MOOX_TMUX_SOCKET and an empty TMUX_TMPDIR: nothing but the
// manifest can choose the socket.
func separateWorld(t *testing.T) (tmuxtarget.MapEnv, string) {
	t.Helper()
	root := t.TempDir()
	tmp := filepath.Join(root, "tmux-tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	env := tmuxtarget.MapEnv{"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": filepath.Join(root, "xs"),
		"XDG_DATA_HOME": filepath.Join(root, "xd"), "XDG_CONFIG_HOME": filepath.Join(root, "xc"),
		"TMUX_TMPDIR": tmp, "PATH": os.Getenv("PATH")}
	b, err := manifest.Encode(&manifest.Manifest{Version: manifest.Version, Mode: string(setup.Separate)})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "xc", "moox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := tmuxtarget.LabelPath(env, setup.SeparateSocket)
	if err != nil {
		t.Fatal(err)
	}
	return env, want
}

// TestSave_SeparateModeTargetsMooxSocket (G2-H2): a bare `moox save` outside
// tmux in separate mode addresses the -L moox server, as every other verb
// does — not the default socket. (No server runs: the error names the
// socket the save went to.)
func TestSave_SeparateModeTargetsMooxSocket(t *testing.T) {
	env, want := separateWorld(t)
	var out, errb bytes.Buffer
	m := &meta{ctx: context.Background(), env: env, stdin: strings.NewReader(""), stdout: &out, stderr: &errb}
	code := run(m, []string{"save", "--wait", "1s"})
	if code == exitOK || !strings.Contains(errb.String(), want+":") {
		t.Fatalf("exit %d, stderr %q; want an attempt on %s", code, errb.String(), want)
	}
	// and --attach-live's import target resolves the same way
	tg, err := resolveTarget(m, "", "", true)
	if err != nil || tg.Socket != want {
		t.Fatalf("import --attach-live target %q (%v), want %s", tg.Socket, err, want)
	}
	// an explicit socket still wins
	tg, err = resolveTarget(m, "other", "", true)
	if err != nil || filepath.Base(tg.Socket) != "other" {
		t.Fatalf("-L other: %q %v", tg.Socket, err)
	}
}

// TestIsTTY_DevNullAndPipesAreNot (G2-M2): /dev/null is a character device
// but no terminal; neither is a pipe or a regular file.
func TestIsTTY_DevNullAndPipesAreNot(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for name, x := range map[string]*os.File{"/dev/null": null, "pipe": r, "file": f} {
		if isTTY(x) {
			t.Errorf("%s reported as a terminal", name)
		}
	}
}

// TestStart_FirstRunInsideTmuxWritesNothing (G2-H12): the first `moox` run
// inside tmux used to run setup --yes, rewriting ~/.tmux.conf under the
// running server before any warning. It now only prints the dry run and
// asks for an explicit `moox setup`.
func TestStart_FirstRunInsideTmuxWritesNothing(t *testing.T) {
	share, err := mooxroot.DevShare(".")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := tmuxtarget.MapEnv{"HOME": home, "XDG_STATE_HOME": filepath.Join(root, "xs"),
		"XDG_DATA_HOME": filepath.Join(root, "xd"), "XDG_CONFIG_HOME": filepath.Join(root, "xc"),
		"TMUX_TMPDIR": root, "PATH": os.Getenv("PATH"), "MOOX_ROOT": share, "SHELL": "/bin/sh",
		"TMUX": filepath.Join(root, "tmux-0", "other") + ",1,0"}
	var out, errb bytes.Buffer
	m := &meta{ctx: context.Background(), env: env, stdin: strings.NewReader(""), stdout: &out, stderr: &errb}
	code := run(m, []string{"--dry-attach"})
	if code != exitNotApplic || !strings.Contains(errb.String(), "moox setup") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out.String(), errb.String())
	}
	for _, p := range []string{filepath.Join(home, ".tmux.conf"), filepath.Join(root, "xc", "moox", manifest.FileName)} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written by a first run inside tmux", p)
		}
	}
}
