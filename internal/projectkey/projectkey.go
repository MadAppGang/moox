// Package projectkey answers "which project is this working directory in" for
// a list that groups sessions by project.
//
// A project is a git repository, and every checkout of it — the main one and
// each linked worktree — belongs to the same project. That is the rule herdr,
// Claude Code and Codex all ship, and it is the one that matters on this
// machine: 31 of 45 live sessions (69%) run in a linked worktree, mostly under
// <repo>/.claude/worktrees/<name>, so grouping by the raw directory makes 39
// groups of which 35 hold one session, and grouping by repository makes 20.
//
// The repository is identified by its COMMON DIR — the one git directory every
// worktree of a repository shares — found by reading files, never by running
// git: a linked worktree's `.git` is a file naming its private git dir, and
// that directory's `commondir` file names the shared one. It costs about 0.1 ms
// per directory against 11 ms per `git rev-parse`, and it agreed with git on
// all 38 live directories measured.
package projectkey

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Project is where one working directory belongs.
type Project struct {
	// Key identifies the project: the canonical common dir of a repository,
	// or the cleaned directory itself outside one. Two directories are in the
	// same project exactly when their keys are equal. "" for an empty cwd.
	Key string
	// Label is the short name to show: the main checkout's directory name.
	// It is not unique on its own; Labels disambiguates a set.
	Label string
	// Root is the directory the label names — the main checkout, or the
	// directory itself outside a repository. Labels uses its parents to tell
	// two same-named projects apart.
	Root string
	// Worktree is the linked worktree's directory name when the cwd is in one
	// that is not the main checkout, and "" otherwise.
	Worktree string
	// Git is whether a repository was found at all.
	Git bool
}

// Resolver resolves directories and remembers the answers. The zero value is
// not usable; build one with New.
//
// Not safe for concurrent use: it belongs to one panel, which resolves from
// its update loop.
type Resolver struct {
	home  string
	cache map[string]cached
}

type cached struct {
	project Project
	// dotGit is the .git entry the answer rests on, "" outside a repository.
	// A cached answer is re-checked against it, so a deleted worktree or a
	// removed repository is resolved afresh rather than remembered.
	dotGit string
}

// New builds a resolver. home is the user's home directory, labelled "~"
// when a session runs there; "" disables that.
func New(home string) *Resolver {
	return &Resolver{home: filepath.Clean(home), cache: map[string]cached{}}
}

// Resolve answers for one working directory.
func (r *Resolver) Resolve(cwd string) Project {
	if cwd == "" {
		return Project{}
	}
	if c, ok := r.cache[cwd]; ok {
		if c.dotGit == "" {
			return c.project
		}
		if _, err := os.Lstat(c.dotGit); err == nil {
			return c.project
		}
	}
	p, dotGit := r.resolve(cwd)
	r.cache[cwd] = cached{project: p, dotGit: dotGit}
	return p
}

