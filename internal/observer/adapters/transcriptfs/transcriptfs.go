// Package transcriptfs reads bounded TAILS of Claude Code's session
// transcripts under ~/.claude/projects/. It never reads a whole file: the tree
// measures 630 MB and the largest single transcript 30.7 MB, so a full scan is
// not a slow version of the right answer, it is a different program.
//
// # Two reads, kept apart on purpose
//
// §7.3 splits transcript reading into two concerns with different sizes,
// different cache keys and different trigger rules, and insists they not be
// merged:
//
//	OpenPath  + ReadTiming   64 KB   CORRECTNESS.   statusSince's cross-check
//	                                 and §3.5 rules 4-5. Runs inside a
//	                                 watch-driven rebuild, for the jobs
//	                                 matching §3.4's Invariant B. Cached on
//	                                 (sessionId, linkScanPath).
//	FindByGlob + ReadFacts  200 KB   ENRICHMENT.    colour, api errors,
//	                                 subagent counts. NEVER part of a rebuild.
//	                                 Cached on (sessionId, path, size, mtime).
//
// P3 built the first pair, P6 the second. They share the tail reader (so a
// background job's read costs exactly what an interactive one's does) and
// NOTHING ELSE: two entry points, two bounds, two caches, two callers. The
// architecture warns in §10 that whoever writes this package will be tempted
// to fold them together; the note is here because that is where the temptation
// arrives. Folding them would put a 200 KB enrichment read on the rebuild path
// — up to 37 of them per event — or shrink the correctness read to a bound
// nobody measured.
//
// # Read-only, structurally
//
// The adapter holds an fs.FS. It has no write method, so the static write
// audit has nothing to find here because there is nothing to find. The root
// path is carried separately, as a string, for one purpose only: validating
// that an absolute path taken out of an untrusted record is inside the tree
// this adapter is allowed to read.
package transcriptfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
)

// DirName is the projects directory relative to the Claude state root.
const DirName = "projects"

// The two production tail sizes. §8.3 item 9 requires both to be injectable
// and requires a test to assert these exact constants, because the numbers are
// measurements rather than preferences:
//
//   - TimingTailBytes: the newest TIMESTAMPED record sits at most 1 912 B from
//     EOF, measured over the ten most-recently-written transcripts on the
//     machine (the last line is usually an untimestamped record type, so the
//     last line is not the answer). 64 KB is 33x that worst case.
//   - FactsTailBytes: the agent-color record was found for all 35 transcripts
//     that carry one within 200 KB, 0 misses, largest file 30.7 MB.
const (
	TimingTailBytes = 64 << 10
	FactsTailBytes  = 200 << 10
)

// ErrOutsideProjectsRoot is returned by OpenPath for a path that resolves
// outside the projects root.
//
// A job record is UNTRUSTED INPUT: `linkScanPath` is an absolute path we did
// not construct, in a file we do not own, and following it wherever it points
// would make "read a bounded tail of a transcript" into "read a bounded tail of
// any file on the machine". The refusal is a distinct sentinel rather than a
// silent not-found so that a test can tell a refused path from a reaped one,
// even though the core treats both as transcript-absent.
var ErrOutsideProjectsRoot = errors.New("transcriptfs: path is outside the projects root")

// ErrInvalidSessionID is returned by FindByGlob for a session id that cannot
// safely be pasted into a glob pattern.
//
// A session id reaches us out of a registry record or a `claude agents --json`
// listing — untrusted input, by the same argument that makes OpenPath validate
// `linkScanPath`. An id containing `*`, `?`, `[` or a path separator would turn
// "find this session's transcript" into a wildcard scan that matches a
// NEIGHBOUR's file, and the answer would look perfectly well-formed. Every id
// measured anywhere on this machine is a plain UUID, so the refusal costs
// nothing real; it is a distinct sentinel rather than a silent not-found so a
// test can tell a refused id from a genuine miss.
var ErrInvalidSessionID = errors.New("transcriptfs: session id is not usable in a glob pattern")

// Source reads transcript tails. It implements observer.TranscriptSource.
type Source struct {
	fsys fs.FS
	// root is the ABSOLUTE projects directory, used only to resolve and
	// validate the absolute paths job records carry. Every actual read goes
	// through fsys with a slash-separated relative name.
	root  string
	clock observer.Clock
}

