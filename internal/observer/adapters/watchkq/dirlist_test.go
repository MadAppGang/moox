package watchkq_test

// The enumeration half of the protocol: depth-1 only, absolute paths, and an
// absent directory as a FACT rather than an error.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
)

func TestListDirsReturnsOnlyImmediateSubdirectoriesAsAbsolutePaths(t *testing.T) {
	jobs := t.TempDir()
	for _, name := range []string{"aaaa", "bbbb", "cccc"} {
		if err := os.MkdirAll(filepath.Join(jobs, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// pins.json is a FILE at the jobs root — contents [], unchanged since June
	// on the reference machine. It is named here so nobody arms it "by .json
	// analogy".
	if err := os.WriteFile(filepath.Join(jobs, "pins.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := watchkq.OSTree{}.ListDirs(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ListDirs: %v", err)
	}
	want := []string{
		filepath.Join(jobs, "aaaa"),
		filepath.Join(jobs, "bbbb"),
		filepath.Join(jobs, "cccc"),
	}
	if len(got) != len(want) {
		t.Fatalf("ListDirs = %v, want %v (pins.json is a file and must not appear)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListDirs[%d] = %q, want %q (sorted, absolute)", i, got[i], want[i])
		}
	}
}

func TestTheEnumerationNeverDescendsIntoADecoy(t *testing.T) {
	// M13's adversarial case, measured: two state.json files exist at depths 5
	// and 8 on the reference machine, inside a worktree a job checked out under
	// tmp/. The fixture decoy carries the same keys a real record does, so a
	// recursive implementation would arm it AND read it as a session.
	jobs := t.TempDir()
	job := filepath.Join(jobs, "be9663df")
	decoyDir := filepath.Join(job, "tmp", "build-wip", "nested", ".claude", ".coaching")
	if err := os.MkdirAll(decoyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(decoyDir, "state.json")
	if err := os.WriteFile(decoy, []byte(`{"state":"working","sessionId":"deadbeef"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	dirs, err := watchkq.OSTree{}.ListDirs(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ListDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != job {
		t.Fatalf("ListDirs = %v, want exactly the one depth-1 job directory", dirs)
	}
	for _, d := range dirs {
		if d == decoyDir {
			t.Fatal("the decoy directory was enumerated")
		}
	}
}

func TestListFilesFiltersOnTheSuffixSoKeyFilesAreNeverArmed(t *testing.T) {
	reg := t.TempDir()
	for _, name := range []string{"100.json", "200.json", "100.abc123.key", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(reg, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(reg, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := watchkq.OSTree{}.ListFiles(context.Background(), reg, ".json")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	want := []string{filepath.Join(reg, "100.json"), filepath.Join(reg, "200.json")}
	if len(got) != len(want) {
		t.Fatalf("ListFiles = %v, want %v (no .key, no .txt, no directory)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAnAbsentDirectoryIsAFactAndNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-ran")

	dirs, err := watchkq.OSTree{}.ListDirs(context.Background(), missing)
	if err != nil || dirs != nil {
		t.Fatalf("ListDirs(absent) = %v, %v; want nil, nil — §6.6 requires \"Claude has "+
			"never run here\" to read as a fact", dirs, err)
	}
	files, err := watchkq.OSTree{}.ListFiles(context.Background(), missing, ".json")
	if err != nil || files != nil {
		t.Fatalf("ListFiles(absent) = %v, %v; want nil, nil", files, err)
	}
	ok, err := watchkq.OSTree{}.Exists(context.Background(), missing)
	if err != nil || ok {
		t.Fatalf("Exists(absent) = %v, %v; want false, nil", ok, err)
	}
}

func TestExistsDistinguishesAPresentPath(t *testing.T) {
	dir := t.TempDir()
	ok, err := (watchkq.OSTree{}).Exists(context.Background(), dir)
	if err != nil || !ok {
		t.Fatalf("Exists(%q) = %v, %v; want true, nil", dir, ok, err)
	}
	if ok, err := (watchkq.OSTree{}).Exists(context.Background(), ""); ok || err != nil {
		t.Fatalf("Exists(\"\") = %v, %v; want false, nil", ok, err)
	}
}

func TestACancelledContextStopsTheEnumeration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (watchkq.OSTree{}).ListDirs(ctx, t.TempDir()); err == nil {
		t.Error("ListDirs ignored a cancelled context")
	}
}