func (r *Resolver) resolve(cwd string) (Project, string) {
	dir := filepath.Clean(cwd)

	// A session can outlive its directory — a deleted worktree, a removed temp
	// workspace. Its nearest existing ancestor still says which repository it
	// was in, but only a repository is worth the substitution: outside one,
	// the nearest ancestor is something like $TMPDIR, and naming the session
	// after that is a wrong answer where its own path is an honest one.
	start := dir
	for !exists(start) {
		parent := filepath.Dir(start)
		if parent == start {
			break
		}
		start = parent
	}

	for d := start; ; {
		dotGit := filepath.Join(d, ".git")
		if p, ok := fromDotGit(d, dotGit); ok {
			return p, dotGit
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return Project{Key: dir, Label: r.label(dir), Root: dir}, ""
}

// fromDotGit reads the `.git` entry at toplevel, if there is one.
func fromDotGit(toplevel, dotGit string) (Project, bool) {
	fi, err := os.Lstat(dotGit)
	if err != nil {
		return Project{}, false
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		// A symlinked .git is not a checkout's own, and following it would
		// let one directory claim to be another repository.
		return Project{}, false
	case fi.IsDir():
		return fromCommonDir(toplevel, canonical(dotGit)), true
	case fi.Mode().IsRegular():
		return fromGitFile(toplevel, dotGit)
	default:
		return Project{}, false
	}
}

// fromGitFile follows a `.git` FILE: a linked worktree's, or a submodule's.
func fromGitFile(toplevel, dotGit string) (Project, bool) {
	gitdir, ok := pointer(dotGit, "gitdir:")
	if !ok {
		return Project{}, false
	}
	common := gitdir
	if c, ok := readTrimmed(filepath.Join(gitdir, "commondir")); ok {
		// A linked worktree's private git dir names the shared one. The
		// back-pointer is checked first: a worktree's gitdir records which
		// `.git` file it belongs to, and a `.git` file copied or left behind
		// somewhere else must not claim a repository it is not a checkout of.
		if back, ok := pointer(filepath.Join(gitdir, "gitdir"), ""); ok && canonical(back) != canonical(dotGit) {
			return Project{Key: canonical(toplevel), Label: filepath.Base(toplevel), Root: toplevel, Git: true}, true
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		common = filepath.Clean(c)
	}
	p := fromCommonDir(toplevel, canonical(common))
	return p, true
}

// fromCommonDir names the project a common dir belongs to.
func fromCommonDir(toplevel, common string) Project {
	p := Project{Key: common, Git: true}
	parent := filepath.Dir(common)
	switch base := filepath.Base(common); {
	case strings.Contains(common, string(filepath.Separator)+".git"+string(filepath.Separator)+"modules"+string(filepath.Separator)):
		// A submodule is a project of its own, named by its checkout.
		p.Root = toplevel
	case base == ".git", base == ".bare":
		// The ordinary layout, and the `.bare` one where the worktrees sit
		// beside the bare repository: both are named by their parent.
		p.Root = parent
	case strings.HasSuffix(base, ".git"):
		// A bare repository, repo.git, named without its suffix.
		p.Root = strings.TrimSuffix(common, ".git")
	default:
		p.Root = common
	}
	p.Label = filepath.Base(p.Root)
	if top := canonical(toplevel); top != canonical(p.Root) && !strings.Contains(common, "/.git/modules/") {
		p.Worktree = filepath.Base(top)
	}
	return p
}

func (r *Resolver) label(dir string) string {
	if r.home != "" && r.home != "." && dir == r.home {
		return "~"
	}
	return filepath.Base(dir)
}

// Labels names a set of projects so that no two DIFFERENT projects share a
// name: a clash is settled by adding parent directories to each clashing
// label until they differ ("mag/api" and "work/api"). A project that clashes
// with nobody keeps its short name. The answer is keyed by Project.Key.
func Labels(projects []Project) map[string]string {
	byLabel := map[string][]Project{}
	seen := map[string]bool{}
	for _, p := range projects {
		if p.Key == "" || seen[p.Key] {
			continue
		}
		seen[p.Key] = true
		byLabel[p.Label] = append(byLabel[p.Label], p)
	}
	out := make(map[string]string, len(seen))
	for label, group := range byLabel {
		if len(group) == 1 {
			out[group[0].Key] = label
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Key < group[j].Key })
		for depth := 2; ; depth++ {
			names := make(map[string]int, len(group))
			for _, p := range group {
				names[tail(p.Root, depth)]++
			}
			unique, exhausted := true, true
			for _, p := range group {
				if names[tail(p.Root, depth)] > 1 {
					unique = false
				}
				if tail(p.Root, depth) != tail(p.Root, depth+1) {
					exhausted = false
				}
			}
			if unique || exhausted {
				for _, p := range group {
					out[p.Key] = tail(p.Root, depth)
				}
				break
			}
		}
	}
	return out
}

// tail is the last n components of a path, joined with "/".
func tail(path string, n int) string {
	parts := strings.Split(strings.Trim(filepath.ToSlash(path), "/"), "/")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "/")
}

// pointer reads a one-line pointer file: "gitdir: <path>" in a .git file, or
// a bare path in a worktree's gitdir file. A relative path is relative to the
// file's own directory.
func pointer(file, prefix string) (string, bool) {
	s, ok := readTrimmed(file)
	if !ok {
		return "", false
	}
	if prefix != "" {
		if !strings.HasPrefix(s, prefix) {
			return "", false
		}
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	if s == "" {
		return "", false
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(filepath.Dir(file), s)
	}
	return filepath.Clean(s), true
}

// readTrimmed reads a small pointer file. Anything larger than a path is not
// one, and is not read into memory to find that out.
func readTrimmed(file string) (string, bool) {
	fi, err := os.Stat(file)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return "", false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// canonical resolves symlinks where it can, so /tmp and /private/tmp — or a
// repository reached through a symlinked parent — are one key.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