// New returns a Source reading the projects directory at dir, which must be
// absolute for OpenPath's containment check to mean anything.
func New(dir string, clock observer.Clock) *Source {
	return NewFS(os.DirFS(dir), dir, clock)
}

// NewFS returns a Source reading from fsys, with root as the absolute path
// fsys is rooted at. Tests pass an fstest.MapFS and a synthetic root; the
// "a colliding directory is never opened" and "no glob fallback is attempted"
// assertions wrap it in a counting fs.FS, because those are claims about which
// files were touched and the only way to test them is to count.
func NewFS(fsys fs.FS, root string, clock observer.Clock) *Source {
	if clock == nil {
		clock = observer.SystemClock{}
	}
	return &Source{fsys: fsys, root: root, clock: clock}
}

// Root is the directory this source reads, for the source report.
func (s *Source) Root() string { return s.root }

// OpenPath locates a background job's transcript from its record's
// linkScanPath. It is §8.3 item 10's second locator.
//
// The order of the checks is the whole of its value:
//
//  1. an empty path is "the record names no transcript" — not found, no error;
//  2. a relative path is refused. A record is untrusted input and a relative
//     path would be resolved against whatever the process's cwd happens to be;
//  3. the path is cleaned and required to be INSIDE root. Cleaning first is
//     what makes "…/projects/../../../etc/passwd" fail the containment test
//     rather than pass it;
//  4. only then is it statted, through the fs.FS, by its relative name.
//
// A directory at the path is not found rather than an error: the field is
// documented as naming a file, and a directory there is a shape we do not
// understand, which the NFR says to have no opinion about.
//
// NO GLOB FALLBACK IS EVER ATTEMPTED. A refused or absent path returns
// not-found and stops. Falling back to the glob would turn a refused path into
// a neighbouring session's transcript — the dotfiles project slug alone holds
// 31 unrelated transcripts — and publish another conversation's last turn as
// this job's last activity.
func (s *Source) OpenPath(ctx context.Context, p string) (observer.TranscriptLocation, error) {
	if err := ctx.Err(); err != nil {
		return observer.TranscriptLocation{}, err
	}
	if strings.TrimSpace(p) == "" {
		return observer.TranscriptLocation{}, nil
	}
	if !filepath.IsAbs(p) {
		return observer.TranscriptLocation{}, ErrOutsideProjectsRoot
	}

	rel, ok := s.relOf(p)
	if !ok {
		return observer.TranscriptLocation{}, ErrOutsideProjectsRoot
	}

	info, err := fs.Stat(s.fsys, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The reaped case, and it is a LIVENESS signal rather than a
			// failure: nothing in the depth-2 session-transcript population is
			// older than the retention floor, so a record older than that
			// window cannot be corroborated by one. §3.5 rule 4 reads this.
			// Path is carried so the advisory can name what was missing.
			return observer.TranscriptLocation{Path: filepath.Clean(p)}, nil
		}
		return observer.TranscriptLocation{Path: filepath.Clean(p)}, err
	}
	if info.IsDir() {
		return observer.TranscriptLocation{Path: filepath.Clean(p)}, nil
	}

	return observer.TranscriptLocation{
		Found:     true,
		Path:      filepath.Clean(p),
		SizeBytes: info.Size(),
		ModTime:   observer.TruncateTime(info.ModTime()),
	}, nil
}

