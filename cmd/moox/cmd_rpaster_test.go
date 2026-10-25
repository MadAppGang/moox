package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// TestRpasterVerb_BundledThenPath (V10): `moox rpaster` runs moox's own
// libexec/rpaster with the caller's arguments and exit status; with no bundled
// copy it falls back to an rpaster on PATH; with neither it fails.
func TestRpasterVerb_BundledThenPath(t *testing.T) {
	d := t.TempDir()
	share := filepath.Join(d, "keg", "share", "moox")
	lib := filepath.Join(d, "keg", "libexec", "rpaster")
	pathBin := filepath.Join(d, "pathbin")
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(share, "moox.conf"), "# test\n")
	write(lib, "#!/bin/sh\necho \"bundled $*\"\nexit 7\n")
	write(filepath.Join(pathBin, "rpaster"), "#!/bin/sh\necho \"onpath $*\"\n")

	runIt := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		env := tmuxtarget.MapEnv{"MOOX_ROOT": share, "PATH": pathBin + ":/usr/bin:/bin", "HOME": d}
		m := &meta{ctx: context.Background(), env: env, environ: []string{"PATH=/usr/bin:/bin", "HOME=" + d},
			stdin: strings.NewReader(""), stdout: &out, stderr: &errb}
		code := run(m, append([]string{"rpaster"}, args...))
		return code, out.String(), errb.String()
	}

	if code, out, _ := runIt("--which"); code != exitOK || out != lib+" (bundled)\n" {
		t.Fatalf("--which: code=%d out=%q", code, out)
	}
	if code, out, errs := runIt("doctor", "--x"); code != 7 || out != "bundled doctor --x\n" {
		t.Fatalf("bundled run: code=%d out=%q err=%q", code, out, errs)
	}
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runIt("version"); code != exitOK || out != "onpath version\n" {
		t.Fatalf("PATH fallback: code=%d out=%q", code, out)
	}
	if err := os.Remove(filepath.Join(pathBin, "rpaster")); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runIt("version"); code != exitError || !strings.Contains(errs, "no rpaster") {
		t.Fatalf("none: code=%d err=%q", code, errs)
	}
}
