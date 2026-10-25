package statedir_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/statedir"
)

// shortTemp keeps socket paths under sockaddr_un's 104 bytes.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ttsd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestOpenCreatesMissingComponentsPrivately(t *testing.T) {
	root := shortTemp(t)
	path := filepath.Join(root, "a", "b", "tt")
	d, err := statedir.Open(path, noClaude(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("created %v, want no group/other bits", perm)
	}
}

func TestOpenExistingDoesNotCreate(t *testing.T) {
	path := filepath.Join(shortTemp(t), "absent")
	if _, err := statedir.OpenExisting(path, noClaude(t)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenExisting = %v, want ErrNotExist", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenExisting created %s", path)
	}
}

func TestContainmentIsCheckedOnTheResolvedPath(t *testing.T) {
	claude := shortTemp(t)
	alias := filepath.Join(shortTemp(t), "alias")
	if err := os.Symlink(claude, alias); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(claude, "x"),            // by spelling
		filepath.Join(alias, "x"),             // through an ancestor
		filepath.Join(claude, "y", "..", "x"), // through ..
		claude,                                // the root itself
	} {
		if _, err := statedir.Open(p, claude); !errors.Is(err, statedir.ErrInsideClaudeTree) {
			t.Errorf("Open(%s) = %v, want ErrInsideClaudeTree", p, err)
		}
	}
	// And the root spelled through the alias is still the root.
	if _, err := statedir.Open(filepath.Join(claude, "z"), alias); !errors.Is(err, statedir.ErrInsideClaudeTree) {
		t.Errorf("a root given through a symlink was not resolved: %v", err)
	}
	if entries, _ := os.ReadDir(claude); len(entries) != 0 {
		t.Fatalf("the refused tree gained %d entries", len(entries))
	}
}

func TestWriteFileRefusesASymlinkAtTheNameAndNeverWritesThroughIt(t *testing.T) {
	d, err := statedir.Open(shortTemp(t), noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	target := filepath.Join(shortTemp(t), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, d.Join("f")); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile("f", []byte("x"), 0o600); !errors.Is(err, statedir.ErrNotRegular) {
		t.Fatalf("WriteFile over a symlink = %v, want ErrNotRegular", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Fatalf("the target changed: %q", got)
	}
	if _, _, err := d.ReadFile("f"); !errors.Is(err, statedir.ErrSymlink) {
		t.Fatalf("ReadFile through a symlink = %v, want ErrSymlink", err)
	}
}

func TestWriteFileIsAtomicAndLeavesNoTempFile(t *testing.T) {
	dir := shortTemp(t)
	d, err := statedir.Open(dir, noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, body := range []string{"one", "two"} {
		if err := d.WriteFile("f", []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, info, err := d.ReadFile("f")
		if err != nil || string(got) != body || info.Type != statedir.TypeRegular {
			t.Fatalf("ReadFile = %q %v %v", got, info.Type, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only f", len(entries))
	}
}

func TestRemoveChecksTypeAndIdentity(t *testing.T) {
	d, err := statedir.Open(shortTemp(t), noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.WriteFile("f", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("f", statedir.TypeSocket, nil); !errors.Is(err, statedir.ErrWrongType) {
		t.Fatalf("removing a regular file as a socket = %v, want ErrWrongType", err)
	}
	old, _ := d.Lstat("f")
	if err := d.WriteFile("f", []byte("y"), 0o600); err != nil { // a new inode
		t.Fatal(err)
	}
	if err := d.Remove("f", statedir.TypeRegular, &old); !errors.Is(err, statedir.ErrWrongType) {
		t.Fatalf("removing a replaced file by its old identity = %v, want ErrWrongType", err)
	}
	cur, _ := d.Lstat("f")
	if err := d.Remove("f", statedir.TypeRegular, &cur); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if err := d.Remove(bad, statedir.TypeRegular, nil); !errors.Is(err, statedir.ErrBadName) {
			t.Errorf("Remove(%q) = %v, want ErrBadName", bad, err)
		}
	}
}

func TestListenUnixBindsInTheVerifiedDirectoryAndDoesNotUnlinkOnClose(t *testing.T) {
	d, err := statedir.Open(shortTemp(t), noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ln, info, err := d.ListenUnix("s.sock")
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	if info.Type != statedir.TypeSocket {
		t.Fatalf("bound a %v", info.Type)
	}
	_ = ln.Close()
	if _, err := d.Lstat("s.sock"); err != nil {
		t.Fatalf("closing unlinked the socket by path: %v", err)
	}
	if err := d.Remove("s.sock", statedir.TypeSocket, &info); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestCreateExclusiveRefusesAnExistingNameIncludingASymlink(t *testing.T) {
	d, err := statedir.Open(shortTemp(t), noClaude(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := os.Symlink("/nonexistent-target", d.Join("lock")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.CreateExclusive("lock", 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CreateExclusive over a dangling symlink = %v, want ErrExist", err)
	}
}
