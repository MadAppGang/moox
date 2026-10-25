// Package jobsfs reads Claude Code's background job tree at ~/.claude/jobs/,
// one state.json per background agent, and returns it in the core's vocabulary.
//
// # Read-only, structurally
//
// Like registryfs, this adapter holds an fs.FS rather than a directory path.
// An fs.FS has no write method, so "this adapter never writes to ~/.claude/" is
// a property of the type rather than a rule a reviewer has to check.
//
// # DEPTH 1, and it is a correctness rule rather than an optimisation
//
// The only path this adapter ever opens is exactly `<dir>/state.json` for a
// `<dir>` that is a DIRECT directory entry of the jobs root. It never walks.
// Measured (M13): the real tree holds 9 files named state.json — the 7 real
// records plus two decoys at depths 5 and 8 under `be9663df/tmp/`, one of which
// carries both a `state` and a `sessionId` key and would therefore parse into a
// plausible-looking session. A filepath.WalkDir implementation reports 9
// background agents on this machine, two of them fabricated.
//
// Three further measured properties this file is shaped by:
//
//   - `pins.json` is a FILE at the jobs root, not a job directory. 15 entries,
//     14 directories, 7 records. The three counts are all reported so any two
//     can be reconciled from the output, because conflating them is what
//     produced the wrong number in measured fact 13.
//   - 7 of the 14 directories carry NO state.json (the oldest dates from
//     4 June). That is a fact about the tree, not a parse error, and it is not
//     counted as one.
//   - `timeline.jsonl` is NEVER opened. It is an append-only log of SOME
//     events: one record's current state does not appear in its timeline at
//     all, and 2 of 7 records were rewritten days after their last entry. The
//     record is the only complete source, so the timeline has no runtime
//     reader. Structurally guaranteed here, since the only name this adapter
//     ever passes to Open is "state.json".
package jobsfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/MadAppGang/moox/internal/observer"
)

// DirName is the jobs directory relative to the Claude state root.
const DirName = "jobs"

// StateFile is the one file name this adapter ever opens, at depth 1 under a
// job directory. Exported so a test can assert what is and is not read.
const StateFile = "state.json"

// TimelineFile is named here only so the test that asserts it is never read can
// cite the same string the adapter would have to use if it read it. Nothing in
// this package references it.
const TimelineFile = "timeline.jsonl"

// Source reads the background job tree. It implements observer.JobsSource.
type Source struct {
	fsys  fs.FS
	root  string
	clock observer.Clock
	// dirGone answers whether a record's cwd is confirmed missing. It is the
	// one look this adapter takes OUTSIDE the jobs tree, a stat and never an
	// open. nil (NewFS) takes no look, so every record's CWDGone stays false.
	dirGone func(path string) bool
}

// New returns a Source reading the jobs directory at dir.
func New(dir string, clock observer.Clock) *Source {
	s := NewFS(os.DirFS(dir), dir, clock)
	s.dirGone = statGone
	return s
}

// statGone is true only for a stat that says "no such file" (or "not a
// directory" on the way there). A refused stat is no opinion, not gone.
func statGone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// NewFS returns a Source reading from fsys, with root used only to name files
// in advisories and in the source report. Tests pass an fstest.MapFS, usually
// wrapped in a counting fs.FS so that "the decoy was never opened" and "the
// timeline was never opened" can be asserted as counts rather than inferred
// from the absence of an error.
func NewFS(fsys fs.FS, root string, clock observer.Clock) *Source {
	if clock == nil {
		clock = observer.SystemClock{}
	}
	return &Source{fsys: fsys, root: root, clock: clock}
}

// Root is the directory this source reads, for the source report.
func (s *Source) Root() string { return s.root }

// Read returns every readable job record, sorted by directory name ascending.
//
// The sort is the same discipline registryfs applies for the same reason: M8's
// duplicate rule is "the FIRST in a fixed order wins", and "first" is only
// deterministic if the order is. Directory enumeration order is not.
//
// A missing jobs directory is a FACT, not an error — it is what a machine that
// has never run a background agent looks like. Records is empty, Absent is
// true, and the error is nil.
//
// A record that will not parse is counted and its path is reported, and the
// other records in the same read are unaffected. There is deliberately NO
// torn-read retry here, unlike registryfs: the registry is rewritten IN PLACE
// (so a reader can see half a write), while a job record is published by
// rename-or-replace — measured two independent ways in §0.2b, by directory
// mtimes and by inode allocation order, 7/7. A rename is atomic, so the torn
// window registryfs sleeps out does not exist on this tree. If that measurement
// is ever falsified, the retry belongs here and not in the core.
func (s *Source) Read(ctx context.Context) (observer.JobsResult, error) {
	res := observer.JobsResult{ObservedAt: s.clock.Now()}

	entries, err := fs.ReadDir(s.fsys, ".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			res.Absent = true
			return res, nil
		}
		res.Err = err
		return res, err
	}

	// entries counts EVERY direct entry, pins.json included, so the published
	// numbers reconcile: entries - dirs is the non-directory entries, and
	// dirs - records is the directories with no state.json.
	res.Entries = len(entries)

	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			// pins.json lives here. It is not a job, it is never opened, and
			// it is never enumerated as one.
			continue
		}
		dirs = append(dirs, e.Name())
	}
	sort.Strings(dirs)
	res.Dirs = len(dirs)

	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res, err
		}

		// The ONLY name this adapter ever opens. path.Join, not filepath.Join:
		// an fs.FS path is always slash-separated regardless of the host.
		name := path.Join(dir, StateFile)
		data, err := fs.ReadFile(s.fsys, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// 7 of 14 real directories look like this. Not an error.
				continue
			}
			res.ParseErrors++
			res.Failures = append(res.Failures, observer.RecordFailure{
				Path: s.pathOf(name), Bytes: len(data), Error: err.Error(),
			})
			continue
		}

		rec, err := parseRecord(dir, s.pathOf(name), data)
		if err != nil {
			res.ParseErrors++
			res.Failures = append(res.Failures, observer.RecordFailure{
				Path: s.pathOf(name), Bytes: len(data), Error: err.Error(),
			})
			continue
		}
		st, _ := observer.MapJobState(rec.State)
		if s.dirGone != nil && !st.IsTerminal() && filepath.IsAbs(rec.CWD) {
			// Only a non-terminal job's folder matters: an ended one is
			// already gone from the default view whatever its folder says.
			rec.CWDGone = s.dirGone(rec.CWD)
		}
		res.Records = append(res.Records, rec)

		if !st.IsTerminal() {
			res.NonTerminal++
		}
	}

	return res, nil
}

// pathOf builds the display path for a record. It is used for advisories and
// for the source report, never to open anything — every read goes through the
// fs.FS with the slash-separated entry name.
func (s *Source) pathOf(name string) string {
	if s.root == "" {
		return name
	}
	if strings.Contains(s.root, string(filepath.Separator)) || filepath.IsAbs(s.root) {
		return filepath.Join(s.root, filepath.FromSlash(name))
	}
	return path.Join(s.root, name)
}

var _ observer.JobsSource = (*Source)(nil)
