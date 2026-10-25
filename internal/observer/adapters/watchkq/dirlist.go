package watchkq

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
)

// OSTree is the enumeration half of the watch protocol: it answers "which
// immediate subdirectories does this directory have", "which files with this
// suffix", and "does this path exist".
//
// It lives beside the watcher because the two are one mechanism — §6.3's fixed
// point alternates between enumerating and arming, and the enumeration is what
// decides which descriptors are owed. Keeping it here also keeps `os` out of
// the core.
//
// Three properties are deliberate:
//
//   - it NEVER walks. One os.ReadDir at depth 1, exactly as M13 requires. Two
//     state.json files exist at depths 5 and 8 on the reference machine, inside
//     a worktree a job checked out under tmp/, and one of them is an
//     adversarial decoy in the fixtures. A recursive implementation would arm
//     and read both.
//   - it returns ABSOLUTE paths, because a WatchTarget is a path and rejoining
//     a name to a root in the core is one more place to get it wrong.
//   - an absent directory is (nil, nil), not an error. "Claude has never run
//     here" and "no background agent has ever run here" are facts, and §6.6
//     requires them to read as facts.
type OSTree struct{}

// ListDirs returns the immediate subdirectories of dir, sorted, as absolute
// paths. Symlinks are not followed and are not reported: a symlinked job
// directory would arm a vnode outside the tree we were asked about.
func (OSTree) ListDirs(ctx context.Context, dir string) ([]string, error) {
	entries, err := readDir(ctx, dir)
	if err != nil || entries == nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListFiles returns the regular files in dir whose name ends in suffix, sorted,
// as absolute paths.
//
// The suffix filter is what keeps the registry's <pid>.<sha256>.key siblings
// unopened — 38 of them today against 37 records. It decides which files to
// ARM, and deliberately not which directory events to ignore: a directory event
// carries no filename, so a .key write still produces a full rebuild. That is
// honest rather than cheap, and a rebuild is 43 KB.
func (OSTree) ListFiles(ctx context.Context, dir, suffix string) ([]string, error) {
	entries, err := readDir(ctx, dir)
	if err != nil || entries == nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !e.Type().IsRegular() {
			continue
		}
		if suffix != "" && !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// Exists reports whether path exists. A permission error is reported as an
// error rather than as absence: "we may not look" is not "it is not there", and
// treating it as absence would arm ~/.claude in place of a jobs tree that is
// present.
func (OSTree) Exists(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if path == "" {
		return false, nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// readDir is the one os.ReadDir call in this package, and the one place the
// absent-directory rule is applied.
func readDir(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

var _ observer.PathLister = OSTree{}
