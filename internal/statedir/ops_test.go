package statedir_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"

	"github.com/MadAppGang/moox/internal/statedir"
)

// noClaude is a Claude tree that exists nowhere near the state directory: a
// sibling temp dir, so the refusal is in force and never fires.
func noClaude(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortTemp(t), ".claude")
}

func openTemp(t *testing.T) (*statedir.Dir, string) {
	t.Helper()
	p := shortTemp(t)
	d, err := statedir.Open(p, noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return d, real
}

func TestOpen_EmptyClaudeRootIsRefused(t *testing.T) {
	p := shortTemp(t)
	if _, err := statedir.Open(p, ""); !errors.Is(err, statedir.ErrNoClaudeRoot) {
		t.Fatalf("Open with no Claude root: %v", err)
	}
	if _, err := statedir.OpenExisting(p, "  "); !errors.Is(err, statedir.ErrNoClaudeRoot) {
		t.Fatalf("OpenExisting with no Claude root: %v", err)
	}
}

func TestOpenUserConfig_SkipsOnlyTheClaudeRefusal(t *testing.T) {
	root := shortTemp(t)
	claude := filepath.Join(root, ".claude")
	if _, err := statedir.Open(claude, claude); !errors.Is(err, statedir.ErrInsideClaudeTree) {
		t.Fatalf("precondition: Open inside the Claude tree: %v", err)
	}
	d, err := statedir.OpenUserConfig(claude)
	if err != nil {
		t.Fatalf("OpenUserConfig: %v", err)
	}
	defer d.Close()
	// Everything else still holds: a symlink at a file name is refused.
	if err := os.Symlink("/etc/hosts", filepath.Join(claude, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile("settings.json", []byte("{}"), 0o600); !errors.Is(err, statedir.ErrNotRegular) {
		t.Fatalf("OpenUserConfig wrote through a symlink: %v", err)
	}
}

func TestStatedir_ModesOwnerOnly(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	d, real := openTemp(t)
	if err := d.WriteFile("a", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFileSync("b", []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	fd, _, err := d.CreateExclusive("c", 0o777)
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Close(fd)
	f, err := d.OpenAppend("d")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	l, err := d.TryLock("e.lock", nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	sub, err := d.OpenSub("sub")
	if err != nil {
		t.Fatal(err)
	}
	_ = sub.Close()
	for _, n := range []string{"a", "b", "c", "d", "e.lock", "sub"} {
		st, err := os.Lstat(filepath.Join(real, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v, want owner-only", n, st.Mode().Perm())
		}
	}

	// TightenToOwner narrows a loose directory and reports it.
	loose := filepath.Join(real, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	ld, err := statedir.OpenExisting(loose, noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ld.Close()
	changed, err := ld.TightenToOwner()
	if err != nil || !changed {
		t.Fatalf("TightenToOwner = %v, %v", changed, err)
	}
	if m, _ := ld.Mode(); m != 0o700 {
		t.Fatalf("mode after tighten = %o", m)
	}
	if changed, _ := ld.TightenToOwner(); changed {
		t.Fatal("second tighten reported a change")
	}
}

func TestWriteFileSync_Publishes(t *testing.T) {
	d, real := openTemp(t)
	if err := d.WriteFileSync("moox-last", []byte("moox-1.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFileSync("moox-last", []byte("moox-2.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(real, "moox-last"))
	if err != nil || string(got) != "moox-2.json\n" {
		t.Fatalf("content %q, %v", got, err)
	}
	names, _ := d.ReadDirNames()
	if len(names) != 1 {
		t.Fatalf("temp files left behind: %v", names)
	}
	// A symlink at the name is refused, not replaced.
	if err := os.Symlink("/etc/hosts", filepath.Join(real, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFileSync("evil", []byte("x"), 0o600); !errors.Is(err, statedir.ErrNotRegular) {
		t.Fatalf("WriteFileSync over a symlink: %v", err)
	}
}

func TestReadDirNames_RepeatableAndComplete(t *testing.T) {
	d, _ := openTemp(t)
	for _, n := range []string{"3", "1", "2"} {
		if err := d.WriteFile(n, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		names, err := d.ReadDirNames()
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		if len(names) != 3 || names[0] != "1" || names[2] != "3" {
			t.Fatalf("pass %d: %v", i, names)
		}
	}
}

func TestOpenAppend_AppendsAndRefusesLinks(t *testing.T) {
	d, real := openTemp(t)
	for _, s := range []string{"a\n", "b\n"} {
		f, err := d.OpenAppend("log")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	got, _ := os.ReadFile(filepath.Join(real, "log"))
	if string(got) != "a\nb\n" {
		t.Fatalf("log = %q", got)
	}
	target := filepath.Join(real, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(real, "lnk")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OpenAppend("lnk"); !errors.Is(err, statedir.ErrSymlink) {
		t.Fatalf("OpenAppend through a symlink: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("symlink target was written")
	}
}

func TestRenameAt_TwoHandlesNoReplaceMovesTheLink(t *testing.T) {
	src, srcReal := openTemp(t)
	dst, dstReal := openTemp(t)

	// A stowed-style symlink moves as a link; its target is untouched.
	target := filepath.Join(srcReal, "real.conf")
	if err := os.WriteFile(target, []byte("conf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(srcReal, ".tmux.conf")); err != nil {
		t.Fatal(err)
	}
	if err := statedir.RenameAt(src, ".tmux.conf", dst, "tmux.conf.bak"); err != nil {
		t.Fatalf("RenameAt: %v", err)
	}
	moved, err := os.Readlink(filepath.Join(dstReal, "tmux.conf.bak"))
	if err != nil || moved != target {
		t.Fatalf("moved entry is not the link: %q, %v", moved, err)
	}
	if _, err := os.Lstat(filepath.Join(srcReal, ".tmux.conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("source entry still present")
	}
	if b, _ := os.ReadFile(target); string(b) != "conf" {
		t.Fatal("link target changed")
	}

	// Never replaces.
	if err := os.WriteFile(filepath.Join(srcReal, "x"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstReal, "x"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := statedir.RenameAt(src, "x", dst, "x"); !errors.Is(err, statedir.ErrExists) {
		t.Fatalf("RenameAt over an existing name: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dstReal, "x")); string(b) != "old" {
		t.Fatal("destination was replaced")
	}
}

func TestRename_SameDirReplacesRegularOnly(t *testing.T) {
	d, real := openTemp(t)
	_ = d.WriteFile("a", []byte("A"), 0o600)
	_ = d.WriteFile("b", []byte("B"), 0o600)
	if err := d.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(real, "b")); string(b) != "A" {
		t.Fatalf("b = %q", b)
	}
	if err := os.Mkdir(filepath.Join(real, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = d.WriteFile("c", nil, 0o600)
	if err := d.Rename("c", "dir"); !errors.Is(err, statedir.ErrNotRegular) {
		t.Fatalf("Rename over a dir: %v", err)
	}
}

func TestRemoveTree_NeverFollowsLinks(t *testing.T) {
	d, real := openTemp(t)
	outside := shortTemp(t)
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(real, "tree", "a", "b")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree, "f"), []byte("f"), 0o600)
	if err := os.Symlink(outside, filepath.Join(real, "tree", "a", "out")); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveTree("tree"); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(real, "tree")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("tree still present")
	}
	if b, err := os.ReadFile(keep); err != nil || !bytes.Equal(b, []byte("k")) {
		t.Fatalf("a file behind a symlink was removed: %v", err)
	}
	if err := d.RemoveTree("tree"); err != nil {
		t.Fatalf("RemoveTree of a missing name: %v", err)
	}
}

func TestOpenSub_RefusesSymlink(t *testing.T) {
	d, real := openTemp(t)
	if err := os.Symlink(shortTemp(t), filepath.Join(real, "s")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OpenSub("s"); err == nil {
		t.Fatal("OpenSub followed a symlink")
	}
	sub, err := d.OpenSub("ok")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.WriteFile("f", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(real, "ok", "f")); err != nil {
		t.Fatal(err)
	}
}
