package mooxroot_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/mooxroot"
)

func mkfile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStable_Brew(t *testing.T) {
	root := realTemp(t)
	exe := filepath.Join(root, "homebrew", "Cellar", "moox", "0.1.0", "bin", "moox")
	mkfile(t, exe)
	// brew's bin/ symlink into the Cellar resolves to the same answer
	link := filepath.Join(root, "homebrew", "bin", "moox")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "homebrew", "opt", "moox", "share", "moox")
	for _, p := range []string{exe, link} {
		got, err := mooxroot.Stable(p, "")
		if err != nil || got != want {
			t.Fatalf("Stable(%s) = %q, %v; want %q", p, got, err, want)
		}
	}
	r, err := mooxroot.Resolve(exe, "")
	if err != nil || r.Bin != filepath.Join(root, "homebrew", "opt", "moox", "bin") {
		t.Fatalf("Resolve = %+v, %v", r, err)
	}
}

func TestStable_Curl(t *testing.T) {
	home := realTemp(t)
	exe := filepath.Join(home, ".local", "share", "moox", "0.1.0", "bin", "moox")
	mkfile(t, exe)
	want := filepath.Join(home, ".local", "share", "moox", "current", "share", "moox")
	got, err := mooxroot.Stable(exe, "")
	if err != nil || got != want {
		t.Fatalf("Stable = %q, %v; want %q", got, err, want)
	}
	// through the `current` link and ~/.local/bin, the same
	cur := filepath.Join(home, ".local", "share", "moox", "current")
	if err := os.Symlink("0.1.0", cur); err != nil {
		t.Fatal(err)
	}
	got, err = mooxroot.Stable(filepath.Join(cur, "bin", "moox"), "")
	if err != nil || got != want {
		t.Fatalf("Stable(current) = %q, %v", got, err)
	}
}

// TestStable_CurlCustomPrefix (G1-H7): `install.sh --prefix /opt/tools`
// puts versions under /opt/tools/share/moox/<v>; the stable root is that
// prefix's `current` link — reached also through the <prefix>/bin symlink —
// never the versioned directory the next upgrade prunes.
func TestStable_CurlCustomPrefix(t *testing.T) {
	prefix := filepath.Join(realTemp(t), "opt", "tools")
	ver := filepath.Join(prefix, "share", "moox", "0.2.0")
	exe := filepath.Join(ver, "bin", "moox")
	mkfile(t, exe)
	mkfile(t, filepath.Join(ver, "share", "moox", "moox.conf"))
	cur := filepath.Join(prefix, "share", "moox", "current")
	if err := os.Symlink("0.2.0", cur); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(prefix, "bin", "moox")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cur, "bin", "moox"), link); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cur, "share", "moox")
	for _, p := range []string{exe, link} {
		got, err := mooxroot.Stable(p, "")
		if err != nil || got != want {
			t.Fatalf("Stable(%s) = %q, %v; want %q", p, got, err, want)
		}
	}
	// a plain <d>/share/moox/<x>/bin that is no installer layout is left to
	// the generic rule (not mapped to a `current` that does not exist)
	other := filepath.Join(realTemp(t), "srv", "share", "moox", "build", "bin", "moox")
	mkfile(t, other)
	if got, err := mooxroot.Stable(other, ""); err == nil {
		t.Fatalf("Stable(%s) = %q for a non-installer tree", other, got)
	}
}

func TestStable_DevTreeAndGeneric(t *testing.T) {
	repo := realTemp(t)
	mkfile(t, filepath.Join(repo, "moox", "go.mod"))
	if err := os.WriteFile(filepath.Join(repo, "moox", "go.mod"), []byte("module github.com/MadAppGang/moox\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(repo, "moox", "share", "moox", "moox.conf")
	mkfile(t, conf)
	exe := filepath.Join(repo, "moox", "bin", "moox")
	mkfile(t, exe)
	want := filepath.Dir(conf)
	if got, err := mooxroot.Stable(exe, ""); err != nil || got != want {
		t.Fatalf("dev bin: %q, %v", got, err)
	}
	// go run: binary in a build cache, cwd inside the module
	goRun := filepath.Join(realTemp(t), "go-build123", "b001", "exe", "moox")
	mkfile(t, goRun)
	if got, err := mooxroot.Stable(goRun, filepath.Join(repo, "moox", "cmd", "moox")); err != nil || got != want {
		t.Fatalf("go run: %q, %v", got, err)
	}
	// nothing to find
	if _, err := mooxroot.Stable(goRun, realTemp(t)); !errors.Is(err, mooxroot.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if s, err := mooxroot.DevShare(filepath.Join(repo, "moox", "internal")); err != nil || s != want {
		t.Fatalf("DevShare = %q, %v", s, err)
	}
}

func TestVersionString(t *testing.T) {
	old := mooxroot.Version
	t.Cleanup(func() { mooxroot.Version = old })

	mooxroot.Version = ""
	if v := mooxroot.VersionString(); v != mooxroot.DevVersion || mooxroot.Stamped() {
		t.Fatalf("unstamped: %q", v)
	}
	mooxroot.Version = "v0.1.0"
	if v := mooxroot.VersionString(); v != "0.1.0" || !mooxroot.Stamped() {
		t.Fatalf("stamped: %q", v)
	}
}

func TestDevShare_ThisTree(t *testing.T) {
	s, err := mooxroot.DevShare(".")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(s) != "moox" || filepath.Base(filepath.Dir(s)) != "share" {
		t.Fatalf("DevShare(.) = %q", s)
	}
}