// relOf converts an absolute path to the slash-separated name fsys expects,
// and reports whether it is inside root at all.
//
// filepath.Rel is what does the containment work: a result of ".." or one
// beginning "../" means the path climbs out of root, which no amount of string
// prefixing catches reliably (root "/a/projects" and path "/a/projects-evil/x"
// share a prefix and are not the same tree).
func (s *Source) relOf(p string) (string, bool) {
	clean := filepath.Clean(p)
	root := filepath.Clean(s.root)
	if root == "" || root == "." {
		return "", false
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// SessionGlobFor is the pattern FindByGlob uses, exposed as a function so a
// test can assert its shape rather than infer it from behaviour.
//
// It is `*/<sessionId>.jsonl`: NON-RECURSIVE, exactly two segments, the same
// shape as SessionTranscriptGlob. Two properties follow from that and both are
// load-bearing:
//
//   - a subagent transcript at depth 3
//     (`<slug>/<sessionId>/subagents/agent-….jsonl`) is structurally
//     unreachable, so a subagent can never be mistaken for its parent session;
//   - the pattern's only wildcard is the slug, which is precisely the thing
//     measured fact 6 says we cannot derive.
func SessionGlobFor(sessionID string) string { return "*/" + sessionID + ".jsonl" }

// FindByGlob locates an interactive session's transcript by GLOBBING for
// <sessionId>.jsonl under every project directory. It is §8.3 item 7 and
// measured fact 6.
//
// Why a glob and not the cwd-derived slug every other implementation uses:
// the derivation measured 95.86 % over 356 real directories and CANNOT be
// fixed. Underscores convert to dashes too, so `proj_a` and `proj-a` produce
// the same slug and the wrong file is a perfectly plausible transcript; and a
// worktree session sometimes collapses to the parent repo's directory. The
// glob costs a median 0.76 ms over 349 directories and is exact.
//
// Four outcomes, all distinguishable, because the core publishes a different
// thing for each:
//
//	invalid id      -> ErrInvalidSessionID. Nothing is read.
//	0 matches       -> the zero location. Not found, NOT an error: a session
//	                   that has not written a transcript yet is ordinary.
//	>1 match        -> Ambiguous with Matches populated and Found FALSE. Nothing
//	                   is opened. Guessing which of two files belongs to a
//	                   session is exactly the wrong answer, and it is the answer
//	                   that looks right.
//	1 match         -> Found, with the size and mtime the enrichment cache keys
//	                   on.
//
// fs.Glob is used rather than filepath.Glob so the whole locator runs inside
// the injected fs.FS. That is what makes AC7's strongest clause — the
// colliding slug directory is NEVER OPENED — a countable claim instead of a
// hope: with filepath.Glob there would be nothing to count.
func (s *Source) FindByGlob(ctx context.Context, sessionID string) (observer.TranscriptLocation, error) {
	if err := ctx.Err(); err != nil {
		return observer.TranscriptLocation{}, err
	}
	if !validSessionID(sessionID) {
		return observer.TranscriptLocation{}, ErrInvalidSessionID
	}

	pattern := SessionGlobFor(sessionID)
	matches, err := fs.Glob(s.fsys, pattern)
	if err != nil {
		// fs.Glob only errors on a malformed pattern, which validSessionID has
		// already excluded. Wrapped rather than swallowed so a future change to
		// the pattern cannot fail silently.
		return observer.TranscriptLocation{}, fmt.Errorf("transcriptfs: globbing %s: %w", pattern, err)
	}

	switch len(matches) {
	case 0:
		return observer.TranscriptLocation{}, nil
	case 1:
	default:
		// Sorted for determinism: fs.Glob returns lexical order already, but
		// the advisory carries these strings onto the wire and AC11 compares
		// bytes, so the order is pinned here rather than inherited.
		out := make([]string, len(matches))
		for i, m := range matches {
			out[i] = filepath.Join(s.root, filepath.FromSlash(m))
		}
		sort.Strings(out)
		return observer.TranscriptLocation{Ambiguous: true, Matches: out}, nil
	}

	rel := matches[0]
	info, err := fs.Stat(s.fsys, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Reaped between the glob and the stat. Not found, not a failure.
			return observer.TranscriptLocation{}, nil
		}
		return observer.TranscriptLocation{}, fmt.Errorf("transcriptfs: stat %s: %w", rel, err)
	}
	if info.IsDir() {
		// A DIRECTORY named "<sessionId>.jsonl". A shape we do not understand,
		// and the NFR says to have no opinion about one rather than to decide
		// it is a transcript.
		return observer.TranscriptLocation{}, nil
	}

	return observer.TranscriptLocation{
		Found:     true,
		Path:      filepath.Join(s.root, filepath.FromSlash(rel)),
		SizeBytes: info.Size(),
		ModTime:   observer.TruncateTime(info.ModTime()),
	}, nil
}

// validSessionID reports whether an id can be pasted into a glob pattern
// without changing what the pattern means.
//
// The test is deliberately a WHITELIST of characters rather than a blacklist of
// glob metacharacters: `\` is an escape inside path.Match on some platforms and
// `-` is only special inside a `[…]` class, so reasoning about which characters
// are dangerous in combination is exactly the kind of thing that is wrong once
// and then wrong forever. Every session id ever observed is a lowercase hex
// UUID, so hex digits and dashes are the whole alphabet needed.
func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

var _ observer.TranscriptSource = (*Source)(nil)
