package mooxroot_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/mooxroot"
)

// TestRpaster_LibexecBeforePath (V10): moox's own rpaster lives in
// <keg>/libexec beside share/, never in bin/ (the separate
// madappgang/tap/rpaster formula owns bin/rpaster). It is found from the share
// root first; a PATH rpaster is only the fallback.
func TestRpaster_LibexecBeforePath(t *testing.T) {
	d := realTemp(t)
	share := filepath.Join(d, "opt", "moox", "share", "moox")
	mkfile(t, filepath.Join(share, "moox.conf"))
	onPath := filepath.Join(d, "bin", "rpaster")
	mkfile(t, onPath)
	look := func(name string) (string, error) {
		if name == "rpaster" {
			return onPath, nil
		}
		return "", exec.ErrNotFound
	}

	if got := mooxroot.LibexecDir(share); got != filepath.Join(d, "opt", "moox", "libexec") {
		t.Fatalf("LibexecDir = %s", got)
	}

	// No bundled copy: the PATH one, not bundled.
	p, bundled, err := mooxroot.Rpaster(share, look)
	if err != nil || p != onPath || bundled {
		t.Fatalf("no libexec copy: got %q bundled=%v err=%v, want the PATH one", p, bundled, err)
	}

	// A bundled copy that is not executable is not used.
	lib := filepath.Join(d, "opt", "moox", "libexec", "rpaster")
	mkfile(t, lib)
	if p, _, _ := mooxroot.Rpaster(share, look); p != onPath {
		t.Fatalf("a non-executable libexec/rpaster was chosen: %q", p)
	}

	// Executable bundled copy: it wins over PATH.
	if err := os.Chmod(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	p, bundled, err = mooxroot.Rpaster(share, look)
	if err != nil || p != lib || !bundled {
		t.Fatalf("bundled: got %q bundled=%v err=%v, want %s", p, bundled, err, lib)
	}

	// Neither: an error naming where it looked.
	none := func(string) (string, error) { return "", exec.ErrNotFound }
	if _, _, err := mooxroot.Rpaster(filepath.Join(d, "x", "share", "moox"), none); err == nil {
		t.Fatal("no rpaster anywhere, no error")
	}
	if p, bundled, err := mooxroot.Rpaster("", look); err != nil || p != onPath || bundled {
		t.Fatalf("no share root: got %q bundled=%v err=%v", p, bundled, err)
	}
}

// TestResolve_Libexec: Root.Libexec is the brew keg's libexec through opt/.
func TestResolve_Libexec(t *testing.T) {
	d := realTemp(t)
	exe := filepath.Join(d, "Cellar", "moox", "0.1.1", "bin", "moox")
	mkfile(t, exe)
	r, err := mooxroot.Resolve(exe, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(d, "opt", "moox", "libexec"); r.Libexec != want {
		t.Fatalf("Libexec = %s, want %s", r.Libexec, want)
	}
}
