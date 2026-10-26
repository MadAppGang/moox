package main

import (
	"os"
	"path/filepath"
	"testing"
)

// systemPathWithout is a PATH directory that holds every command of the
// system directories (/usr/bin, /bin) except the named ones, as symlinks
// under a temp dir. A test that needs "jq is not on PATH" cannot just leave
// /usr/bin off PATH: macOS ships /usr/bin/jq, and on a merged-/usr Linux
// (every current Ubuntu) /bin IS /usr/bin, so `PATH=<bin>:/bin` still finds
// the runner's jq. The farm keeps the rest of the userland reachable, so the
// command under test behaves as it does on a real PATH.
func systemPathWithout(t *testing.T, exclude ...string) string {
	t.Helper()
	skip := map[string]bool{}
	for _, n := range exclude {
		skip[n] = true
	}
	dir := filepath.Join(t.TempDir(), "sysbin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sys := range []string{"/usr/bin", "/bin"} {
		entries, err := os.ReadDir(sys)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if skip[name] {
				continue
			}
			target := filepath.Join(sys, name)
			if fi, err := os.Stat(target); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
				continue
			}
			// The first directory wins, as on PATH; a name already linked
			// from /usr/bin is not linked again from /bin.
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
	}
	for _, n := range exclude {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("systemPathWithout: %s is still in %s", n, dir)
		}
	}
	return dir
}
