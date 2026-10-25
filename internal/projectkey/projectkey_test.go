package projectkey

import (
	"os"
	"path/filepath"
	"testing"
)

// The layouts below are built by hand, file by file, rather than with `git`:
// the resolver never runs git, so neither do its tests, and each fixture says
// exactly which pointer files the answer rests on.

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// root is a canonical temp dir: on macOS t.TempDir() lives under /var, a
// symlink to /private/var, and the resolver answers in canonical paths.
func root(t *testing.T) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// linkWorktree lays out a linked worktree of the repository whose common dir
// is common, checked out at dir, the way `git worktree add` does.
func linkWorktree(t *testing.T, common, dir, name string) {
	t.Helper()
	private := filepath.Join(common, "worktrees", name)
	write(t, filepath.Join(private, "commondir"), "../..\n")
	write(t, filepath.Join(private, "gitdir"), filepath.Join(dir, ".git")+"\n")
	write(t, filepath.Join(dir, ".git"), "gitdir: "+private+"\n")
}

func TestAWorktreeBelongsToTheRepositoryItWasCheckedOutFrom(t *testing.T) {
	r := root(t)
	repo := filepath.Join(r, "authflow")
	mkdir(t, filepath.Join(repo, ".git"))
	mkdir(t, filepath.Join(repo, "src"))
	wt := filepath.Join(repo, ".claude", "worktrees", "2fa")
	linkWorktree(t, filepath.Join(repo, ".git"), wt, "2fa")

	res := New("")
	main, sub, linked := res.Resolve(repo), res.Resolve(filepath.Join(repo, "src")), res.Resolve(wt)

	want := filepath.Join(repo, ".git")
	for name, p := range map[string]Project{"main": main, "subdir": sub, "worktree": linked} {
		if p.Key != want || p.Label != "authflow" || !p.Git {
			t.Errorf("%s: %+v, want key %s labelled authflow", name, p, want)
		}
	}
	if main.Worktree != "" || linked.Worktree != "2fa" {
		t.Errorf("worktree names = %q (main), %q (linked); want \"\" and 2fa", main.Worktree, linked.Worktree)
	}
}

func TestTheOtherRepositoryLayoutsAreNamedByTheirCheckout(t *testing.T) {
	r := root(t)

	// .bare: the bare repository and its worktrees sit side by side.
	bareHome := filepath.Join(r, "acmeclinic")
	mkdir(t, filepath.Join(bareHome, ".bare"))
	write(t, filepath.Join(bareHome, ".git"), "gitdir: ./.bare\n")
	linkWorktree(t, filepath.Join(bareHome, ".bare"), filepath.Join(bareHome, "main"), "main")

	// repo.git: a bare repository with a worktree elsewhere.
	bareRepo := filepath.Join(r, "claudish.git")
	mkdir(t, bareRepo)
	linkWorktree(t, bareRepo, filepath.Join(r, "claudish-wt"), "claudish-wt")

	// A submodule is its own project, however deep in another it sits.
	super := filepath.Join(r, "super")
	mkdir(t, filepath.Join(super, ".git", "modules", "vendorlib"))
	write(t, filepath.Join(super, "vendorlib", ".git"), "gitdir: ../.git/modules/vendorlib\n")

	res := New("")
	cases := []struct {
		cwd, label, key string
	}{
		{filepath.Join(bareHome, "main"), "acmeclinic", filepath.Join(bareHome, ".bare")},
		{filepath.Join(r, "claudish-wt"), "claudish", bareRepo},
		{filepath.Join(super, "vendorlib"), "vendorlib", filepath.Join(super, ".git", "modules", "vendorlib")},
		{super, "super", filepath.Join(super, ".git")},
	}
	for _, c := range cases {
		if p := res.Resolve(c.cwd); p.Label != c.label || p.Key != c.key {
			t.Errorf("%s: label %q key %q, want %q %q", c.cwd, p.Label, p.Key, c.label, c.key)
		}
	}
}

func TestADeletedWorktreeStillBelongsToItsRepository(t *testing.T) {
	r := root(t)
	repo := filepath.Join(r, "madbench")
	mkdir(t, filepath.Join(repo, ".git"))
	mkdir(t, filepath.Join(repo, ".claude", "worktrees"))

	p := New("").Resolve(filepath.Join(repo, ".claude", "worktrees", "gone"))
	if p.Label != "madbench" || !p.Git {
		t.Errorf("%+v, want the repository the worktree was in", p)
	}
}

func TestADirectoryOutsideAnyRepositoryIsItsOwnProject(t *testing.T) {
	r := root(t)
	plain := filepath.Join(r, "notes", "scratch")
	mkdir(t, plain)
	res := New(filepath.Join(r, "notes"))

	if p := res.Resolve(plain); p.Key != plain || p.Label != "scratch" || p.Git {
		t.Errorf("existing: %+v", p)
	}
	// Deleted and never in a repository: its own path, not the nearest
	// ancestor's — that would name it after something like $TMPDIR.
	gone := filepath.Join(r, "tmp", "madbench-workspace-1")
	if p := res.Resolve(gone); p.Key != gone || p.Label != "madbench-workspace-1" {
		t.Errorf("deleted: %+v", p)
	}
	if p := res.Resolve(filepath.Join(r, "notes")); p.Label != "~" {
		t.Errorf("home: label %q, want ~", p.Label)
	}
	if p := res.Resolve(""); p != (Project{}) {
		t.Errorf("empty cwd: %+v, want the zero project", p)
	}
}

func TestAGitFileThatIsNotTheWorktreesOwnClaimsNothing(t *testing.T) {
	r := root(t)
	repo := filepath.Join(r, "magus")
	mkdir(t, filepath.Join(repo, ".git"))
	linkWorktree(t, filepath.Join(repo, ".git"), filepath.Join(r, "wt"), "wt")
	// The same pointer, copied somewhere else: the worktree's back-pointer
	// names the original, so the copy is not a checkout of magus.
	copied := filepath.Join(r, "copy")
	data, err := os.ReadFile(filepath.Join(r, "wt", ".git"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(copied, ".git"), string(data))

	if p := New("").Resolve(copied); p.Label != "copy" || p.Key == filepath.Join(repo, ".git") {
		t.Errorf("%+v, want the copy to stand alone", p)
	}

	// And a symlinked .git is not followed at all.
	linked := filepath.Join(r, "linked")
	mkdir(t, linked)
	if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	if p := New("").Resolve(linked); p.Git {
		t.Errorf("%+v: a symlinked .git was followed", p)
	}
}

func TestARememberedAnswerIsDroppedWhenItsRepositoryGoes(t *testing.T) {
	r := root(t)
	repo := filepath.Join(r, "mnemex")
	mkdir(t, filepath.Join(repo, ".git"))
	res := New("")
	if p := res.Resolve(repo); !p.Git {
		t.Fatalf("%+v, want a repository", p)
	}
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	if p := res.Resolve(repo); p.Git {
		t.Errorf("%+v: the cached answer outlived the repository", p)
	}
}

func TestLabelsTellSameNamedProjectsApartAndLeaveTheRestShort(t *testing.T) {
	projects := []Project{
		{Key: "/a/mag/api/.git", Label: "api", Root: "/a/mag/api"},
		{Key: "/a/work/api/.git", Label: "api", Root: "/a/work/api"},
		{Key: "/a/mag/api/.git", Label: "api", Root: "/a/mag/api"}, // the same project twice
		{Key: "/a/dotfiles/.git", Label: "dotfiles", Root: "/a/dotfiles"},
		{Key: "/x/deep/same/name", Label: "name", Root: "/x/deep/same/name"},
		{Key: "/y/deep/same/name", Label: "name", Root: "/y/deep/same/name"},
		{},
	}
	got := Labels(projects)
	want := map[string]string{
		"/a/mag/api/.git":   "mag/api",
		"/a/work/api/.git":  "work/api",
		"/a/dotfiles/.git":  "dotfiles",
		"/x/deep/same/name": "x/deep/same/name",
		"/y/deep/same/name": "y/deep/same/name",
	}
	if len(got) != len(want) {
		t.Errorf("got %d labels, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}
